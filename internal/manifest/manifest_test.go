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
