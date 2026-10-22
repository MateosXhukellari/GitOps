package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/MateosXhukellari/GitOps/internal/health"
	"github.com/MateosXhukellari/GitOps/internal/manifest"
)

type fakeHealth struct{ results []health.Resource }

func (f *fakeHealth) Check(context.Context, []manifest.Resource) []health.Resource { return f.results }

func TestHealthDisabledByDefault(t *testing.T) {
	c, _ := statusController(t, &markApplier{})
	c.Reconcile(context.Background())
	if st := c.Status(); st.Health != "" || st.Unhealthy != nil {
		t.Fatalf("health=%q unhealthy=%v, want empty", st.Health, st.Unhealthy)
	}
}

func TestFailingHealthCheckDoesNotFailReconcile(t *testing.T) {
	c, _ := statusController(t, &markApplier{})
	c.Health = &fakeHealth{[]health.Resource{
		{Kind: "Deployment", Namespace: "demo", Name: "ok", Health: health.Healthy},
		{Kind: "Deployment", Namespace: "demo", Name: "web", Health: health.Unknown, Reason: "kubectl get: exit 1"},
	}}
	c.Reconcile(context.Background())
	st := c.Status()
	if st.Sync != SyncSynced || st.LastError != "" || st.AppliedRev == "" {
		t.Fatalf("health must not change the sync result: %+v", st)
	}
	if st.Health != health.Unknown || len(st.Unhealthy) != 1 || st.Unhealthy[0].Name != "web" {
		t.Fatalf("health=%q unhealthy=%+v", st.Health, st.Unhealthy)
	}
}

func TestDegradedWarnsFromSecondCycle(t *testing.T) {
	c, _ := statusController(t, &markApplier{})
	var logs bytes.Buffer
	c.Log = slog.New(slog.NewTextHandler(&logs, nil))
	h := &fakeHealth{[]health.Resource{
		{Kind: "Deployment", Namespace: "demo", Name: "web", Health: health.Degraded, Reason: "ProgressDeadlineExceeded"},
		{Kind: "Deployment", Namespace: "demo", Name: "api", Health: health.Progressing},
	}}
	c.Health = h
	warns := func() int { return strings.Count(logs.String(), "level=WARN") }

	c.Reconcile(context.Background())
	if warns() != 0 {
		t.Fatalf("first Degraded cycle must not warn:\n%s", logs.String())
	}
	c.Reconcile(context.Background())
	if warns() != 1 || !strings.Contains(logs.String(), "name=web cycles=2") {
		t.Fatalf("want one WARN for web on cycle 2:\n%s", logs.String())
	}

	h.results = []health.Resource{{Kind: "Deployment", Namespace: "demo", Name: "web", Health: health.Healthy}}
	c.Reconcile(context.Background())
	h.results[0].Health = health.Degraded
	c.Reconcile(context.Background())
	if warns() != 1 {
		t.Fatalf("recovering must reset the streak:\n%s", logs.String())
	}
	if st := c.Status(); st.Health != health.Degraded {
		t.Fatalf("health=%q", st.Health)
	}
}
