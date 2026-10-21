package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rta/gitops/internal/controller"
)

const maxBody = 5 << 20 //5MB

func New(c *controller.Controller, secret string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Status())
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w, c.Status())
	})

	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if secret != "" && !validSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		c.Trigger()
		w.WriteHeader(http.StatusAccepted)
	})

	return mux
}

func writeMetrics(w io.Writer, st controller.Status) {
	metric := func(name, typ, help string, value any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, value)
	}
	metric("gitops_reconcile_total", "counter", "Reconcile cycles started.", st.ReconcileRuns)
	metric("gitops_applies_total", "counter", "Cycles that changed the cluster.", st.Applies)
	metric("gitops_pruned_total", "counter", "Objects deleted by prune.", st.Pruned)
	metric("gitops_resources", "gauge", "Resources in the last applied revision.", st.Resources)

	outOfSync := 0
	if st.Sync == controller.SyncOutOfSync {
		outOfSync = 1
	}
	metric("gitops_out_of_sync", "gauge", "1 if the cluster differs from git and was left alone.", outOfSync)

	var lastSuccess int64
	if !st.LastSuccess.IsZero() {
		lastSuccess = st.LastSuccess.Unix()
	}
	metric("gitops_last_success_timestamp_seconds", "gauge", "Unix time of the last successful cycle (0 = never).", lastSuccess)

	fmt.Fprint(w, "# HELP gitops_reconcile_failures_total Failed cycles by kind.\n# TYPE gitops_reconcile_failures_total counter\n")
	for _, kind := range controller.FailureKinds {
		fmt.Fprintf(w, "gitops_reconcile_failures_total{kind=%q} %d\n", kind, st.Failures[kind])
	}
}

// Github signs the body with HMAC-SHA256 and sends "sha256=<hex>"
func validSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil)) // constat-time compare
}
