package prune

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rta/gitops/internal/controller"
	"github.com/rta/gitops/internal/kube"
	"github.com/rta/gitops/internal/manifest"
	"github.com/rta/gitops/internal/report"
)

// Compile-time checks that the pieces fit together as wired in main.go:
// kube.Applier can be wrapped, and the Pruner can be given to the controller.
var (
	_ Applier            = kube.Applier{}
	_ controller.Applier = Pruner{}
)

type fakeNext struct {
	err    error
	called bool
}

func (f *fakeNext) Apply(context.Context, string, []manifest.Resource) error {
	f.called = true
	return f.err
}

// fakeKubectl writes a stub kubectl. `get` prints liveJSON (or fails when
// getFails), every call is logged, and label's stdin is saved.
func fakeKubectl(t *testing.T, liveJSON string, getFails bool) (bin, callLog, labelStdin string) {
	t.Helper()
	dir := t.TempDir()
	callLog = filepath.Join(dir, "calls.log")
	labelStdin = filepath.Join(dir, "label.stdin")
	live := filepath.Join(dir, "live.json")
	if err := os.WriteFile(live, []byte(liveJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	fail := "0"
	if getFails {
		fail = "1"
	}
	script := `#!/bin/sh
echo "$@" >> "` + callLog + `"
if [ "$1" = label ]; then cat > "` + labelStdin + `"; exit 0; fi
cat > /dev/null
if [ "$1" = get ]; then
  if [ ` + fail + ` = 1 ]; then echo "boom" >&2; exit 1; fi
  cat "` + live + `"
fi
exit 0
`
	bin = filepath.Join(dir, "kubectl")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, callLog, labelStdin
}

func calls(t *testing.T, path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func res(kind, name string) manifest.Resource {
	return manifest.Resource{
		APIVersion: "v1", Kind: kind, Namespace: "demo", Name: name,
		Raw: []byte("kind: " + kind + "\nmetadata:\n  name: " + name + "\n  namespace: demo\n"),
	}
}

const liveWebAndOld = `{"items":[
 {"kind":"Deployment","metadata":{"name":"web","namespace":"demo"}},
 {"kind":"ConfigMap","metadata":{"name":"old","namespace":"demo"}}]}`

func pruner(bin string, next Applier) Pruner {
	return Pruner{Next: next, Kubectl: bin, Owner: "gitops",
		Kinds: []string{"deployments.apps", "configmaps"}, Namespaces: []string{"demo"}}
}

func TestPrunesOnlyWhatIsGoneFromGit(t *testing.T) {
	bin, log, labelIn := fakeKubectl(t, liveWebAndOld, false)
	err := pruner(bin, &fakeNext{}).Apply(context.Background(), "abc",
		[]manifest.Resource{res("Deployment", "web")})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls(t, log), "\n")
	if !strings.Contains(got, "delete configmap old -n demo") {
		t.Fatalf("stale ConfigMap not deleted:\n%s", got)
	}
	if strings.Contains(got, "delete deployment") {
		t.Fatalf("a resource still in git must never be deleted:\n%s", got)
	}
	if !strings.Contains(got, "label --overwrite -f - "+OwnerLabel+"=gitops") {
		t.Fatalf("managed resources were not labelled:\n%s", got)
	}
	b, _ := os.ReadFile(labelIn)
	if !strings.Contains(string(b), "kind: Deployment") {
		t.Fatalf("label stdin should carry the manifests, got %q", b)
	}
}

func TestFailedApplyNeverPrunes(t *testing.T) {
	bin, log, _ := fakeKubectl(t, liveWebAndOld, false)
	boom := errors.New("apply failed")
	err := pruner(bin, &fakeNext{err: boom}).Apply(context.Background(), "abc",
		[]manifest.Resource{res("Deployment", "web")})
	if !errors.Is(err, boom) {
		t.Fatalf("want the apply error back, got %v", err)
	}
	if got := calls(t, log); len(got) != 0 {
		t.Fatalf("kubectl must not be called after a failed apply, got %v", got)
	}
}

func TestDryRunDeletesAndLabelsNothing(t *testing.T) {
	bin, log, _ := fakeKubectl(t, liveWebAndOld, false)
	p := pruner(bin, &fakeNext{})
	p.DryRun = true
	if err := p.Apply(context.Background(), "abc", []manifest.Resource{res("Deployment", "web")}); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls(t, log) {
		if strings.HasPrefix(c, "delete") || strings.HasPrefix(c, "label") {
			t.Fatalf("dry-run must not change the cluster, got %q", c)
		}
	}
}

func TestEmptyRevisionNeverPrunes(t *testing.T) {
	bin, log, _ := fakeKubectl(t, liveWebAndOld, false)
	ns := manifest.Resource{APIVersion: "v1", Kind: "Namespace", Name: "demo", Raw: []byte("kind: Namespace\n")}
	for _, desired := range [][]manifest.Resource{nil, {ns}} {
		if err := pruner(bin, &fakeNext{}).Apply(context.Background(), "abc", desired); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls(t, log); len(got) != 0 {
		t.Fatalf("an empty revision must not prune anything, got %v", got)
	}
}

func TestListFailureDeletesNothing(t *testing.T) {
	bin, log, _ := fakeKubectl(t, liveWebAndOld, true)
	err := pruner(bin, &fakeNext{}).Apply(context.Background(), "abc",
		[]manifest.Resource{res("Deployment", "web")})
	if err == nil || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Fatalf("want a listing error, got %v", err)
	}
	for _, c := range calls(t, log) {
		if strings.HasPrefix(c, "delete") {
			t.Fatalf("no delete may happen after a failed listing, got %q", c)
		}
	}
}

func TestRefusesToDeleteTooMany(t *testing.T) {
	bin, log, _ := fakeKubectl(t, liveWebAndOld, false)
	p := pruner(bin, &fakeNext{})
	p.MaxDeletes = 1
	// Nothing from the live list is in git, so both objects are stale.
	err := p.Apply(context.Background(), "abc", []manifest.Resource{res("Service", "other")})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete 2 objects") {
		t.Fatalf("want a limit error, got %v", err)
	}
	for _, c := range calls(t, log) {
		if strings.HasPrefix(c, "delete") {
			t.Fatalf("nothing may be deleted over the limit, got %q", c)
		}
	}
}

func TestOutcomeMarks(t *testing.T) {
	bin, _, _ := fakeKubectl(t, liveWebAndOld, false)
	ctx, out := report.With(context.Background())
	if err := pruner(bin, &fakeNext{}).Apply(ctx, "abc", []manifest.Resource{res("Deployment", "web")}); err != nil {
		t.Fatal(err)
	}
	if out.Pruned() != 1 || out.Pending() {
		t.Fatalf("live: pruned=%d pending=%v, want 1 false", out.Pruned(), out.Pending())
	}

	p := pruner(bin, &fakeNext{})
	p.DryRun = true
	ctx, out = report.With(context.Background())
	if err := p.Apply(ctx, "abc", []manifest.Resource{res("Deployment", "web")}); err != nil {
		t.Fatal(err)
	}
	if out.Pruned() != 0 || !out.Pending() {
		t.Fatalf("dry-run: pruned=%d pending=%v, want 0 true", out.Pruned(), out.Pending())
	}
}

func TestPruneErrorsAreErrPrune(t *testing.T) {
	bin, _, _ := fakeKubectl(t, liveWebAndOld, true) // listing fails
	err := pruner(bin, &fakeNext{}).Apply(context.Background(), "abc", []manifest.Resource{res("Deployment", "web")})
	if !errors.Is(err, report.ErrPrune) {
		t.Fatalf("listing failure: err = %v, want report.ErrPrune", err)
	}

	okBin, _, _ := fakeKubectl(t, liveWebAndOld, false)
	p := pruner(okBin, &fakeNext{})
	p.MaxDeletes = 1
	err = p.Apply(context.Background(), "abc", []manifest.Resource{res("Service", "other")})
	if !errors.Is(err, report.ErrPrune) || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("over the limit: err = %v, want report.ErrPrune", err)
	}

	// An apply failure passed through from Next must keep its own kind.
	err = pruner(bin, &fakeNext{err: errors.New("kubectl apply: exit 1")}).Apply(context.Background(), "abc", nil)
	if err == nil || errors.Is(err, report.ErrPrune) {
		t.Fatalf("apply failure: err = %v, must not be report.ErrPrune", err)
	}
}
