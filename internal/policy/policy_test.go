package policy

import (
	"strings"
	"testing"

	"github.com/MateosXhukellari/GitOps/internal/manifest"
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

func TestSecretsRejectedByDefault(t *testing.T) {
	p := New([]string{"demo"})
	rs := []manifest.Resource{
		res("Secret", "demo", "db-password"),
		res("SealedSecret", "demo", "db-password"), // encrypted form is fine
	}

	err := p.Check(rs)
	if err == nil || !strings.Contains(err.Error(), "1 violation") || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("expected exactly one plaintext-secret violation, got %v", err)
	}

	// Explicit opt-in allows it.
	p.AllowSecrets = true
	if err := p.Check(rs); err != nil {
		t.Fatalf("AllowSecrets should permit Secret, got %v", err)
	}

	// A Secret in a forbidden namespace reports both problems.
	p.AllowSecrets = false
	err = p.Check([]manifest.Resource{res("Secret", "kube-system", "x")})
	if err == nil || !strings.Contains(err.Error(), "2 violation") {
		t.Fatalf("expected 2 violations, got %v", err)
	}
}

func TestSealedSecretFollowsNamespaceAllowlist(t *testing.T) {
	p := New([]string{"demo"})
	ss := func(ns string) manifest.Resource {
		r := res("SealedSecret", ns, "db")
		r.APIVersion = "bitnami.com/v1alpha1"
		return r
	}
	if err := p.Check([]manifest.Resource{ss("demo")}); err != nil {
		t.Fatalf("SealedSecret in an allowed namespace: %v", err)
	}
	if err := p.Check([]manifest.Resource{ss("kube-system")}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("SealedSecret in another namespace: err = %v", err)
	}
	if err := p.Check([]manifest.Resource{ss("")}); err == nil || !strings.Contains(err.Error(), "has no namespace") {
		t.Fatalf("SealedSecret without a namespace: err = %v", err)
	}
}

func TestPlaintextSecretPointsToAlternatives(t *testing.T) {
	err := New([]string{"demo"}).Check([]manifest.Resource{res("Secret", "demo", "db")})
	if err == nil || !strings.Contains(err.Error(), "SealedSecret") || strings.Contains(err.Error(), "SOPS") {
		t.Fatalf("err = %v, want it to point to SealedSecret only (the controller cannot decrypt SOPS)", err)
	}
}
