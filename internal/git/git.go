package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A struct groups related fields, like a class without methods inside.
type Repo struct {
	URL    string
	Branch string
	Dir    string
}

// Sync returns the commit SHA it ended up on, or an error.
func (r Repo) Sync(ctx context.Context) (string, error) {
	if r.URL == "" || r.Branch == "" || r.Dir == "" {
		return "", errors.New("git: URL, Branch and Dir are required")
	}

	// Does Dir/.git exist? If not, this is the first run, so clone.
	if _, err := os.Stat(filepath.Join(r.Dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
			return "", err
		}
		if _, err := run(ctx, "", "clone", "--branch", r.Branch, "--single-branch", r.URL, r.Dir); err != nil {
			return "", err
		}
	} else {
		// Already cloned: fetch, then force the working copy to match the remote.
		// Git is the source of truth, so local edits get discarded on purpose.
		if _, err := run(ctx, r.Dir, "fetch", "--prune", "origin", r.Branch); err != nil {
			return "", err
		}
		if _, err := run(ctx, r.Dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
			return "", err
		}
		if _, err := run(ctx, r.Dir, "clean", "-fdx"); err != nil {
			return "", err
		}
	}
	return r.Head(ctx)
}

func (r Repo) Head(ctx context.Context) (string, error) {
	out, err := run(ctx, r.Dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// run executes `git <args>` in dir and returns stdout.
// Lowercase name means it is private to this package.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // never hang on a password prompt
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// %w wraps the original error so callers can still inspect it.
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}