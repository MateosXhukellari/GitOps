package git

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthEnvOnlyForHTTPS(t *testing.T) {
	for _, u := range []string{
		"http://github.com/a/b.git",
		"git@github.com:a/b.git",
		"ssh://git@github.com/a/b.git",
		"/srv/repo",
		"file:///srv/repo",
	} {
		if env := (Repo{URL: u, Token: "tok"}).authEnv(); env != nil {
			t.Errorf("%s: token env %q, want none", u, env)
		}
	}
	if env := (Repo{URL: "https://github.com/a/b.git"}).authEnv(); env != nil {
		t.Errorf("no token: env %q, want none", env)
	}

	env := Repo{URL: "https://github.com/a/b.git", Token: "tok"}.authEnv()
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	want := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + cred,
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env = %q, want %q", env, want)
	}
}

func TestRedact(t *testing.T) {
	r := Repo{Token: "s3cr3t-token"}
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:s3cr3t-token"))
	got := r.redact("raw s3cr3t-token and basic " + cred + " end")
	if strings.Contains(got, "s3cr3t-token") || strings.Contains(got, cred) {
		t.Fatalf("token survived redaction: %q", got)
	}
	if got != "raw *** and basic *** end" {
		t.Fatalf("got %q", got)
	}
	if (Repo{}).redact("x") != "x" {
		t.Fatal("no token must leave text unchanged")
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, src, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(src, "f.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, src, "add", ".")
	gitIn(t, src, "commit", "-q", "-m", content)
	return gitIn(t, src, "rev-parse", "HEAD")
}

func TestSyncCloneFetchReset(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	rev1 := commit(t, src, "one")

	r := Repo{URL: src, Branch: "main", Dir: filepath.Join(t.TempDir(), "nested", "co")}
	ctx := context.Background()

	got, err := r.Sync(ctx) // clone
	if err != nil || got != rev1 {
		t.Fatalf("clone: rev=%s err=%v, want %s", got, err, rev1)
	}

	rev2 := commit(t, src, "two")
	// Local edits and stray files in the checkout must be discarded.
	f := filepath.Join(r.Dir, "f.yaml")
	if err := os.WriteFile(f, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(r.Dir, "stray.yaml")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err = r.Sync(ctx) // fetch + reset + clean
	if err != nil || got != rev2 {
		t.Fatalf("fetch: rev=%s err=%v, want %s", got, err, rev2)
	}
	if b, _ := os.ReadFile(f); string(b) != "two" {
		t.Fatalf("checkout content = %q, want %q", b, "two")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("stray file survived: %v", err)
	}
}

func TestSyncRequiresFields(t *testing.T) {
	if _, err := (Repo{URL: "x"}).Sync(context.Background()); err == nil {
		t.Fatal("want error for missing Branch and Dir")
	}
}
