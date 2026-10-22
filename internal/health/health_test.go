package health

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateosXhukellari/GitOps/internal/manifest"
)

// fakeKubectl writes a stub kubectl that logs its args and prints liveJSON,
// or fails like a missing object when liveJSON is empty.
func fakeKubectl(t *testing.T, liveJSON string) (bin, callLog string) {
	t.Helper()
	dir := t.TempDir()
	callLog = filepath.Join(dir, "calls.log")
	live := filepath.Join(dir, "live.json")
	if err := os.WriteFile(live, []byte(liveJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$@" >> "` + callLog + `"
if [ ! -s "` + live + `" ]; then echo 'Error from server (NotFound): deployments.apps "web" not found' >&2; exit 1; fi
cat "` + live + `"
`
	bin = filepath.Join(dir, "kubectl")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, callLog
}

var desired = []manifest.Resource{
	{Kind: "Service", Namespace: "demo", Name: "web"},
	{Kind: "Deployment", Namespace: "demo", Name: "web"},
}

func checkOne(t *testing.T, liveJSON string) Resource {
	t.Helper()
	bin, callLog := fakeKubectl(t, liveJSON)
	got := Checker{Kubectl: bin}.Check(context.Background(), desired)
	if len(got) != 1 {
		t.Fatalf("want 1 result (Deployments only), got %+v", got)
	}
	b, _ := os.ReadFile(callLog)
	if strings.TrimSpace(string(b)) != "get deployment web -o json -n demo" {
		t.Fatalf("unexpected kubectl calls: %q", b)
	}
	return got[0]
}

func TestHealthy(t *testing.T) {
	r := checkOne(t, `{"metadata":{"generation":3},"spec":{"replicas":2},
		"status":{"observedGeneration":3,"replicas":2,"updatedReplicas":2,"availableReplicas":2,
		"conditions":[{"type":"Available","status":"True"},{"type":"Progressing","status":"True","reason":"NewReplicaSetAvailable"}]}}`)
	if r.Health != Healthy || r.Reason != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestProgressing(t *testing.T) {
	r := checkOne(t, `{"metadata":{"generation":4},"spec":{"replicas":2},
		"status":{"observedGeneration":4,"updatedReplicas":1,"availableReplicas":2,
		"conditions":[{"type":"Progressing","status":"True","reason":"ReplicaSetUpdated"}]}}`)
	if r.Health != Progressing || r.Reason != "1/2 updated, 2/2 available" {
		t.Fatalf("got %+v", r)
	}
}

func TestProgressingUntilGenerationObserved(t *testing.T) {
	r := checkOne(t, `{"metadata":{"generation":5},"spec":{"replicas":1},
		"status":{"observedGeneration":4,"updatedReplicas":1,"availableReplicas":1}}`)
	if r.Health != Progressing {
		t.Fatalf("got %+v", r)
	}
}

func TestDegradedOnDeadlineExceeded(t *testing.T) {
	r := checkOne(t, `{"metadata":{"generation":2},"spec":{"replicas":1},
		"status":{"observedGeneration":2,"updatedReplicas":1,"availableReplicas":0,
		"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded","message":"took too long"}]}}`)
	if r.Health != Degraded || r.Reason != "ProgressDeadlineExceeded: took too long" {
		t.Fatalf("got %+v", r)
	}
}

func TestDegradedOnReplicaFailure(t *testing.T) {
	r := checkOne(t, `{"metadata":{"generation":1},"spec":{"replicas":1},
		"status":{"observedGeneration":1,
		"conditions":[{"type":"ReplicaFailure","status":"True","reason":"FailedCreate","message":"quota"}]}}`)
	if r.Health != Degraded {
		t.Fatalf("got %+v", r)
	}
}

func TestUnknownWhenKubectlFails(t *testing.T) {
	r := checkOne(t, "")
	if r.Health != Unknown || !strings.Contains(r.Reason, "NotFound") {
		t.Fatalf("got %+v", r)
	}
}

func TestReplicasDefaultToOne(t *testing.T) {
	r := checkOne(t, `{"metadata":{"generation":1},"spec":{},
		"status":{"observedGeneration":1,"updatedReplicas":1,"availableReplicas":1}}`)
	if r.Health != Healthy {
		t.Fatalf("got %+v", r)
	}
	r = checkOne(t, `{"metadata":{"generation":1},"spec":{},"status":{"observedGeneration":1}}`)
	if r.Health != Progressing || r.Reason != "0/1 updated, 0/1 available" {
		t.Fatalf("got %+v", r)
	}
}

func TestUnavailableIsNotHealthy(t *testing.T) {
	r := checkOne(t, `{"metadata":{"generation":1},"spec":{"replicas":1},
		"status":{"observedGeneration":1,"updatedReplicas":1,"availableReplicas":1,
		"conditions":[{"type":"Available","status":"False"}]}}`)
	if r.Health != Progressing {
		t.Fatalf("got %+v", r)
	}
}

func TestWorst(t *testing.T) {
	if got := Worst(nil); got != Healthy {
		t.Fatalf("empty: got %s", got)
	}
	rs := []Resource{{Health: Healthy}, {Health: Degraded}, {Health: Unknown}, {Health: Progressing}}
	if got := Worst(rs); got != Degraded {
		t.Fatalf("got %s", got)
	}
	if got := Worst(rs[2:]); got != Unknown {
		t.Fatalf("got %s", got)
	}
}

// checkKind runs Check on a single resource against a fake kubectl printing
// liveJSON ("" = kubectl fails) and asserts the kubectl call made.
func checkKind(t *testing.T, r manifest.Resource, liveJSON, wantCall string) Resource {
	t.Helper()
	bin, callLog := fakeKubectl(t, liveJSON)
	got := Checker{Kubectl: bin}.Check(context.Background(), []manifest.Resource{r})
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %+v", got)
	}
	b, _ := os.ReadFile(callLog)
	if strings.TrimSpace(string(b)) != wantCall {
		t.Fatalf("kubectl call = %q, want %q", b, wantCall)
	}
	return got[0]
}

func TestStatefulSet(t *testing.T) {
	sts := manifest.Resource{APIVersion: "apps/v1", Kind: "StatefulSet", Namespace: "demo", Name: "db"}
	call := "get statefulset db -o json -n demo"
	cases := []struct{ live, want string }{
		{`{"metadata":{"generation":2},"spec":{"replicas":3},"status":{"observedGeneration":2,"updatedReplicas":3,"readyReplicas":3}}`, Healthy},
		{`{"metadata":{"generation":2},"spec":{"replicas":3},"status":{"observedGeneration":2,"updatedReplicas":1,"readyReplicas":3}}`, Progressing},
		{`{"metadata":{"generation":3},"spec":{"replicas":3},"status":{"observedGeneration":2,"updatedReplicas":3,"readyReplicas":3}}`, Progressing},
		{`{"metadata":{"generation":1},"spec":{},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1}}`, Healthy},
		{"", Unknown},
	}
	for _, tc := range cases {
		if got := checkKind(t, sts, tc.live, call); got.Health != tc.want {
			t.Errorf("%s: got %+v, want %s", tc.live, got, tc.want)
		}
	}
}

func TestDaemonSet(t *testing.T) {
	ds := manifest.Resource{APIVersion: "apps/v1", Kind: "DaemonSet", Namespace: "demo", Name: "agent"}
	call := "get daemonset agent -o json -n demo"
	cases := []struct{ live, want string }{
		{`{"metadata":{"generation":4},"status":{"observedGeneration":4,"numberReady":1,"desiredNumberScheduled":1}}`, Healthy},
		{`{"metadata":{"generation":4},"status":{"observedGeneration":4,"numberReady":0,"desiredNumberScheduled":1}}`, Progressing},
		{`{"metadata":{"generation":5},"status":{"observedGeneration":4,"numberReady":1,"desiredNumberScheduled":1}}`, Progressing},
		{"", Unknown},
	}
	for _, tc := range cases {
		if got := checkKind(t, ds, tc.live, call); got.Health != tc.want {
			t.Errorf("%s: got %+v, want %s", tc.live, got, tc.want)
		}
	}
}

func TestRollout(t *testing.T) {
	ro := manifest.Resource{APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Namespace: "demo", Name: "web"}
	call := "get rollouts.argoproj.io web -o json -n demo"
	cases := []struct{ live, want string }{
		{`{"status":{"phase":"Healthy"}}`, Healthy},
		{`{"status":{"phase":"Progressing","message":"more replicas need to be updated"}}`, Progressing},
		{`{"status":{"phase":"Paused","message":"CanaryPauseStep"}}`, Progressing},
		{`{"status":{"phase":"Degraded","message":"ProgressDeadlineExceeded"}}`, Degraded},
		{`{"status":{}}`, Unknown},
		{`{"status":{"phase":"Bogus"}}`, Unknown},
		{"", Unknown}, // also what a missing CRD looks like
	}
	for _, tc := range cases {
		if got := checkKind(t, ro, tc.live, call); got.Health != tc.want {
			t.Errorf("%s: got %+v, want %s", tc.live, got, tc.want)
		}
	}
}

func TestOnlySupportedKindsAreQueried(t *testing.T) {
	bin, callLog := fakeKubectl(t, `{}`)
	got := Checker{Kubectl: bin}.Check(context.Background(), []manifest.Resource{
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "c"},
		{APIVersion: "example.com/v1", Kind: "Rollout", Namespace: "demo", Name: "other"},
	})
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	if b, _ := os.ReadFile(callLog); len(b) != 0 {
		t.Fatalf("kubectl must not be called, got %q", b)
	}
}
