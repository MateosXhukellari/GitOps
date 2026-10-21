package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MateosXhukellari/GitOps/internal/controller"
	"github.com/MateosXhukellari/GitOps/internal/git"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newServer(secret string) http.Handler {
	c := controller.New(git.Repo{}, ".", 0, nil, nil)
	return New(c, secret)
}

func do(h http.Handler, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	rec := do(newServer(""), "GET", "/healthz", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestStatusReturnsJSON(t *testing.T) {
	rec := do(newServer(""), "GET", "/status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var st controller.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestWebhook(t *testing.T) {
	const secret, body = "s3cret", `{"ref":"refs/heads/main"}`

	tests := []struct {
		name   string
		secret string
		sig    string
		want   int
	}{
		{"no secret configured accepts unsigned", "", "", http.StatusAccepted},
		{"missing signature", secret, "", http.StatusUnauthorized},
		{"wrong prefix", secret, "sha1=abcd", http.StatusUnauthorized},
		{"not hex", secret, "sha256=zzzz", http.StatusUnauthorized},
		{"signed with wrong secret", secret, sign("other", body), http.StatusUnauthorized},
		{"valid signature", secret, sign(secret, body), http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(newServer(tt.secret), "POST", "/webhook", body,
				map[string]string{"X-Hub-Signature-256": tt.sig})
			if rec.Code != tt.want {
				t.Fatalf("got %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestWebhookSignatureCoversBody(t *testing.T) {
	// A valid signature for one body must not authorize a different body.
	sig := sign("s3cret", `{"a":1}`)
	rec := do(newServer("s3cret"), "POST", "/webhook", `{"a":2}`,
		map[string]string{"X-Hub-Signature-256": sig})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestWebhookBodyTooLarge(t *testing.T) {
	rec := do(newServer(""), "POST", "/webhook", strings.Repeat("x", maxBody+1), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newServer("")
	for _, tc := range []struct{ method, path string }{
		{"GET", "/webhook"},
		{"POST", "/healthz"},
		{"POST", "/status"},
	} {
		if rec := do(h, tc.method, tc.path, "", nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: got %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	rec := do(newServer(""), "GET", "/metrics", "", nil)
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Fatalf("Content-Type = %q", ct)
	}
	for _, kind := range controller.FailureKinds {
		line := `gitops_reconcile_failures_total{kind="` + kind + `"} 0` + "\n"
		if !strings.Contains(rec.Body.String(), line) {
			t.Errorf("missing zero-filled %q", line)
		}
	}
}

func TestWriteMetricsValues(t *testing.T) {
	var b strings.Builder
	writeMetrics(&b, controller.Status{
		ReconcileRuns: 7, Applies: 3, Pruned: 2, Resources: 5,
		Sync:        controller.SyncOutOfSync,
		LastSuccess: time.Unix(1700000000, 0),
		Failures:    map[string]int{controller.KindPolicy: 4},
	})
	for _, line := range []string{
		"gitops_reconcile_total 7\n",
		"gitops_applies_total 3\n",
		"gitops_pruned_total 2\n",
		"gitops_resources 5\n",
		"gitops_out_of_sync 1\n",
		"gitops_last_success_timestamp_seconds 1700000000\n",
		`gitops_reconcile_failures_total{kind="policy"} 4` + "\n",
		`gitops_reconcile_failures_total{kind="git"} 0` + "\n",
	} {
		if !strings.Contains(b.String(), line) {
			t.Errorf("missing %q in:\n%s", line, b.String())
		}
	}

	b.Reset()
	writeMetrics(&b, controller.Status{Sync: controller.SyncSynced})
	for _, line := range []string{"gitops_out_of_sync 0\n", "gitops_last_success_timestamp_seconds 0\n"} {
		if !strings.Contains(b.String(), line) {
			t.Errorf("missing %q for a never-succeeded Synced status", line)
		}
	}
}
