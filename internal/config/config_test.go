package config

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// envMap turns a map into a getenv function.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func noFiles(string) ([]byte, error) { return nil, errors.New("no such file") }

var required = []string{"-repo", "https://example.com/r.git", "-namespaces", "demo"}

func TestDefaults(t *testing.T) {
	c, err := Load(required, envMap(nil), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.Branch != "main" || c.Path != "." || c.Interval != time.Minute || c.Listen != ":8080" ||
		!c.DryRun || c.AllowSecrets || c.Kubectl != "kubectl" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if len(c.Namespaces) != 1 || c.Namespaces[0] != "demo" {
		t.Fatalf("namespaces = %v", c.Namespaces)
	}
}

func TestPrecedenceFlagOverEnvOverDefault(t *testing.T) {
	env := envMap(map[string]string{
		"GITOPS_BRANCH":   "from-env",
		"GITOPS_INTERVAL": "10s",
		"GITOPS_PATH":     "env-path",
	})
	c, err := Load(append([]string{"-branch", "from-flag"}, required...), env, noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.Branch != "from-flag" {
		t.Errorf("flag should beat env, got %q", c.Branch)
	}
	if c.Interval != 10*time.Second {
		t.Errorf("env should beat default, got %v", c.Interval)
	}
	if c.Path != "env-path" {
		t.Errorf("path = %q", c.Path)
	}
}

func TestRequiredAllReportedAtOnce(t *testing.T) {
	_, err := Load(nil, envMap(nil), noFiles)
	if err == nil || !strings.Contains(err.Error(), "repo is required") || !strings.Contains(err.Error(), "namespaces is required") {
		t.Fatalf("expected both problems reported, got %v", err)
	}
}

func TestMissingDashIsAnError(t *testing.T) {
	args := append([]string{}, required...)
	args = append(args, "listen", "127.0.0.1:8080") // forgot the dash
	_, err := Load(args, envMap(nil), noFiles)
	if err == nil || !strings.Contains(err.Error(), "forget a leading dash") {
		t.Fatalf("expected a missing-dash error, got %v", err)
	}
}

func TestBadEnvValues(t *testing.T) {
	for _, tc := range []map[string]string{
		{"GITOPS_DRY_RUN": "maybe"},
		{"GITOPS_INTERVAL": "soon"},
		{"GITOPS_ALLOW_PLAINTEXT_SECRETS": "nope"},
	} {
		if _, err := Load(required, envMap(tc), noFiles); err == nil {
			t.Errorf("expected error for %v", tc)
		}
	}
}

func TestSecretFromEnvAndFile(t *testing.T) {
	c, err := Load(required, envMap(map[string]string{"GITOPS_WEBHOOK_SECRET": "s3cret"}), noFiles)
	if err != nil || c.WebhookSecret != "s3cret" {
		t.Fatalf("env secret: %v %q", err, c.WebhookSecret)
	}

	read := func(p string) ([]byte, error) {
		if p != "/etc/gitops/webhook" {
			return nil, errors.New("wrong path")
		}
		return []byte("from-file\n"), nil // trailing newline must be trimmed
	}
	c, err = Load(required, envMap(map[string]string{"GITOPS_WEBHOOK_SECRET_FILE": "/etc/gitops/webhook"}), read)
	if err != nil || c.WebhookSecret != "from-file" {
		t.Fatalf("file secret: %v %q", err, c.WebhookSecret)
	}
}

func TestSecretBothSetOrUnreadable(t *testing.T) {
	both := envMap(map[string]string{"GITOPS_WEBHOOK_SECRET": "a", "GITOPS_WEBHOOK_SECRET_FILE": "/x"})
	if _, err := Load(required, both, noFiles); err == nil {
		t.Error("expected error when both secret forms are set")
	}
	missing := envMap(map[string]string{"GITOPS_WEBHOOK_SECRET_FILE": "/does/not/exist"})
	if _, err := Load(required, missing, noFiles); err == nil {
		t.Error("expected error for an unreadable secret file")
	}
}

func TestLoggingNeverLeaksSecrets(t *testing.T) {
	c, err := Load(
		[]string{"-repo", "https://user:hunter2@example.com/r.git", "-namespaces", "demo"},
		envMap(map[string]string{"GITOPS_WEBHOOK_SECRET": "topsecretvalue"}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config", "cfg", c)
	out := buf.String()
	if strings.Contains(out, "topsecretvalue") || strings.Contains(out, "hunter2") {
		t.Fatalf("secret leaked into log output: %s", out)
	}
	if !strings.Contains(out, "webhook_secret_set=true") {
		t.Fatalf("expected the set/unset marker in: %s", out)
	}
}

func TestGitToken(t *testing.T) {
	https := []string{"-repo", "https://example.com/r.git", "-namespaces", "demo"}
	c, err := Load(https, envMap(map[string]string{"GITOPS_GIT_TOKEN": "ghp_abc"}), noFiles)
	if err != nil || c.GitToken != "ghp_abc" {
		t.Fatalf("token from env: %v %q", err, c.GitToken)
	}

	// A token with a non-https repo is an error, not silently ignored.
	ssh := []string{"-repo", "git@github.com:o/r.git", "-namespaces", "demo"}
	if _, err := Load(ssh, envMap(map[string]string{"GITOPS_GIT_TOKEN": "x"}), noFiles); err == nil ||
		!strings.Contains(err.Error(), "not https") {
		t.Fatalf("expected a not-https error, got %v", err)
	}

	// The token is never logged.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config", "cfg", c)
	if strings.Contains(buf.String(), "ghp_abc") || !strings.Contains(buf.String(), "git_token_set=true") {
		t.Fatalf("unexpected log output: %s", buf.String())
	}
}

func TestPruneDefaultsAndValidation(t *testing.T) {
	c, err := Load(required, envMap(nil), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prune || c.PruneMax != 10 || c.Owner != "gitops" ||
		strings.Join(c.PruneKinds, ",") != "deployments.apps,services,configmaps" {
		t.Fatalf("unexpected prune defaults: %+v", c)
	}

	on := append([]string{"-prune", "-prune-max", "3", "-owner", "team-a"}, required...)
	c, err = Load(on, envMap(nil), noFiles)
	if err != nil || !c.Prune || c.PruneMax != 3 || c.Owner != "team-a" {
		t.Fatalf("flags not applied: %v %+v", err, c)
	}

	bad := [][]string{
		append([]string{"-owner", "has spaces"}, required...),
		append([]string{"-prune-max", "0"}, required...),
		append([]string{"-prune", "-prune-kinds", ","}, required...),
	}
	for _, args := range bad {
		if _, err := Load(args, envMap(nil), noFiles); err == nil {
			t.Errorf("expected an error for %v", args)
		}
	}
	if _, err := Load(required, envMap(map[string]string{"GITOPS_PRUNE_MAX": "lots"}), noFiles); err == nil {
		t.Error("expected an error for a non-numeric GITOPS_PRUNE_MAX")
	}
}
