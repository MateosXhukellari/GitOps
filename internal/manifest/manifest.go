package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	for i := 1; i < len(out); i++ {
		if out[i].Key() == out[i-1].Key() {
			return nil, fmt.Errorf("manifest: duplicate resource %s in %s and %s",
				out[i].Key(), out[i-1].File, out[i].File)
		}
	}
	return out, nil
}

// parse splits multi-doc yaml stream into resources
func Parse(data []byte) ([]Resource, error) {
	var out []Resource
	for _, doc := range splitDocs(data) {
		r, ok := identify(doc)

		if !ok {
			continue
		}
		if r.Name == "" {
			return nil, fmt.Errorf("%s is missing metadata.name", r.Kind)
		}
		r.Raw = doc
		out = append(out, r)
	}

	return out, nil
}

func splitDocs(data []byte) [][]byte {
	var docs [][]byte
	var cur bytes.Buffer
	flush := func() {
		if cur.Len() > 0 {
			docs = append(docs, append([]byte(nil), cur.Bytes()...))
			cur.Reset()
		}
	}

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "---" || strings.HasPrefix(trimmed, "--- ") {
			flush()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	flush()
	return docs
}

func identify(doc []byte) (Resource, bool) {
	var r Resource
	inMetadata := false
	metaIndent := -1

	sc := bufio.NewScanner(bytes.NewReader(doc))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, val, ok := splitKv(strings.TrimSpace(line))

		if !ok {
			continue
		}

		if indent == 0 {
			inMetadata = false
			metaIndent = -1
			switch key {
			case "apiVersion":
				r.APIVersion = val
			case "kind":
				r.Kind = val
			case "metadata":
				inMetadata = true
			}
			continue
		}

		if inMetadata {
			if metaIndent == -1 {
				metaIndent = indent
			}
			if indent == metaIndent {
				switch key {
				case "name":
					r.Name = val
				case "namespace":
					r.Namespace = val
				}
			}
		}
	}
	return r, r.Kind != ""
}

// this func turns 'key': "value" # comment` intp key and value
func splitKv(s string) (key, val string, ok bool) {
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:i])
	val = strings.TrimSpace(s[i+1:])
	if strings.HasPrefix(key, "- ") || strings.ContainsAny(key, " \t\"'") {
		return "", "", false
	}

	if len(val) > 0 && (val[0] == '"' || val[0] == '\'') {
		q := val[0]
		if end := strings.IndexByte(val[1:], q); end >= 0 {
			return key, val[1 : 1+end], true
		}
	}
	if c := strings.Index(val, " #"); c >= 0 {
		val = strings.TrimSpace(val[:c])
	}
	return key, val, true
}
