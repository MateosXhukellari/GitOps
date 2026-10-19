package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	RepoURL      string
	Branch       string
	Path         string
	Workdir      string
	Interval     time.Duration
	Listen       string
	DryRun       bool
	Namespaces   []string
	AllowSecrets bool
	Kubectl      string

	WebhookSecret string // env or file only, never logged
}

func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("repo", redactURL(c.RepoURL)),
		slog.String("branch", c.Branch),
		slog.String("path", c.Path),
		slog.Duration("interval", c.Interval),
		slog.String("listen", c.Listen),
		slog.Bool("dry_run", c.DryRun),
		slog.Any("namespaces", c.Namespaces),
		slog.Bool("allow_plaintext_secrets", c.AllowSecrets),
		slog.Bool("webhook_secret_set", c.WebhookSecret != ""),
	)
}

func Load(args []string, getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	var c Config

	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	defDryRun, err := envBool(getenv, "GITOPS_DRY_RUN", true)
	if err != nil {
		return Config{}, err
	}
	defAllow, err := envBool(getenv, "GITOPS_ALLOW_PLAINTEXT_SECRETS", false)
	if err != nil {
		return Config{}, err
	}
	defInterval, err := envDuration(getenv, "GITOPS_INTERVAL", time.Minute)
	if err != nil {
		return Config{}, err
	}

	fs := flag.NewFlagSet("gitops", flag.ContinueOnError)
	fs.StringVar(&c.RepoURL, "repo", env("GITOPS_REPO", ""), "git repository URL (env GITOPS_REPO)")
	fs.StringVar(&c.Branch, "branch", env("GITOPS_BRANCH", "main"), "branch to track (env GITOPS_BRANCH)")
	fs.StringVar(&c.Path, "path", env("GITOPS_PATH", "."), "directory in the repo containing manifests (env GITOPS_PATH)")
	fs.StringVar(&c.Workdir, "workdir", env("GITOPS_WORKDIR", "/tmp/gitops/checkout"), "local checkout directory (env GITOPS_WORKDIR)")
	fs.DurationVar(&c.Interval, "interval", defInterval, "poll interval (env GITOPS_INTERVAL)")
	fs.StringVar(&c.Listen, "listen", env("GITOPS_LISTEN", ":8080"), "HTTP listen address (env GITOPS_LISTEN)")
	fs.BoolVar(&c.DryRun, "dry-run", defDryRun, "log diffs but never apply; use -dry-run=false to go live (env GITOPS_DRY_RUN)")
	namespaces := fs.String("namespaces", env("GITOPS_NAMESPACES", ""), "comma-separated namespaces gitops may manage (env GITOPS_NAMESPACES)")
	fs.BoolVar(&c.AllowSecrets, "allow-plaintext-secrets", defAllow, "permit kind: Secret manifests (env GITOPS_ALLOW_PLAINTEXT_SECRETS)")
	fs.StringVar(&c.Kubectl, "kubectl", env("GITOPS_KUBECTL", "kubectl"), "kubectl binary: a name in PATH or a full path (env GITOPS_KUBECTL)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("config: unexpected argument %q(fogot leading dash)", fs.Arg(0))
	}
	c.Namespaces = splitList(*namespaces)

	c.WebhookSecret, err = secret(getenv, readFile, "GITOPS_WEBHOOK_SECRET")
	if err != nil {
		return Config{}, err
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	var problems []string
	if c.RepoURL == "" {
		problems = append(problems, "repo is required (-repo or GITOPS_REPO)")
	}
	if len(c.Namespaces) == 0 {
		problems = append(problems, "namespaces is required (-namespaces or GITOPS_NAMESPACES)")
	}
	if c.Interval <= 0 {
		problems = append(problems, "interval must be positive")
	}
	if len(problems) > 0 {
		return errors.New("config: " + strings.Join(problems, "; "))
	}
	return nil
}

// secret reads NAME from the environment, or NAME_FILE from a file.
func secret(getenv func(string) string, readFile func(string) ([]byte, error), name string) (string, error) {
	val, file := getenv(name), getenv(name+"_FILE")
	switch {
	case val != "" && file != "":
		return "", fmt.Errorf("config: set only one of %s and %s_FILE", name, name)
	case file != "":
		b, err := readFile(file)
		if err != nil {
			return "", fmt.Errorf("config: reading %s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil // editors add a trailing newline
	}
	return val, nil
}

func envBool(getenv func(string) string, key string, def bool) (bool, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: %s=%q is not a boolean", key, v)
	}
	return b, nil
}

func envDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a duration (try 30s or 2m)", key, v)
	}
	return d, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// redactURL hides a password in https://user:token@host/repo.git.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if strings.Contains(raw, "://") {
			return "<unparseable url>" // might contain credentials
		}
		return raw // e.g. git@github.com:org/repo.git has no password in it
	}
	return u.Redacted()
}
