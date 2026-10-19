package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/rta/gitops/internal/manifest"
	"github.com/rta/gitops/internal/policy"
)

type Applier struct {
	Policy policy.Policy
	DryRun bool
	Log    *slog.Logger
	// Kubectl is the kubectl binary to run. Empty means "kubectl" from PATH.
	Kubectl string
}

func (a Applier) Apply(ctx context.Context, revision string, desired []manifest.Resource) error {
	log := a.Log
	if log == nil {
		log = slog.Default()
	}

	// 1. Policy first. One violation rejects the whole commit.
	if err := a.Policy.Check(desired); err != nil {
		return err
	}
	if len(desired) == 0 {
		log.Info("no resources in revision", "revision", revision)
		return nil
	}

	// 2. Namespaces go first. A server-side diff of a resource whose namespace
	// doesn't exist yet fails with NotFound, so the rest must wait for them.
	nss, rest := splitNamespaces(desired)
	if err := a.diffApply(ctx, log, revision, nss); err != nil {
		return err
	}

	// 3. In dry-run the namespaces were not created, so resources in brand-new
	// ones can't be diffed yet. Skip them instead of failing the whole commit.
	if a.DryRun {
		var err error
		if rest, err = a.skipPending(ctx, log, nss, rest); err != nil {
			return err
		}
	}
	return a.diffApply(ctx, log, revision, rest)
}

// diffApply diffs rs against the cluster and applies them unless in dry-run.
func (a Applier) diffApply(ctx context.Context, log *slog.Logger, revision string, rs []manifest.Resource) error {
	if len(rs) == 0 {
		return nil
	}
	stream := join(rs)

	// kubectl diff: exit 0 = same, 1 = differences, >1 = real error.
	out, code, err := a.kubectl(ctx, stream, "diff", "--server-side", "--force-conflicts", "--field-manager=gitops", "-f", "-")
	if err != nil && code != 1 {
		return err
	}
	if code != 1 {
		log.Info("cluster already matches git", "revision", revision, "resources", len(rs))
		return nil
	}
	log.Info("cluster differs from git", "revision", revision, "diff", out)

	if a.DryRun {
		log.Info("dry-run: not applying", "revision", revision)
		return nil
	}

	if _, _, err := a.kubectl(ctx, stream, "apply", "--server-side", "--force-conflicts", "--field-manager=gitops", "-f", "-"); err != nil {
		return err
	}
	log.Info("applied", "revision", revision, "resources", len(rs))
	return nil
}

func splitNamespaces(rs []manifest.Resource) (nss, rest []manifest.Resource) {
	for _, r := range rs {
		if r.Kind == "Namespace" {
			nss = append(nss, r)
		} else {
			rest = append(rest, r)
		}
	}
	return nss, rest
}

// skipPending drops resources that live in a namespace declared in git but not
// yet present in the cluster.
func (a Applier) skipPending(ctx context.Context, log *slog.Logger, nss, rest []manifest.Resource) ([]manifest.Resource, error) {
	if len(nss) == 0 {
		return rest, nil
	}
	args := []string{"get", "namespace"}
	for _, ns := range nss {
		args = append(args, ns.Name)
	}
	args = append(args, "--ignore-not-found", "-o", "name")
	out, _, err := a.kubectl(ctx, nil, args...)
	if err != nil {
		return nil, err
	}

	existing := map[string]bool{}
	for _, line := range strings.Fields(out) {
		existing[strings.TrimPrefix(line, "namespace/")] = true
	}
	pending := map[string]bool{}
	for _, ns := range nss {
		if !existing[ns.Name] {
			pending[ns.Name] = true
		}
	}

	var keep []manifest.Resource
	for _, r := range rest {
		if pending[r.Namespace] {
			log.Info("dry-run: will be created along with its namespace",
				"kind", r.Kind, "namespace", r.Namespace, "name", r.Name)
			continue
		}
		keep = append(keep, r)
	}
	return keep, nil
}

func join(rs []manifest.Resource) []byte {
	var b bytes.Buffer
	for _, namespacesPass := range []bool{true, false} {
		for _, r := range rs {
			if (r.Kind == "Namespace") == namespacesPass {
				b.WriteString("---\n")
				b.Write(r.Raw)
			}
		}
	}
	return b.Bytes()
}

// kubectl returns stdout, the exit code, and an error for any non-zero exit.
// Callers check the code first, because diff's exit 1 is normal.
func (a Applier) kubectl(ctx context.Context, stdin []byte, args ...string) (string, int, error) {
	bin := a.Kubectl
	if bin == "" {
		bin = "kubectl"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) { // errors.As asks "is this error an ExitError?"
		return stdout.String(), exitErr.ExitCode(),
			fmt.Errorf("kubectl %s: exit %d: %s", args[0], exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return "", -1, fmt.Errorf("kubectl %s: %w", args[0], err)
	}
	return stdout.String(), 0, nil
}
