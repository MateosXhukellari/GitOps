package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rta/gitops/internal/git"
	"github.com/rta/gitops/internal/manifest"
)

// fakeApplier records every Apply call and can be told to fail.
type fakeApplier struct {
	calls []string // revisions applied
	err   error
}

func (f *fakeApplier) Apply(_ context.Context, rev string, _ []manifest.Resource) error {
	f.calls = append(f.calls, rev)
	return f.err
}

// gitCmd runs git in dir and fails the test on error.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@test"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes a file into the source repo and commits it, returning the new SHA.
func commitFile(t *testing.T, repo, name, content string) string {
	t.Helper()
	p := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-q", "-m", "update "+name)
	return gitCmd(t, repo, "rev-parse", "HEAD")
}

const nsYAML = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n"

// setup creates a source repo with one manifest and a controller pointed at it.
func setup(t *testing.T) (src string, firstRev string, c *Controller, app *fakeApplier) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src = t.TempDir()
	gitCmd(t, src, "init", "-q", "-b", "main")
	firstRev = commitFile(t, src, "manifests/ns.yaml", nsYAML)

	app = &fakeApplier{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo := git.Repo{URL: src, Branch: "main", Dir: filepath.Join(t.TempDir(), "checkout")}
	c = New(repo, "manifests", 0, app, log)
	return src, firstRev, c, app
}

func TestReconcileAppliesOnlyOnNewCommit(t *testing.T) {
	src, rev1, c, app := setup(t)
	ctx := context.Background()

	c.Reconcile(ctx)
	st := c.Status()
	if st.LastError != "" {
		t.Fatalf("unexpected error: %s", st.LastError)
	}
	if st.AppliedRev != rev1 || st.Resources != 1 {
		t.Fatalf("got rev=%s resources=%d, want rev=%s resources=1", st.AppliedRev, st.Resources, rev1)
	}

	// Same commit: must not apply again.
	c.Reconcile(ctx)
	if len(app.calls) != 1 {
		t.Fatalf("applied %d times on unchanged commit, want 1", len(app.calls))
	}

	// New commit: must apply the new revision.
	rev2 := commitFile(t, src, "manifests/cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n  namespace: demo\n")
	c.Reconcile(ctx)
	st = c.Status()
	if len(app.calls) != 2 || app.calls[1] != rev2 {
		t.Fatalf("calls=%v, want second apply of %s", app.calls, rev2)
	}
	if st.AppliedRev != rev2 || st.Resources != 2 || st.ReconcileRuns != 3 {
		t.Fatalf("status = %+v", st)
	}
}

func TestReconcileApplyFailureKeepsLastGoodRevision(t *testing.T) {
	_, rev1, c, app := setup(t)
	ctx := context.Background()

	app.err = errors.New("boom")
	c.Reconcile(ctx)
	st := c.Status()
	if !strings.Contains(st.LastError, "apply failed: boom") {
		t.Fatalf("LastError = %q", st.LastError)
	}
	if st.AppliedRev != "" || !st.LastSuccess.IsZero() {
		t.Fatalf("failed apply must not record success: %+v", st)
	}

	// Once apply works again, the same commit is retried and the error clears.
	app.err = nil
	c.Reconcile(ctx)
	st = c.Status()
	if st.LastError != "" || st.AppliedRev != rev1 || len(app.calls) != 2 {
		t.Fatalf("after recovery: status=%+v calls=%v", st, app.calls)
	}
}

func TestReconcileBadManifestReportsError(t *testing.T) {
	src, rev1, c, app := setup(t)
	ctx := context.Background()
	c.Reconcile(ctx)

	// A resource without metadata.name is invalid.
	commitFile(t, src, "manifests/bad.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  namespace: demo\n")
	c.Reconcile(ctx)
	st := c.Status()
	if !strings.Contains(st.LastError, "load manifests failed") {
		t.Fatalf("LastError = %q", st.LastError)
	}
	if st.AppliedRev != rev1 || len(app.calls) != 1 {
		t.Fatalf("bad manifest must not be applied: status=%+v calls=%v", st, app.calls)
	}
}

func TestReconcileSyncFailure(t *testing.T) {
	_, _, c, app := setup(t)
	c.Repo.URL = filepath.Join(t.TempDir(), "does-not-exist")

	c.Reconcile(context.Background())
	st := c.Status()
	if !strings.Contains(st.LastError, "git sync failed") {
		t.Fatalf("LastError = %q", st.LastError)
	}
	if len(app.calls) != 0 {
		t.Fatalf("applied despite sync failure: %v", app.calls)
	}
}

func TestTriggerNeverBlocks(t *testing.T) {
	c := New(git.Repo{}, ".", 0, &fakeApplier{}, nil)
	// The channel holds one signal; extra triggers must be dropped, not block.
	for i := 0; i < 5; i++ {
		c.Trigger()
	}
	if len(c.trigger) != 1 {
		t.Fatalf("queued %d triggers, want 1", len(c.trigger))
	}
}

func TestNewDefaults(t *testing.T) {
	c := New(git.Repo{}, ".", 0, &fakeApplier{}, nil)
	if c.Interval <= 0 || c.Log == nil {
		t.Fatalf("defaults not applied: interval=%v log=%v", c.Interval, c.Log)
	}
}
