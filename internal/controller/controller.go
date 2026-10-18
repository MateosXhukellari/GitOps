package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rta/gitops/internal/git"
	"github.com/rta/gitops/internal/manifest"
)

type Applier interface {
	Apply(ctx context.Context, revision string, desired []manifest.Resource) error
}

type Status struct {
	Revision      string    `json:"revision"`
	Resources     int       `json:"resources"`
	LastAttempt   time.Time `json:"lastAttempt"`
	LastSuccess   time.Time `json:"lastSuccess"`
	LastError     string    `json:"lastError,omitempty"`
	AppliedRev    string    `json:"appliedRevision"`
	ReconcileRuns int       `json:"reconcileRuns"`
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
	return c.status // returns a copy
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
	c.mu.Lock()
	c.status.LastAttempt = time.Now()
	c.status.ReconcileRuns++
	applied := c.status.AppliedRev
	c.mu.Unlock()

	rev, err := c.Repo.Sync(ctx)
	if err != nil {
		c.fail("git sync failed", err)
		return
	}
	desired, err := manifest.Load(c.Repo.Dir, c.Path)
	if err != nil {
		c.fail("load manifests failed", err)
		return
	}

	// only apply when commit changed
	// last good revision is never overwritten
	if rev != applied {
		if err := c.Applier.Apply(ctx, rev, desired); err != nil {
			c.fail("apply failed", err)
			return
		}
	}

	c.mu.Lock()
	c.status.Revision = rev
	c.status.AppliedRev = rev
	c.status.Resources = len(desired)
	c.status.LastSuccess = time.Now()
	c.status.LastError = ""
	c.mu.Unlock()
}

func (c *Controller) fail(msg string, err error) {
	c.Log.Error(msg, "err", err)
	c.mu.Lock()
	c.status.LastError = msg + ": " + err.Error()
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
