package prune

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"

	"github.com/rta/gitops/internal/manifest"
	"github.com/rta/gitops/internal/report"
)

const OwnerLabel = "gitops.rta.io/owner"

const DefaultMaxDeletes = 10

type Applier interface {
	Apply(ctx context.Context, revision string, desired []manifest.Resource) error
}

type Pruner struct {
	Next       Applier
	Kubectl    string
	Owner      string
	Kinds      []string
	Namespaces []string
	DryRun     bool
	MaxDeletes int
	Log        *slog.Logger
}

type object struct{ Kind, Namespace, Name string }

func (o object) key() string { return o.Kind + "/" + o.Namespace + "/" + o.Name }

func (p Pruner) Apply(ctx context.Context, revision string, desired []manifest.Resource) error {
	if err := p.Next.Apply(ctx, revision, desired); err != nil {
		return err // a failed apply never prunes
	}
	return p.prune(ctx, revision, desired)
}

func (p Pruner) prune(ctx context.Context, revision string, desired []manifest.Resource) error {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}

	var managed []manifest.Resource
	want := map[string]bool{}
	for _, r := range desired {
		if r.Kind == "Namespace" {
			continue
		}
		managed = append(managed, r)
		want[object{r.Kind, r.Namespace, r.Name}.key()] = true
	}
	if len(managed) == 0 {
		log.Warn("prune skipped: revision has no namespaced resources", "revision", revision)
		return nil
	}

	if !p.DryRun {
		if _, err := p.run(ctx, join(managed), "label", "--overwrite", "-f", "-", OwnerLabel+"="+p.Owner); err != nil {
			return fmt.Errorf("%w: labelling: %w", report.ErrPrune, err)
		}
	}

	live, err := p.list(ctx)
	if err != nil {
		return fmt.Errorf("%w: listing (nothing was deleted): %w", report.ErrPrune, err)
	}
	var stale []object
	for _, o := range live {
		if !want[o.key()] {
			stale = append(stale, o)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].key() < stale[j].key() })

	limit := p.MaxDeletes
	if limit <= 0 {
		limit = DefaultMaxDeletes
	}
	if len(stale) > limit {
		return fmt.Errorf("%w: refusing to delete %d objects (limit %d): %s", report.ErrPrune,
			len(stale), limit, names(stale))
	}

	for _, o := range stale {
		if p.DryRun {
			report.MarkPending(ctx)
			log.Info("dry-run: would prune", "kind", o.Kind, "namespace", o.Namespace, "name", o.Name)
			continue
		}
		args := []string{"delete", strings.ToLower(o.Kind), o.Name, "-n", o.Namespace, "--ignore-not-found", "--wait=false"}
		if _, err := p.run(ctx, nil, args...); err != nil {
			return fmt.Errorf("%w: deleting %s: %w", report.ErrPrune, o.key(), err)
		}
		report.MarkPruned(ctx, 1)
		log.Info("pruned", "kind", o.Kind, "namespace", o.Namespace, "name", o.Name, "revision", revision)
	}
	return nil
}

func (p Pruner) list(ctx context.Context) ([]object, error) {
	if len(p.Kinds) == 0 || len(p.Namespaces) == 0 {
		return nil, errors.New("no kinds or namespaces configured")
	}
	var out []object
	for _, ns := range p.Namespaces {
		stdout, err := p.run(ctx, nil, "get", strings.Join(p.Kinds, ","), "-n", ns,
			"-l", OwnerLabel+"="+p.Owner, "-o", "json")
		if err != nil {
			return nil, err
		}
		var l struct {
			Items []struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(stdout), &l); err != nil {
			return nil, fmt.Errorf("parsing kubectl get output: %w", err)
		}
		for _, it := range l.Items {
			if it.Kind == "" || it.Metadata.Name == "" {
				return nil, errors.New("kubectl get returned an item without kind or name")
			}
			out = append(out, object{it.Kind, it.Metadata.Namespace, it.Metadata.Name})
		}
	}
	return out, nil
}

func (p Pruner) run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	bin := p.Kubectl
	if bin == "" {
		bin = "kubectl"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func join(rs []manifest.Resource) []byte {
	var b bytes.Buffer
	for _, r := range rs {
		b.WriteString("---\n")
		b.Write(r.Raw)
	}
	return b.Bytes()
}

func names(os []object) string {
	var s []string
	for _, o := range os {
		s = append(s, o.key())
	}
	return strings.Join(s, ", ")
}
