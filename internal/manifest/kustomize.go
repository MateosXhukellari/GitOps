package manifest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	SourceAuto      = "auto"
	SourcePlain     = "plain"
	SourceKustomize = "kustomize"
)

// DefaultRenderTimeout bounds one `kubectl kustomize` run.
const DefaultRenderTimeout = 60 * time.Second

// maxKustomizeDepth bounds how deep kustomization references are followed.
const maxKustomizeDepth = 32

var kustomizationFiles = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// Renderer renders the kustomization in dir and returns the YAML stream.
type Renderer func(ctx context.Context, dir string) ([]byte, error)

// Kubectl renders with `<bin> kustomize <dir>`. No flags are passed on purpose:
// Helm, alpha plugins and exec stay disabled and the default load restrictor
// (files must live under their kustomization root) stays in force.
func Kubectl(bin string) Renderer {
	return func(ctx context.Context, dir string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, "kustomize", dir)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("kubectl kustomize: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), nil
	}
}

// Loader loads the desired state from a directory, either by walking plain
// YAML files or by rendering a kustomization.
type Loader struct {
	Source  string        // auto (default), plain or kustomize
	Render  Renderer      // nil renders with "kubectl" from PATH
	Timeout time.Duration // per render; 0 means DefaultRenderTimeout
}

// Load returns the resources under root/path and the source that produced them.
// In auto mode a kustomization file in path selects kustomize; then only the
// rendered output counts and other YAML files in the directory are ignored.
func (l Loader) Load(ctx context.Context, root, path string) ([]Resource, string, error) {
	dir := filepath.Join(root, path)
	if rel, err := filepath.Rel(root, dir); err != nil || strings.HasPrefix(rel, "..") {
		return nil, "", fmt.Errorf("manifest: path %q escapes repository root", path)
	}
	source := l.Source
	if source == "" || source == SourceAuto {
		source = SourcePlain
		if kustomizationFile(dir) != "" {
			source = SourceKustomize
		}
	}
	switch source {
	case SourcePlain:
		rs, err := Load(root, path)
		return rs, source, err
	case SourceKustomize:
		rs, err := l.kustomize(ctx, root, path)
		return rs, source, err
	}
	return nil, "", fmt.Errorf("manifest: unknown source %q", source)
}

func (l Loader) kustomize(ctx context.Context, root, path string) ([]Resource, error) {
	dir := filepath.Join(root, path)
	if kustomizationFile(dir) == "" {
		return nil, fmt.Errorf("manifest: source is kustomize but %s has no kustomization file", path)
	}
	if err := checkKustomization(root, dir); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}

	render := l.Render
	if render == nil {
		render = Kubectl("kubectl")
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = DefaultRenderTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := render(rctx, dir)
	if errors.Is(rctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("manifest: kustomize render of %s timed out after %s", path, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("manifest: kustomize render of %s: %w", path, err)
	}

	rs, err := Parse(out)
	if err != nil {
		return nil, fmt.Errorf("manifest: kustomize output of %s: %w", path, err)
	}
	// An empty render must fail here: passed on, it would look like an empty
	// repo and the revision would apply as "nothing is desired".
	if len(rs) == 0 {
		return nil, fmt.Errorf("manifest: kustomize render of %s produced no resources", path)
	}
	for i := range rs {
		rs[i].File = path + " (kustomize)"
	}
	if err := checkDuplicates(rs); err != nil {
		return nil, err
	}
	return rs, nil
}

func kustomizationFile(dir string) string {
	for _, name := range kustomizationFiles {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			return filepath.Join(dir, name)
		}
	}
	return ""
}

type kustomization struct {
	Kind       string   `yaml:"kind"`
	Resources  []string `yaml:"resources"`
	Bases      []string `yaml:"bases"`
	Components []string `yaml:"components"`

	PatchesStrategicMerge []string   `yaml:"patchesStrategicMerge"`
	Crds                  []string   `yaml:"crds"`
	Configurations        []string   `yaml:"configurations"`
	Patches               []pathRef  `yaml:"patches"`
	PatchesJSON6902       []pathRef  `yaml:"patchesJson6902"`
	Replacements          []pathRef  `yaml:"replacements"`
	OpenAPI               pathRef    `yaml:"openapi"`
	ConfigMapGenerator    []fileRefs `yaml:"configMapGenerator"`
	SecretGenerator       []fileRefs `yaml:"secretGenerator"`
}

type pathRef struct {
	Path string `yaml:"path"`
}

type fileRefs struct {
	Files []string `yaml:"files"`
	Envs  []string `yaml:"envs"`
	Env   string   `yaml:"env"`
}

// forbiddenFields can run code or fetch from the network at render time.
var forbiddenFields = []string{"helmCharts", "helmChartInflationGenerator", "helmGlobals", "generators", "transformers", "validators"}

// checkKustomization statically checks every kustomization reachable from dir
// before kubectl runs, so a remote reference or plugin is rejected instead of
// being fetched or executed. Directory references must stay inside root.
func checkKustomization(root, dir string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	done := map[string]bool{}
	onPath := map[string]bool{}

	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		rel := relTo(realRoot, dir)
		if onPath[dir] {
			return fmt.Errorf("kustomization cycle at %s", rel)
		}
		if done[dir] {
			return nil
		}
		if depth > maxKustomizeDepth {
			return fmt.Errorf("kustomization references nested deeper than %d at %s", maxKustomizeDepth, rel)
		}
		file := kustomizationFile(dir)
		if file == "" {
			return fmt.Errorf("%s: referenced directory has no kustomization file", rel)
		}
		k, err := readKustomization(file)
		if err != nil {
			return fmt.Errorf("%s: %w", relTo(realRoot, file), err)
		}
		where := relTo(realRoot, file)

		onPath[dir] = true
		defer delete(onPath, dir)

		for _, ref := range k.fileRefs() {
			if isRemote(ref) {
				return fmt.Errorf("%s: remote reference %q is not allowed", where, ref)
			}
		}
		for _, ref := range append(append(append([]string(nil), k.Resources...), k.Bases...), k.Components...) {
			if isRemote(ref) {
				return fmt.Errorf("%s: remote resource %q is not allowed", where, ref)
			}
			target := filepath.Join(dir, ref)
			fi, err := os.Stat(target)
			if err != nil {
				return fmt.Errorf("%s: resource %q not found in the checkout (remote resources are not allowed)", where, ref)
			}
			real, err := filepath.EvalSymlinks(target)
			if err != nil {
				return err
			}
			if r, err := filepath.Rel(realRoot, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%s: resource %q is outside the repository checkout", where, ref)
			}
			if fi.IsDir() {
				if err := walk(real, depth+1); err != nil {
					return err
				}
			}
		}
		done[dir] = true
		return nil
	}
	start, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	return walk(start, 0)
}

func readKustomization(file string) (kustomization, error) {
	var k kustomization
	data, err := os.ReadFile(file)
	if err != nil {
		return k, err
	}
	var fields map[string]yaml.Node
	if err := yaml.Unmarshal(data, &fields); err != nil {
		return k, fmt.Errorf("invalid YAML: %w", err)
	}
	for _, f := range forbiddenFields {
		if _, ok := fields[f]; ok {
			return k, fmt.Errorf("%q is not allowed (no Helm or plugins at render time)", f)
		}
	}
	if err := yaml.Unmarshal(data, &k); err != nil {
		return k, fmt.Errorf("invalid kustomization: %w", err)
	}
	if k.Kind != "" && k.Kind != "Kustomization" && k.Kind != "Component" {
		return k, fmt.Errorf("unexpected kind %q", k.Kind)
	}
	return k, nil
}

// fileRefs lists the non-resource file references, which kustomize could
// otherwise fetch over HTTP.
func (k kustomization) fileRefs() []string {
	refs := append(append(append([]string(nil), k.Crds...), k.Configurations...), k.OpenAPI.Path)
	for _, p := range k.PatchesStrategicMerge {
		if !strings.Contains(p, "\n") { // an inline patch, not a file reference
			refs = append(refs, p)
		}
	}
	for _, list := range [][]pathRef{k.Patches, k.PatchesJSON6902, k.Replacements} {
		for _, p := range list {
			refs = append(refs, p.Path)
		}
	}
	for _, g := range append(append([]fileRefs(nil), k.ConfigMapGenerator...), k.SecretGenerator...) {
		refs = append(append(append(refs, g.Files...), g.Envs...), g.Env)
	}
	return refs
}

func isRemote(ref string) bool {
	return strings.Contains(ref, "://") || strings.HasPrefix(ref, "git@") || strings.HasPrefix(ref, "git::")
}

func relTo(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil {
		return rel
	}
	return p
}
