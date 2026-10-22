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
