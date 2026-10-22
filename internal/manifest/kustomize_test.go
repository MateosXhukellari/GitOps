package manifest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const kustomizeRoot = "testdata/kustomize"

// realKubectl returns kubectl from PATH. CI must have it; locally the
// real-render tests are skipped without it.
func realKubectl(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("kubectl is required in CI for the real kustomize render tests")
		}
		t.Skip("kubectl not on PATH: skipping the real `kubectl kustomize` render test")
	}
	return bin
}

func byKey(rs []Resource) map[string]Resource {
	m := map[string]Resource{}
	for _, r := range rs {
		m[r.Kind+"/"+r.Namespace+"/"+r.Name] = r
	}
	return m
}

func TestKustomizeRendersOverlayWithBasesOutsidePath(t *testing.T) {
	l := Loader{Render: Kubectl(realKubectl(t))}
	start := time.Now()
	rs, source, err := l.Load(context.Background(), kustomizeRoot, "k8s/overlays/prod")
	t.Logf("render took %s", time.Since(start))
	if err != nil {
		t.Fatal(err)
	}
	if source != SourceKustomize {
		t.Fatalf("source = %q", source)
	}
	got := byKey(rs)
	if len(rs) != 3 || got["Deployment/prod/web"].Name == "" || got["Service/prod/web"].Name == "" || got["Deployment/prod/api"].Name == "" {
		t.Fatalf("want web Deployment+Service and api Deployment in prod, got %v", got)
	}
	web := string(got["Deployment/prod/web"].Raw)
	if !strings.Contains(web, "replicas: 3") || !strings.Contains(web, "nginx:1.29") {
		t.Fatalf("overlay patch or image not applied:\n%s", web)
	}
	if _, ok := got["ConfigMap/prod/stray"]; ok {
		t.Fatal("a YAML file not listed in the kustomization was applied")
	}
	if f := got["Service/prod/web"].File; f != "k8s/overlays/prod (kustomize)" {
		t.Fatalf("File = %q", f)
	}
}

func TestKustomizeSameNamesInAnotherOverlay(t *testing.T) {
	l := Loader{Render: Kubectl(realKubectl(t))}
	rs, _, err := l.Load(context.Background(), kustomizeRoot, "k8s/overlays/staging")
	if err != nil {
		t.Fatal(err)
	}
	web := string(byKey(rs)["Deployment/staging/web"].Raw)
	if len(rs) != 3 || !strings.Contains(web, "replicas: 2") || !strings.Contains(web, "nginx:1.28") {
		t.Fatalf("staging render wrong (%d resources):\n%s", len(rs), web)
	}
}

func TestKustomizeSecretGeneratorRendersASecret(t *testing.T) {
	l := Loader{Render: Kubectl(realKubectl(t))}
	rs, _, err := l.Load(context.Background(), kustomizeRoot, "k8s/overlays/secretgen")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Kind == "Secret" {
			return // policy_test proves policy rejects it
		}
	}
	t.Fatalf("no Secret rendered: %v", byKey(rs))
}

// tree writes files (path -> content) under a temp root.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const cm = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  namespace: demo\n"

// fakeRender returns out/err and records whether it was called.
func fakeRender(out string, err error, called *bool) Renderer {
	return func(context.Context, string) ([]byte, error) {
		*called = true
		return []byte(out), err
	}
}

func TestKustomizeRenderFailure(t *testing.T) {
	root := tree(t, map[string]string{"app/kustomization.yaml": "resources: []\n"})
	var called bool
	_, _, err := Loader{Render: fakeRender("", errors.New("kubectl kustomize: exit 1: accumulating resources: boom"), &called)}.
		Load(context.Background(), root, "app")
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "kustomize render of app") {
		t.Fatalf("err = %v", err)
	}
}

func TestKustomizeRenderTimeout(t *testing.T) {
	root := tree(t, map[string]string{"app/kustomization.yaml": "resources: []\n"})
	hang := func(ctx context.Context, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, _, err := Loader{Render: hang, Timeout: 20 * time.Millisecond}.Load(context.Background(), root, "app")
	if err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestKustomizeEmptyRenderIsAnError(t *testing.T) {
	root := tree(t, map[string]string{"app/kustomization.yaml": "resources: []\n"})
	for _, out := range []string{"", "# nothing\n---\n"} {
		var called bool
		rs, _, err := Loader{Render: fakeRender(out, nil, &called)}.Load(context.Background(), root, "app")
		if err == nil || rs != nil || !strings.Contains(err.Error(), "produced no resources") {
			t.Fatalf("render %q: rs=%v err=%v", out, rs, err)
		}
	}
}

func TestKustomizeStaticChecksRejectBeforeRendering(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"remote URL base": {map[string]string{
			"o/kustomization.yaml": "resources:\n  - https://github.com/org/repo//base?ref=v1\n"}, `remote resource "https://`},
		"git@ base": {map[string]string{
			"o/kustomization.yaml": "bases:\n  - git@github.com:org/repo.git\n"}, `remote resource "git@`},
		"github.com shorthand": {map[string]string{
			"o/kustomization.yaml": "resources:\n  - github.com/org/repo/base?ref=v1\n"}, "remote resources are not allowed"},
		"remote patch file": {map[string]string{
			"o/kustomization.yaml": "patches:\n  - path: https://example.com/p.yaml\n"}, `remote reference "https://`},
		"remote component in a base": {map[string]string{
			"o/kustomization.yaml": "resources:\n  - ../b\n",
			"b/kustomization.yaml": "components:\n  - https://example.com/c\n"}, "b/kustomization.yaml: remote resource"},
		"helmCharts": {map[string]string{
			"o/kustomization.yaml": "helmCharts:\n  - name: x\n    repo: https://charts.example.com\n"}, `"helmCharts" is not allowed`},
		"generators plugin": {map[string]string{
			"o/kustomization.yaml": "generators:\n  - gen.yaml\n"}, `"generators" is not allowed`},
		"transformers plugin": {map[string]string{
			"o/kustomization.yaml": "transformers:\n  - t.yaml\n"}, `"transformers" is not allowed`},
		"cycle": {map[string]string{
			"o/kustomization.yaml": "resources:\n  - ../b\n",
			"b/kustomization.yaml": "resources:\n  - ../o\n"}, "kustomization cycle at o"},
		"dir without kustomization": {map[string]string{
			"o/kustomization.yaml": "resources:\n  - ../plain\n",
			"plain/cm.yaml":        cm}, "plain: referenced directory has no kustomization file"},
		"wrong kind": {map[string]string{
			"o/kustomization.yaml": "kind: Deployment\n"}, `unexpected kind "Deployment"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := tree(t, tc.files)
			var called bool
			_, _, err := Loader{Render: fakeRender(cm, nil, &called)}.Load(context.Background(), root, "o")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if called {
				t.Fatal("kubectl must not run when the static check fails")
			}
		})
	}
}

func TestKustomizeOutsideCheckoutIsRejected(t *testing.T) {
	outside := tree(t, map[string]string{"kustomization.yaml": "resources: []\n"})
	root := tree(t, map[string]string{})
	if err := os.MkdirAll(filepath.Join(root, "o"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(filepath.Join(root, "o"), outside)
	k := "resources:\n  - " + rel + "\n"
	if err := os.WriteFile(filepath.Join(root, "o", "kustomization.yaml"), []byte(k), 0o644); err != nil {
		t.Fatal(err)
	}
	var called bool
	_, _, err := Loader{Render: fakeRender(cm, nil, &called)}.Load(context.Background(), root, "o")
	if err == nil || !strings.Contains(err.Error(), "outside the repository checkout") || called {
		t.Fatalf("err = %v called=%v", err, called)
	}
}

func TestKustomizeDeepButAcyclicIsFine(t *testing.T) {
	files := map[string]string{
		"o/kustomization.yml":       "resources:\n  - ../b\n  - ../c\n",
		"b/Kustomization":           "resources:\n  - ../shared\n",
		"c/kustomization.yaml":      "resources:\n  - ../shared\n",
		"shared/kustomization.yaml": "resources:\n  - cm.yaml\n",
		"shared/cm.yaml":            cm,
	}
	var called bool
	rs, source, err := Loader{Render: fakeRender(cm, nil, &called)}.Load(context.Background(), tree(t, files), "o")
	if err != nil || source != SourceKustomize || len(rs) != 1 || !called {
		t.Fatalf("rs=%v source=%q err=%v called=%v", rs, source, err, called)
	}
}

func TestSourceSelection(t *testing.T) {
	root := tree(t, map[string]string{
		"plain/cm.yaml":           cm,
		"kust/kustomization.yaml": "resources: []\n",
	})
	var called bool
	l := Loader{Render: fakeRender(cm, nil, &called)}
	if _, source, err := l.Load(context.Background(), root, "plain"); err != nil || source != SourcePlain || called {
		t.Fatalf("auto on a plain dir: source=%q err=%v called=%v", source, err, called)
	}
	l.Source = SourceKustomize
	if _, _, err := l.Load(context.Background(), root, "plain"); err == nil || !strings.Contains(err.Error(), "has no kustomization file") {
		t.Fatalf("forced kustomize without a kustomization: err = %v", err)
	}
	l.Source = SourceAuto
	if _, source, err := l.Load(context.Background(), root, "kust"); err != nil || source != SourceKustomize {
		t.Fatalf("auto on a kustomization: source=%q err=%v", source, err)
	}
	if _, _, err := l.Load(context.Background(), root, "../x"); err == nil {
		t.Fatal("a path escaping the root must be rejected")
	}
}

func TestKustomizeDuplicateRenderedResource(t *testing.T) {
	root := tree(t, map[string]string{"app/kustomization.yaml": "resources: []\n"})
	var called bool
	_, _, err := Loader{Render: fakeRender(cm+"---\n"+cm, nil, &called)}.Load(context.Background(), root, "app")
	if err == nil || !strings.Contains(err.Error(), "duplicate resource") {
		t.Fatalf("err = %v", err)
	}
}
