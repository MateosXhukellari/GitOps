package policy

import (
	"strings"
	"testing"

	"github.com/rta/gitops/internal/manifest"
)

func res(kind, ns, name string) manifest.Resource {
	return manifest.Resource{Kind: kind, Namespace: ns, Name: name, File: name + ".yaml"}
}

func TestNewTrimsAndSkipsEmpty(t *testing.T) {
	p := New([]string{" demo ", "", "  ", "staging"})
	if p.Empty() {
		t.Fatal("policy should not be empty")
	}
	if len(p.allowed) != 2 || !p.allowed["demo"] || !p.allowed["staging"] {
		t.Fatalf("allowed = %v", p.allowed)
	}
	if !New([]string{""}).Empty() || !New(nil).Empty() {
		t.Fatal("blank input should give an empty policy")
	}
}

func TestCheck(t *testing.T) {
	p := New([]string{"demo"})

	tests := []struct {
		name    string
		r       manifest.Resource
		wantErr string // empty means allowed
	}{
		{"namespaced in allowed ns", res("ConfigMap", "demo", "cfg"), ""},
		{"allowed Namespace object", res("Namespace", "", "demo"), ""},
		{"namespaced in other ns", res("ConfigMap", "kube-system", "evil"), `in namespace "kube-system", which is not allowed`},
		{"other Namespace object", res("Namespace", "", "kube-system"), `Namespace "kube-system" is not allowed`},
		{"cluster-scoped kind", res("ClusterRole", "", "admin"), "cluster-scope kinds are not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Check([]manifest.Resource{tt.r})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckReportsEveryViolation(t *testing.T) {
	p := New([]string{"demo"})
	err := p.Check([]manifest.Resource{
		res("ConfigMap", "demo", "ok"),
		res("ConfigMap", "kube-system", "a"),
		res("ClusterRole", "", "b"),
	})
	if err == nil {
		t.Fatal("expected violations")
	}
	msg := err.Error()
	if !strings.Contains(msg, "2 violation(s)") || !strings.Contains(msg, "a.yaml") || !strings.Contains(msg, "b.yaml") {
		t.Fatalf("got %q", msg)
	}
}

func TestEmptyPolicyRejectsEverything(t *testing.T) {
	if err := New(nil).Check([]manifest.Resource{res("ConfigMap", "demo", "cfg")}); err == nil {
		t.Fatal("empty policy must not allow anything")
	}
}
