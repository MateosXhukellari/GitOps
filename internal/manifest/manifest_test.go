package manifest

import "testing"

func TestParseMultiDoc(t *testing.T) {
	in := []byte(`---
apiVersion: v1
kind: Namespace
metadata:
  name: demo
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: demo
  labels:
    name: not-the-name
`)

	rs, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d resources, want 2", len(rs))
	}
	if rs[1].Key() != "apps/v1/Deployment/demo/web" {
		t.Errorf("got %q", rs[1].Key())
	}
}

// exact bytes of DemoKubernetes de9ce33:manifests/scratch.yaml (uniform 3-space indent, no trailing newline)
const indentedScratch = "   apiVersion: v1\n   kind: ConfigMap\n   metadata:\n     name: scratch\n     namespace: demo\n   data:\n     hello: world"

func TestParseIndentedDoc(t *testing.T) {
	rs, err := Parse([]byte(indentedScratch))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Key() != "v1/ConfigMap/demo/scratch" {
		t.Fatalf("got %+v", rs)
	}
}

func TestParseUnidentifiedDocErrors(t *testing.T) {
	for name, in := range map[string]string{
		"no kind":       "apiVersion: v1\nmetadata:\n  name: x\n",
		"no apiVersion": "kind: ConfigMap\nmetadata:\n  name: x\n",
		"no name":       "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  namespace: demo\n",
		"tab indent":    "\tapiVersion: v1\n\tkind: ConfigMap\n",
		"garbage":       "just some text\n",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestParseSkipsEmptyDocs(t *testing.T) {
	rs, err := Parse([]byte("---\n# only a comment\n\n---\n   \n---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d resources, want 1", len(rs))
	}
}
