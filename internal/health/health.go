// Package health reports whether the workloads in the desired set have
// rolled out. It only observes: it never changes or rolls back anything.
package health

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/MateosXhukellari/GitOps/internal/manifest"
)

const (
	Healthy     = "Healthy"
	Progressing = "Progressing"
	Unknown     = "Unknown"
	Degraded    = "Degraded"
)

// rank orders states from best to worst for Worst.
var rank = map[string]int{Healthy: 0, Progressing: 1, Unknown: 2, Degraded: 3}

// Resource is the health of one checked object.
type Resource struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Health    string `json:"health"`
	Reason    string `json:"reason,omitempty"`
}

// Worst returns the worst state in rs, or Healthy when rs is empty.
func Worst(rs []Resource) string {
	worst := Healthy
	for _, r := range rs {
		if rank[r.Health] > rank[worst] {
			worst = r.Health
		}
	}
	return worst
}

// Checker reads workloads with `kubectl get` (needs only the get verb).
type Checker struct {
	Kubectl string
}

type kind struct {
	resource string // the name passed to kubectl get
	group    string // required API group; empty accepts any
	classify func([]byte) (string, string)
}

var kinds = map[string]kind{
	"Deployment":  {resource: "deployment", classify: classify},
	"StatefulSet": {resource: "statefulset", classify: classifyStatefulSet},
	"DaemonSet":   {resource: "daemonset", classify: classifyDaemonSet},
	// Group-qualified so a missing Argo CRD is a NotFound error, never another
	// API's Rollout.
	"Rollout": {resource: "rollouts.argoproj.io", group: "argoproj.io", classify: classifyRollout},
}

// Check returns the health of every supported workload in desired. Other kinds are skipped.
func (c Checker) Check(ctx context.Context, desired []manifest.Resource) []Resource {
	var out []Resource
	for _, r := range desired {
		k, ok := kinds[r.Kind]
		if !ok || (k.group != "" && !strings.HasPrefix(r.APIVersion, k.group+"/")) {
			continue
		}
		state, reason := c.get(ctx, k, r.Namespace, r.Name)
		out = append(out, Resource{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Health: state, Reason: reason})
	}
	return out
}

func (c Checker) get(ctx context.Context, k kind, ns, name string) (string, string) {
	bin := c.Kubectl
	if bin == "" {
		bin = "kubectl"
	}
	args := []string{"get", k.resource, name, "-o", "json"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Unknown, fmt.Sprintf("kubectl get: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return k.classify(stdout.Bytes())
}

func classify(data []byte) (string, string) {
	var d struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			UpdatedReplicas    int32 `json:"updatedReplicas"`
			AvailableReplicas  int32 `json:"availableReplicas"`
			Conditions         []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return Unknown, "parsing kubectl get output: " + err.Error()
	}

	want := int32(1) // the API server's default when spec.replicas is unset
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	unavailable := false
	for _, cond := range d.Status.Conditions {
		failed := (cond.Type == "Progressing" && cond.Status == "False") ||
			(cond.Type == "ReplicaFailure" && cond.Status == "True")
		if failed {
			return Degraded, cond.Reason + ": " + cond.Message
		}
		if cond.Type == "Available" && cond.Status == "False" {
			unavailable = true
		}
	}

	st := d.Status
	if st.ObservedGeneration < d.Metadata.Generation {
		return Progressing, "waiting for the deployment controller to observe the new spec"
	}
	if st.UpdatedReplicas == want && st.AvailableReplicas == want && !unavailable {
		return Healthy, ""
	}
	return Progressing, fmt.Sprintf("%d/%d updated, %d/%d available", st.UpdatedReplicas, want, st.AvailableReplicas, want)
}

type workload struct {
	Metadata struct {
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration     int64 `json:"observedGeneration"`
		UpdatedReplicas        int32 `json:"updatedReplicas"`
		ReadyReplicas          int32 `json:"readyReplicas"`
		NumberReady            int32 `json:"numberReady"`
		DesiredNumberScheduled int32 `json:"desiredNumberScheduled"`
	} `json:"status"`
}

// StatefulSets and DaemonSets have no progress deadline, so a stuck rollout
// stays Progressing; only Deployments and Rollouts can report Degraded.
func classifyStatefulSet(data []byte) (string, string) {
	var w workload
	if err := json.Unmarshal(data, &w); err != nil {
		return Unknown, "parsing kubectl get output: " + err.Error()
	}
	want := int32(1)
	if w.Spec.Replicas != nil {
		want = *w.Spec.Replicas
	}
	st := w.Status
	if st.ObservedGeneration < w.Metadata.Generation {
		return Progressing, "waiting for the statefulset controller to observe the new spec"
	}
	if st.UpdatedReplicas == want && st.ReadyReplicas == want {
		return Healthy, ""
	}
	return Progressing, fmt.Sprintf("%d/%d updated, %d/%d ready", st.UpdatedReplicas, want, st.ReadyReplicas, want)
}

func classifyDaemonSet(data []byte) (string, string) {
	var w workload
	if err := json.Unmarshal(data, &w); err != nil {
		return Unknown, "parsing kubectl get output: " + err.Error()
	}
	st := w.Status
	if st.ObservedGeneration < w.Metadata.Generation {
		return Progressing, "waiting for the daemonset controller to observe the new spec"
	}
	if st.NumberReady == st.DesiredNumberScheduled {
		return Healthy, ""
	}
	return Progressing, fmt.Sprintf("%d/%d ready", st.NumberReady, st.DesiredNumberScheduled)
}

// classifyRollout trusts the Argo Rollouts controller's own phase.
func classifyRollout(data []byte) (string, string) {
	var r struct {
		Status struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Unknown, "parsing kubectl get output: " + err.Error()
	}
	reason := r.Status.Message
	switch r.Status.Phase {
	case "Healthy":
		return Healthy, ""
	case "Progressing":
		return Progressing, reason
	case "Paused":
		return Progressing, strings.TrimSpace("paused " + reason)
	case "Degraded":
		return Degraded, reason
	case "":
		return Unknown, "rollout has no status.phase yet"
	}
	return Unknown, fmt.Sprintf("unrecognised phase %q", r.Status.Phase)
}
