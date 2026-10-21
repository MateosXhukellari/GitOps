package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rta/gitops/internal/manifest"
	"github.com/rta/gitops/internal/policy"
	"github.com/rta/gitops/internal/report"
)

// fakeKubectl is a shell script standing in for kubectl. It logs each call's
// args, saves stdin as stdin-<call number>, and exits with FAKE_<SUBCOMMAND>_EXIT.
// `get namespace` prints the namespaces listed in FAKE_EXISTING_NS.
const fakeKubectl = `#!/bin/sh
echo "$*" >> "$FAKE_DIR/calls"
n=$(wc -l < "$FAKE_DIR/calls" | tr -d ' ')
cat > "$FAKE_DIR/stdin-$n"
case "$1" in
  diff)  echo "-  old"; echo "+  new"; exit ${FAKE_DIFF_EXIT:-0} ;;
  apply) echo "applied"; echo "apply boom" >&2; exit ${FAKE_APPLY_EXIT:-0} ;;
  get)   for ns in $FAKE_EXISTING_NS; do echo "namespace/$ns"; done
         echo "get boom" >&2; exit ${FAKE_GET_EXIT:-0} ;;
esac
`

// setupKubectl puts the fake kubectl first on PATH and returns its working dir.
func setupKubectl(t *testing.T, diffExit, applyExit string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(fakeKubectl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", dir)
	t.Setenv("FAKE_DIFF_EXIT", diffExit)
	t.Setenv("FAKE_APPLY_EXIT", applyExit)
	t.Setenv("FAKE_GET_EXIT", "0")
	t.Setenv("FAKE_EXISTING_NS", "")
	return dir
}

// stdinOf returns what the n-th kubectl call (1-based) received on stdin.
func stdinOf(t *testing.T, dir string, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "stdin-"+strconv.Itoa(n)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func calls(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func resources(t *testing.T) []manifest.Resource {
	t.Helper()
	rs, err := manifest.Parse([]byte(`apiVersion: v1
kind: Namespace
metadata:
  name: demo
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: demo
`))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func applier(dryRun bool) Applier {
	return Applier{
		Policy: policy.New([]string{"demo"}),
		DryRun: dryRun,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestApply(t *testing.T) {
	// resources(t) is the demo Namespace plus a ConfigMap inside it.
	tests := []struct {
		name      string
		dryRun    bool
		diffExit  string
		applyExit string
		existing  string // namespaces the fake cluster already has
		wantErr   string
		wantCalls []string // subcommands, in order
	}{
		{"cluster matches git", false, "0", "0", "", "", []string{"diff", "diff"}},
		{"differs, live: namespace applied before the rest", false, "1", "0", "", "", []string{"diff", "apply", "diff", "apply"}},
		{"differs, dry-run, namespace exists", true, "1", "0", "demo", "", []string{"diff", "get", "diff"}},
		{"differs, dry-run, namespace pending", true, "1", "0", "", "", []string{"diff", "get"}},
		{"diff errors", false, "2", "0", "", "kubectl diff: exit 2", []string{"diff"}},
		{"apply errors", false, "1", "1", "", "kubectl apply: exit 1: apply boom", []string{"diff", "apply"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupKubectl(t, tt.diffExit, tt.applyExit)
			t.Setenv("FAKE_EXISTING_NS", tt.existing)

			err := applier(tt.dryRun).Apply(context.Background(), "abc123", resources(t))
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}

			got := calls(t, dir)
			if len(got) != len(tt.wantCalls) {
				t.Fatalf("calls = %q, want %v", got, tt.wantCalls)
			}
			for i, sub := range tt.wantCalls {
				if !strings.HasPrefix(got[i], sub+" ") {
					t.Fatalf("call %d = %q, want %s", i, got[i], sub)
				}
			}
		})
	}
}

func TestApplyNamespacesFirstServerSide(t *testing.T) {
	dir := setupKubectl(t, "1", "0")
	if err := applier(false).Apply(context.Background(), "abc123", resources(t)); err != nil {
		t.Fatal(err)
	}

	got := calls(t, dir)
	want := []string{
		"diff --server-side --force-conflicts --field-manager=gitops -f -",
		"apply --server-side --force-conflicts --field-manager=gitops -f -",
		"diff --server-side --force-conflicts --field-manager=gitops -f -",
		"apply --server-side --force-conflicts --field-manager=gitops -f -",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", got, want)
	}

	// First apply carries only the Namespace, second only the ConfigMap.
	first, second := stdinOf(t, dir, 2), stdinOf(t, dir, 4)
	if !strings.Contains(first, "kind: Namespace") || strings.Contains(first, "kind: ConfigMap") {
		t.Fatalf("first apply should be the namespace only:\n%s", first)
	}
	if !strings.Contains(second, "name: cfg") || strings.Contains(second, "kind: Namespace") {
		t.Fatalf("second apply should be the configmap only:\n%s", second)
	}
}

func TestDryRunAsksForDeclaredNamespaces(t *testing.T) {
	dir := setupKubectl(t, "1", "0")
	if err := applier(true).Apply(context.Background(), "abc123", resources(t)); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, dir); got[1] != "get namespace demo --ignore-not-found -o name" {
		t.Fatalf("get args = %q", got[1])
	}
}

func TestDryRunKeepsResourcesOutsidePendingNamespaces(t *testing.T) {
	dir := setupKubectl(t, "1", "0")
	t.Setenv("FAKE_EXISTING_NS", "") // demo does not exist yet
	rs := append(resources(t), manifest.Resource{
		Kind: "ConfigMap", Namespace: "staging", Name: "other", Raw: []byte("kind: ConfigMap\nname: other\n"),
	})
	a := applier(true)
	a.Policy = policy.New([]string{"demo", "staging"})

	if err := a.Apply(context.Background(), "abc123", rs); err != nil {
		t.Fatal(err)
	}
	// diff(ns), get, diff(rest): the rest must hold staging/other but not demo/cfg.
	rest := stdinOf(t, dir, 3)
	if !strings.Contains(rest, "name: other") || strings.Contains(rest, "name: cfg") {
		t.Fatalf("dry-run diff of the rest = \n%s", rest)
	}
}

func TestDryRunGetNamespaceFails(t *testing.T) {
	setupKubectl(t, "1", "0")
	t.Setenv("FAKE_GET_EXIT", "1")
	err := applier(true).Apply(context.Background(), "abc123", resources(t))
	if err == nil || !strings.Contains(err.Error(), "kubectl get: exit 1: get boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoNamespacesSkipsGet(t *testing.T) {
	dir := setupKubectl(t, "1", "0")
	rs := resources(t)[1:] // ConfigMap only
	if err := applier(true).Apply(context.Background(), "abc123", rs); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, dir); len(got) != 1 || !strings.HasPrefix(got[0], "diff ") {
		t.Fatalf("calls = %q, want a single diff", got)
	}
}

func TestPolicyViolationNeverCallsKubectl(t *testing.T) {
	dir := setupKubectl(t, "1", "0")
	rs := []manifest.Resource{{Kind: "ConfigMap", Namespace: "kube-system", Name: "evil", File: "evil.yaml"}}

	err := applier(false).Apply(context.Background(), "abc123", rs)
	if err == nil || !strings.Contains(err.Error(), "policy:") {
		t.Fatalf("err = %v, want policy error", err)
	}
	if c := calls(t, dir); c != nil {
		t.Fatalf("kubectl called despite policy violation: %q", c)
	}
}

func TestNoResourcesNeverCallsKubectl(t *testing.T) {
	dir := setupKubectl(t, "1", "0")
	if err := applier(false).Apply(context.Background(), "abc123", nil); err != nil {
		t.Fatal(err)
	}
	if c := calls(t, dir); c != nil {
		t.Fatalf("kubectl called with no resources: %q", c)
	}
}

func TestKubectlMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: no kubectl anywhere
	err := applier(false).Apply(context.Background(), "abc123", resources(t))
	if err == nil || !strings.Contains(err.Error(), "kubectl diff") {
		t.Fatalf("err = %v, want kubectl not found error", err)
	}
}

func TestJoinPutsNamespacesFirst(t *testing.T) {
	cm := manifest.Resource{Kind: "ConfigMap", Raw: []byte("kind: ConfigMap\n")}
	ns := manifest.Resource{Kind: "Namespace", Raw: []byte("kind: Namespace\n")}
	got := string(join([]manifest.Resource{cm, ns}))
	if strings.Index(got, "kind: Namespace") > strings.Index(got, "kind: ConfigMap") {
		t.Fatalf("Namespace must come before ConfigMap: \n%s", got)
	}
}

func TestCustomKubectlPath(t *testing.T) {
	dir := setupKubectl(t, "0", "0")
	t.Setenv("PATH", t.TempDir()) // fake is no longer on PATH, only reachable by full path
	a := applier(false)
	a.Kubectl = filepath.Join(dir, "kubectl")
	if err := a.Apply(context.Background(), "abc123", resources(t)); err != nil {
		t.Fatal(err)
	}
	if c := calls(t, dir); len(c) == 0 || !strings.HasPrefix(c[0], "diff") {
		t.Fatalf("calls = %q, want the custom kubectl to run diff", c)
	}
}

func TestApplyReportsOutcome(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		dryRun               bool
		diffExit             string
		wantApplied, wantPen bool
	}{
		{"applied", false, "1", true, false},
		{"dry-run pending", true, "1", false, true},
		{"no diff", false, "0", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupKubectl(t, tt.diffExit, "0")
			t.Setenv("FAKE_EXISTING_NS", "demo")
			ctx, out := report.With(context.Background())
			if err := applier(tt.dryRun).Apply(ctx, "abc123", resources(t)); err != nil {
				t.Fatal(err)
			}
			if out.Applied() != tt.wantApplied || out.Pending() != tt.wantPen {
				t.Fatalf("applied=%v pending=%v, want %v %v", out.Applied(), out.Pending(), tt.wantApplied, tt.wantPen)
			}
		})
	}
}

func TestPolicyRejectionIsErrPolicy(t *testing.T) {
	setupKubectl(t, "1", "0")
	rs := []manifest.Resource{{Kind: "ConfigMap", Namespace: "kube-system", Name: "evil", File: "evil.yaml"}}
	if err := applier(false).Apply(context.Background(), "abc123", rs); !errors.Is(err, report.ErrPolicy) {
		t.Fatalf("err = %v, want report.ErrPolicy", err)
	}
}
