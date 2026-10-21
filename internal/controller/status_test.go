package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/MateosXhukellari/GitOps/internal/git"
	"github.com/MateosXhukellari/GitOps/internal/manifest"
	"github.com/MateosXhukellari/GitOps/internal/report"
)

// markApplier calls mark on the cycle's context and returns err.
type markApplier struct {
	mark func(ctx context.Context)
	err  error
}

func (m *markApplier) Apply(ctx context.Context, _ string, _ []manifest.Resource) error {
	if m.mark != nil {
		m.mark(ctx)
	}
	return m.err
}

// statusRepo makes a one-commit repo holding a Namespace manifest.
func statusRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	statusGit(t, src, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(src, "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	statusCommit(t, src, "m/ns.yaml", nsYAML)
	return src
}

func statusGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func statusCommit(t *testing.T, src, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	statusGit(t, src, "add", ".")
	statusGit(t, src, "commit", "-q", "-m", name)
}

func statusController(t *testing.T, a Applier) (*Controller, string) {
	t.Helper()
	src := statusRepo(t)
	repo := git.Repo{URL: src, Branch: "main", Dir: filepath.Join(t.TempDir(), "co")}
	return New(repo, "m", 0, a, slog.New(slog.NewTextHandler(io.Discard, nil))), src
}

func TestStatusStartsUnknown(t *testing.T) {
	c := New(git.Repo{}, ".", 0, &markApplier{}, nil)
	st := c.Status()
	if st.Sync != SyncUnknown || st.Failures == nil {
		t.Fatalf("sync=%q failures=%v, want Unknown and a non-nil map", st.Sync, st.Failures)
	}
}

func TestCleanCycleIsSynced(t *testing.T) {
	c, _ := statusController(t, &markApplier{})
	c.Reconcile(context.Background())
	st := c.Status()
	if st.Sync != SyncSynced || st.Applies != 0 || st.Pruned != 0 || st.LastErrorKind != "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestMarksAreCounted(t *testing.T) {
	a := &markApplier{mark: func(ctx context.Context) {
		report.MarkApplied(ctx)
		report.MarkPruned(ctx, 2)
	}}
	c, _ := statusController(t, a)
	c.Reconcile(context.Background())
	c.Reconcile(context.Background())
	st := c.Status()
	if st.Sync != SyncSynced || st.Applies != 2 || st.Pruned != 4 {
		t.Fatalf("sync=%s applies=%d pruned=%d, want Synced 2 4", st.Sync, st.Applies, st.Pruned)
	}
}

func TestPendingIsOutOfSync(t *testing.T) {
	c, _ := statusController(t, &markApplier{mark: report.MarkPending})
	c.Reconcile(context.Background())
	if st := c.Status(); st.Sync != SyncOutOfSync || st.Applies != 0 {
		t.Fatalf("sync=%s applies=%d, want OutOfSync 0", st.Sync, st.Applies)
	}
}

func TestApplyFailuresAreClassified(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: 1 violation(s)", report.ErrPolicy), KindPolicy},
		{fmt.Errorf("%w: deleting x: boom", report.ErrPrune), KindPrune},
		{errors.New("kubectl apply: exit 1"), KindApply},
	} {
		c, _ := statusController(t, &markApplier{err: tt.err})
		c.Reconcile(context.Background())
		st := c.Status()
		if st.LastErrorKind != tt.want || st.Failures[tt.want] != 1 || st.Sync != SyncUnknown {
			t.Errorf("%v: kind=%q failures=%v sync=%s, want %s", tt.err, st.LastErrorKind, st.Failures, st.Sync, tt.want)
		}
	}
}

func TestGitFailureIsClassified(t *testing.T) {
	c, _ := statusController(t, &markApplier{})
	c.Repo.URL = filepath.Join(t.TempDir(), "missing")
	c.Reconcile(context.Background())
	if st := c.Status(); st.LastErrorKind != KindGit || st.Failures[KindGit] != 1 {
		t.Fatalf("kind=%q failures=%v", st.LastErrorKind, st.Failures)
	}
}

func TestManifestFailureIsClassified(t *testing.T) {
	c, src := statusController(t, &markApplier{})
	statusCommit(t, src, "m/bad.yaml", "just text\n")
	c.Reconcile(context.Background())
	if st := c.Status(); st.LastErrorKind != KindManifest || st.Failures[KindManifest] != 1 {
		t.Fatalf("kind=%q failures=%v", st.LastErrorKind, st.Failures)
	}
}

func TestGoodCycleClearsErrorKeepsTotals(t *testing.T) {
	a := &markApplier{err: errors.New("boom")}
	c, _ := statusController(t, a)
	c.Reconcile(context.Background())
	a.err = nil
	c.Reconcile(context.Background())
	st := c.Status()
	if st.LastError != "" || st.LastErrorKind != "" || st.Sync != SyncSynced {
		t.Fatalf("error not cleared: %+v", st)
	}
	if st.Failures[KindApply] != 1 {
		t.Fatalf("failures = %v, want apply=1 kept", st.Failures)
	}
}

func TestStatusCopiesFailures(t *testing.T) {
	c, _ := statusController(t, &markApplier{err: errors.New("boom")})
	c.Reconcile(context.Background())
	st := c.Status()
	st.Failures[KindApply] = 99
	if got := c.Status().Failures[KindApply]; got != 1 {
		t.Fatalf("Status() leaked its map: apply=%d after caller edit", got)
	}
}
