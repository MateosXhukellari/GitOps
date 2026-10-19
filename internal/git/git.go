package git

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repo struct {
	URL    string
	Branch string
	Dir    string

	// Token is an optional HTTPS access token. It is handed to git through
	// environment variables, never in the URL or the command line, and is
	// scrubbed from errors.
	Token string
}

func (r Repo) Sync(ctx context.Context) (string, error) {
	if r.URL == "" || r.Branch == "" || r.Dir == "" {
		return "", errors.New("git: URL, Branch and Dir are required")
	}

	if _, err := os.Stat(filepath.Join(r.Dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
			return "", err
		}
		if _, err := r.run(ctx, "", "clone", "--branch", r.Branch, "--single-branch", r.URL, r.Dir); err != nil {
			return "", err
		}
	} else {
		if _, err := r.run(ctx, r.Dir, "fetch", "--prune", "origin", r.Branch); err != nil {
			return "", err
		}
		if _, err := r.run(ctx, r.Dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return "", err
		}
		if _, err := r.run(ctx, r.Dir, "clean", "-fdx"); err != nil {
			return "", err
		}
	}
	return r.Head(ctx)
}

func (r Repo) Head(ctx context.Context) (string, error) {
	out, err := r.run(ctx, r.Dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// authEnv makes git send the token as HTTP basic auth. It uses
// GIT_CONFIG_COUNT/KEY/VALUE instead of `git -c ...` so the token is not in the
// process's argument list, and scopes the header to this repo's host so it
// cannot leak to another site.
func (r Repo) authEnv() []string {
	if r.Token == "" {
		return nil
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil // never send a token over anything but https
	}
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + r.Token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		fmt.Sprintf("GIT_CONFIG_KEY_0=http.https://%s/.extraHeader", u.Host),
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + cred,
	}
}

// redact removes the token (raw and base64 forms) from text such as git's
// stderr, in case a server or proxy echoes it back.
func (r Repo) redact(s string) string {
	if r.Token == "" {
		return s
	}
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + r.Token))
	s = strings.ReplaceAll(s, cred, "***")
	return strings.ReplaceAll(s, r.Token, "***")
}

func (r Repo) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Never block waiting for a credentials prompt inside a controller.
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), r.authEnv()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := fmt.Sprintf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
		return "", errors.New(r.redact(msg))
	}
	return stdout.String(), nil
}
