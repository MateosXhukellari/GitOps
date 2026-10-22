package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Resource struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	File       string
	Raw        []byte
}

func (r Resource) Key() string {
	return fmt.Sprintf("%s/%s/%s/%s", r.APIVersion, r.Kind, r.Namespace, r.Name)
}

func Load(root, path string) ([]Resource, error) {
	base := filepath.Join(root, path)
	if rel, err := filepath.Rel(root, base); err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("manifest: path %q escapes repository root", path)
	}

	var out []Resource
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(root, p)
		rs, err := Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}

		for i := range rs {
			rs[i].File = rel
		}
		out = append(out, rs...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := checkDuplicates(out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkDuplicates sorts rs by key and rejects any key that appears twice.
func checkDuplicates(rs []Resource) error {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Key() < rs[j].Key() })
	for i := 1; i < len(rs); i++ {
		if rs[i].Key() == rs[i-1].Key() {
			return fmt.Errorf("manifest: duplicate resource %s in %s and %s",
				rs[i].Key(), rs[i-1].File, rs[i].File)
		}
	}
	return nil
}

// Parse splits a multi-document YAML stream into resources.
func Parse(data []byte) ([]Resource, error) {
	docs, err := splitDocs(data)
	if err != nil {
		return nil, err
	}
	var out []Resource
	for i, doc := range docs {
		r, hasContent, err := identify(doc)
		if err != nil {
			// never drop a non-empty document silently: it would vanish from desired state
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		if !hasContent {
			continue
		}
		r.Raw = doc
		out = append(out, r)
	}
	return out, nil
}

// splitDocs splits on "---" lines itself so each Raw is the document exactly as written.
func splitDocs(data []byte) ([][]byte, error) {
	var docs [][]byte
	var cur bytes.Buffer
	flush := func() {
		if cur.Len() > 0 {
			docs = append(docs, append([]byte(nil), cur.Bytes()...))
			cur.Reset()
		}
	}

	for i, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			continue
		}
		trimmed := strings.TrimRight(line, " \t\r\n")
		if trimmed == "---" || strings.HasPrefix(trimmed, "--- ") {
			// content on the marker line would otherwise be dropped silently
			if rest := strings.TrimSpace(trimmed[3:]); rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, fmt.Errorf("line %d: content after \"---\" on the same line is not supported", i+1)
			}
			flush()
			continue
		}
		cur.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			cur.WriteByte('\n') // join() needs every doc to end in a newline
		}
	}
	flush()
	return docs, nil
}

// identify reads apiVersion, kind and metadata name/namespace; hasContent is false for blank or comment-only docs.
func identify(doc []byte) (r Resource, hasContent bool, err error) {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	var n yaml.Node
	if err := dec.Decode(&n); err == io.EOF {
		return r, false, nil
	} else if err != nil {
		return r, true, fmt.Errorf("invalid YAML: %w", err)
	}
	if len(n.Content) == 0 {
		return r, false, nil
	}
	// a "..." end marker followed by more content would hide a second document
	if err := dec.Decode(new(yaml.Node)); err != io.EOF {
		return r, true, errors.New("more than one YAML document; separate documents with \"---\"")
	}
	if n.Content[0].Kind != yaml.MappingNode {
		return r, true, errors.New("not a YAML mapping")
	}

	var v struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
	}
	if err := n.Decode(&v); err != nil {
		return r, true, fmt.Errorf("cannot read identity fields: %w", err)
	}
	r = Resource{APIVersion: v.APIVersion, Kind: v.Kind, Name: v.Metadata.Name, Namespace: v.Metadata.Namespace}
	if r.APIVersion == "" || r.Kind == "" || r.Name == "" {
		return r, true, fmt.Errorf("cannot identify resource (apiVersion=%q kind=%q metadata.name=%q)",
			r.APIVersion, r.Kind, r.Name)
	}
	return r, true, nil
}
