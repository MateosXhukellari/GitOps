package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

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

	Prune      bool
	PruneKinds []string // which kinds may be pruned, e.g. deployments.apps
	PruneMax   int      // refuse to delete more than this many objects per cycle
	Owner      string   // label value marking objects this instance manages

	WebhookSecret string
	GitToken      string
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
		slog.Bool("prune", c.Prune),
		slog.Any("prune_kinds", c.PruneKinds),
		slog.Int("prune_max", c.PruneMax),
		slog.String("owner", c.Owner),
		slog.Bool("webhook_secret_set", c.WebhookSecret != ""),
		slog.Bool("git_token_set", c.GitToken != ""),
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

	defPrune, err := envBool(getenv, "GITOPS_PRUNE", false)
	if err != nil {
		return Config{}, err
	}
	defPruneMax, err := envInt(getenv, "GITOPS_PRUNE_MAX", 10)
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
	fs.BoolVar(&c.Prune, "prune", defPrune, "delete objects removed from git; only ones this tool labelled (env GITOPS_PRUNE)")
	pruneKinds := fs.String("prune-kinds", env("GITOPS_PRUNE_KINDS", "deployments.apps,services,configmaps"), "comma-separated kinds that may be pruned (env GITOPS_PRUNE_KINDS)")
	fs.IntVar(&c.PruneMax, "prune-max", defPruneMax, "refuse to delete more than this many objects in one cycle (env GITOPS_PRUNE_MAX)")
	fs.StringVar(&c.Owner, "owner", env("GITOPS_OWNER", "gitops"), "label value marking objects this instance manages; use a different one per instance (env GITOPS_OWNER)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("config: unexpected argument %q (did you forget a leading dash?)", fs.Arg(0))
	}
	c.Namespaces = splitList(*namespaces)
	c.PruneKinds = splitList(*pruneKinds)

	c.WebhookSecret, err = secret(getenv, readFile, "GITOPS_WEBHOOK_SECRET")
	if err != nil {
		return Config{}, err
	}

	c.GitToken, err = secret(getenv, readFile, "GITOPS_GIT_TOKEN")
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
	if c.GitToken != "" && !strings.HasPrefix(c.RepoURL, "https://") {
		problems = append(problems, "a git token is set but the repo URL is not https://, so it would never be sent")
	}
	if !labelValue.MatchString(c.Owner) {
		problems = append(problems, fmt.Sprintf("owner %q is not a valid label value (letters, digits, - _ . ; max 63)", c.Owner))
	}
	if c.Prune && len(c.PruneKinds) == 0 {
		problems = append(problems, "prune is on but prune-kinds is empty")
	}
	if c.PruneMax <= 0 {
		problems = append(problems, "prune-max must be positive")
	}
	if len(problems) > 0 {
		return errors.New("config: " + strings.Join(problems, "; "))
	}
	return nil
}

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
		return strings.TrimRight(string(b), "\r\n"), nil
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

func envInt(getenv func(string) string, key string, def int) (int, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a whole number", key, v)
	}
	return n, nil
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

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if strings.Contains(raw, "://") {
			return "<unparseable url>"
		}
		return raw
	}
	return u.Redacted()
}
