package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func parseOne(t *testing.T, in string) Resource {
	t.Helper()
	rs, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d resources, want 1", len(rs))
	}
	return rs[0]
}

func TestParseTabIndentation(t *testing.T) {
	// Tabs are not valid YAML indentation: the second document must fail by number.
	_, err := Parse([]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n\tname: x\n"))
	if err == nil || !strings.Contains(err.Error(), "document 2") {
		t.Fatalf("err = %v, want a document 2 error", err)
	}
	// A tab after the colon is a valid separator.
	if r := parseOne(t, "apiVersion:\tv1\nkind:\tConfigMap\nmetadata:\n  name:\tx\n  namespace: demo\n"); r.Key() != "v1/ConfigMap/demo/x" {
		t.Fatalf("got %q", r.Key())
	}
}

func TestLoadErrorNamesFileAndDocument(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bad.yaml"), []byte("---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n---\n- a list\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(root, ".")
	if err == nil || !strings.Contains(err.Error(), "bad.yaml: document 2: not a YAML mapping") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseCRLF(t *testing.T) {
	in := "apiVersion: v1\r\nkind: Namespace\r\nmetadata:\r\n  name: demo\r\n---\r\napiVersion: v1\r\nkind: ConfigMap\r\nmetadata:\r\n  name: cfg\r\n  namespace: demo\r\n"
	rs, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[1].Key() != "v1/ConfigMap/demo/cfg" || rs[0].Name != "demo" {
		t.Fatalf("got %+v", rs)
	}
	if !strings.Contains(string(rs[1].Raw), "\r\n") {
		t.Fatal("Raw must keep the original line endings")
	}
}

func TestParseSeparatorInBlockScalar(t *testing.T) {
	in := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n  namespace: demo\ndata:\n  page.md: |\n    title\n    ---\n    body\n"
	r := parseOne(t, in)
	if string(r.Raw) != in {
		t.Fatalf("Raw changed:\n%q\nwant\n%q", r.Raw, in)
	}
}

func TestParseAnchorsAndAliases(t *testing.T) {
	r := parseOne(t, "apiVersion: v1\nkind: ConfigMap\nbase: &meta\n  name: cfg\n  namespace: demo\nmetadata:\n  <<: *meta\n  labels:\n    app: &app web\n    copy: *app\n")
	if r.Key() != "v1/ConfigMap/demo/cfg" {
		t.Fatalf("got %q", r.Key())
	}
}

func TestParseQuotedValues(t *testing.T) {
	r := parseOne(t, "\"apiVersion\": 'apps/v1'\nkind: \"Deployment\"\nmetadata:\n  name: 'it''s-web'\n  namespace: \"demo\"\n")
	if r.Key() != "apps/v1/Deployment/demo/it's-web" {
		t.Fatalf("got %q", r.Key())
	}
}

func TestParseTrailingComment(t *testing.T) {
	r := parseOne(t, "apiVersion: v1 # core\nkind: ConfigMap\nmetadata:\n  name: cfg   # the name\n  namespace: demo #ns\n")
	if r.Key() != "v1/ConfigMap/demo/cfg" {
		t.Fatalf("got %q", r.Key())
	}
}

func TestParseRejectsHiddenContent(t *testing.T) {
	ns := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n"
	for name, in := range map[string]string{
		"content on marker line": "--- " + ns,
		"after end marker":       ns + "...\n" + ns,
		"invalid yaml":           "apiVersion: v1\nkind: [\n",
		"duplicate key":          ns + "kind: ConfigMap\n",
		"metadata is a list":     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  - name: x\n",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
	// A comment after the marker is fine.
	if r := parseOne(t, "--- # first\n"+ns); r.Name != "demo" {
		t.Fatalf("got %+v", r)
	}
}
