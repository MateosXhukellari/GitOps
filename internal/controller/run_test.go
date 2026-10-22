package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MateosXhukellari/GitOps/internal/manifest"
)

// gateApplier blocks every Apply until release is closed (if set) and tracks
// how many Applies ran and the most that ever ran at once.
type gateApplier struct {
	started chan struct{}
	release chan struct{}
	running atomic.Int32
	maxSeen atomic.Int32
	calls   atomic.Int32
}

func (g *gateApplier) Apply(context.Context, string, []manifest.Resource) error {
	n := g.running.Add(1)
	defer g.running.Add(-1)
	for {
		m := g.maxSeen.Load()
		if n <= m || g.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	g.calls.Add(1)
	g.started <- struct{}{}
	if g.release != nil {
		<-g.release
	}
	return nil
}

func waitStarted(t *testing.T, g *gateApplier) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile did not start")
	}
}

func TestTriggerReconcilesImmediately(t *testing.T) {
	g := &gateApplier{started: make(chan struct{}, 10)}
	c, _ := statusController(t, g) // poll interval defaults to 1m
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitStarted(t, g)

	start := time.Now()
	c.Trigger()
	waitStarted(t, g)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("triggered reconcile took %s", d)
	}
}

func TestBurstOfTriggersNeverOverlaps(t *testing.T) {
	g := &gateApplier{started: make(chan struct{}, 10), release: make(chan struct{})}
	c, _ := statusController(t, g)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitStarted(t, g) // first reconcile is now blocked inside Apply

	for i := 0; i < 20; i++ {
		c.Trigger()
	}
	close(g.release)
	waitStarted(t, g) // exactly one queued reconcile follows

	time.Sleep(300 * time.Millisecond)
	if calls, maxSeen := g.calls.Load(), g.maxSeen.Load(); calls != 2 || maxSeen != 1 {
		t.Fatalf("calls=%d max concurrent=%d, want 2 and 1", calls, maxSeen)
	}
}
