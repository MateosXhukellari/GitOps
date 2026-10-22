package policy

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/MateosXhukellari/GitOps/internal/manifest"
)

const renderedSecret = `apiVersion: v1
kind: Secret
metadata:
  name: db-5h2k8f
  namespace: prod
data:
  password: bm90LWEtcmVhbC1zZWNyZXQ=
`

func checkKustomized(t *testing.T, l manifest.Loader) error {
	t.Helper()
	rs, _, err := l.Load(context.Background(), "../manifest/testdata/kustomize", "k8s/overlays/secretgen")
	if err != nil {
		t.Fatal(err)
	}
	return New([]string{"prod"}).Check(rs)
}

func TestRenderedSecretIsRejected(t *testing.T) {
	fake := func(context.Context, string) ([]byte, error) { return []byte(renderedSecret), nil }
	err := checkKustomized(t, manifest.Loader{Render: fake})
	if err == nil || !strings.Contains(err.Error(), `Secret "db-5h2k8f" must not be stored in git in plaintext`) {
		t.Fatalf("err = %v", err)
	}
}

func TestSecretGeneratorIsRejectedWithRealKustomize(t *testing.T) {
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("kubectl is required in CI for the real kustomize render tests")
		}
		t.Skip("kubectl not on PATH: skipping the real `kubectl kustomize` render test")
	}
	err = checkKustomized(t, manifest.Loader{Render: manifest.Kubectl(bin)})
	if err == nil || !strings.Contains(err.Error(), "Secret \"db-") || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("err = %v", err)
	}
}
