package policy

import (
	"fmt"
	"strings"

	"github.com/rta/gitops/internal/manifest"
)

type Policy struct {
	allowed map[string]bool

	AllowSecrets bool
}

func New(namespace []string) Policy {
	m := make(map[string]bool, len(namespace))
	for _, ns := range namespace {
		if ns = strings.TrimSpace(ns); ns != "" {
			m[ns] = true
		}
	}
	return Policy{allowed: m}
}

func (p Policy) Empty() bool { return len(p.allowed) == 0 }

// check collects every violation
func (p Policy) Check(rs []manifest.Resource) error {
	var problems []string

	for _, r := range rs {
		if r.Kind == "Secret" && !p.AllowSecrets {
			problems = append(problems, fmt.Sprintf("%s: Secret %q must not be stored in git in plaintext", r.File, r.Name))
		}
		switch {
		case r.Kind == "Namespace":
			if !p.allowed[r.Name] {
				problems = append(problems, fmt.Sprintf("%s: Namespace %q is not allowed", r.File, r.Name))
			}
		case r.Namespace == "":
			problems = append(problems, fmt.Sprintf("%s: %s %q has no namespace (cluster-scope kinds are not allowed)", r.File, r.Kind, r.Name))
		case !p.allowed[r.Namespace]:
			problems = append(problems, fmt.Sprintf("%s: %s %q is in namespace %q, which is not allowed", r.File, r.Kind, r.Name, r.Namespace))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("policy: %d violation(s):\n %s", len(problems), strings.Join(problems, "\n "))
	}
	return nil
}
