package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/MateosXhukellari/GitOps/internal/git"
	"github.com/MateosXhukellari/GitOps/internal/manifest"
	"github.com/MateosXhukellari/GitOps/internal/report"
)

type Applier interface {
	Apply(ctx context.Context, revision string, desired []manifest.Resource) error
}

const (
	KindGit      = "git"
	KindManifest = "manifest"
	KindPolicy   = "policy"
	KindApply    = "apply"
	KindPrune    = "prune"
)

var FailureKinds = []string{KindGit, KindManifest, KindPolicy, KindApply, KindPrune}

const (
	SyncUnknown   = "Unknown"
	SyncSynced    = "Synced"
	SyncOutOfSync = "OutOfSync"
)

var failureMessages = map[string]string{
	KindGit:      "git sync failed",
	KindManifest: "load manifests failed",
	KindPolicy:   "policy rejected the revision",
	KindApply:    "apply failed",
	KindPrune:    "prune failed",
}

type Status struct {
	Revision      string    `json:"revision"`
	Resources     int       `json:"resources"`
	LastAttempt   time.Time `json:"lastAttempt"`
	LastSuccess   time.Time `json:"lastSuccess"`
	LastError     string    `json:"lastError,omitempty"`
	AppliedRev    string    `json:"appliedRevision"`
	ReconcileRuns int       `json:"reconcileRuns"`

	Sync          string         `json:"sync"`                    // Synced, OutOfSync or Unknown
	LastErrorKind string         `json:"lastErrorKind,omitempty"` // one of FailureKinds
	Failures      map[string]int `json:"failures"`                // total failed cycles by kind
	Applies       int            `json:"applies"`                 // cycles that changed the cluster
	Pruned        int            `json:"pruned"`
}

type Controller struct {
	Repo     git.Repo
	Path     string
	Interval time.Duration
	Applier  Applier
	Log      *slog.Logger

	trigger chan struct{} // webhook -> loop signal, size 1
	mu      sync.RWMutex  // protects status: loop writes, HTTP reads
	status  Status
}

func New(repo git.Repo, path string, interval time.Duration, applier Applier, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = time.Minute
	}
	return &Controller{
		Repo: repo, Path: path, Interval: interval, Applier: applier, Log: log,
		trigger: make(chan struct{}, 1),
		status:  Status{Sync: SyncUnknown, Failures: map[string]int{}},
	}
}

// trigger asks from reconcile now. If one is alreadt queued, extra resources are dropped
func (c *Controller) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

func (c *Controller) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st := c.status
	st.Failures = make(map[string]int, len(c.status.Failures))
	for k, v := range c.status.Failures {
		st.Failures[k] = v
	}
	return st // returns a copy
}

func (c *Controller) Run(ctx context.Context) {
	c.Reconcile(ctx)
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.trigger:
		}
		c.Reconcile(ctx)
	}
}

func (c *Controller) Reconcile(ctx context.Context) {
	start := time.Now()
	c.mu.Lock()
	c.status.LastAttempt = time.Now()
	c.status.ReconcileRuns++
	c.mu.Unlock()

	rev, err := c.Repo.Sync(ctx)
	if err != nil {
		c.fail(KindGit, err)
		return
	}
	desired, err := manifest.Load(c.Repo.Dir, c.Path)
	if err != nil {
		c.fail(KindManifest, err)
		return
	}

	// only apply when commit changed
	// last good revision is never overwritten
	ctx, out := report.With(ctx)
	if err := c.Applier.Apply(ctx, rev, desired); err != nil {
		c.fail(kindOf(err), err)
		return
	}

	sync := SyncSynced
	if out.Pending() {
		sync = SyncOutOfSync
	}

	c.mu.Lock()
	c.status.Revision = rev
	c.status.AppliedRev = rev
	c.status.Resources = len(desired)
	c.status.LastSuccess = time.Now()
	c.status.LastError = ""
	c.status.LastErrorKind = ""
	c.status.Sync = sync
	if out.Applied() {
		c.status.Applies++
	}
	c.status.Pruned += out.Pruned()
	c.mu.Unlock()

	c.Log.Info("reconciled",
		"revision", shortRev(rev), "resources", len(desired), "sync", sync,
		"applied", out.Applied(), "pruned", out.Pruned(),
		"took", time.Since(start).Round(time.Millisecond))
}

func kindOf(err error) string {
	switch {
	case errors.Is(err, report.ErrPolicy):
		return KindPolicy
	case errors.Is(err, report.ErrPrune):
		return KindPrune
	default:
		return KindApply
	}
}

func shortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

func (c *Controller) fail(kind string, err error) {
	msg := failureMessages[kind]
	c.Log.Error(msg, "kind", kind, "err", err)
	c.mu.Lock()
	c.status.LastError = msg + ": " + err.Error()
	c.status.LastErrorKind = kind
	c.status.Failures[kind]++
	c.status.Sync = SyncUnknown
	c.mu.Unlock()
}

type LogApplier struct{ Log *slog.Logger }

func (a LogApplier) Apply(_ context.Context, revision string, desired []manifest.Resource) error {
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("desired state loaded", "revision", revision, "resources", len(desired))
	for _, r := range desired {
		log.Info(" resource", "kind", r.Kind, "namespace", r.Namespace, "name", r.Name, "file", r.File)
	}
	return nil
}
