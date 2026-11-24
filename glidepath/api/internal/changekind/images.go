package changekind

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// parsed is one file's documents: as written, with the image references taken out (what the semantic compare reads), and the
// references themselves.
type parsed struct {
	docs     []any
	stripped []any
	refs     []imageRef
}

// imageRef is one image reference: where it is (document index and YAML path) and its normalised form.
type imageRef struct {
	Doc  int
	Path string
	Ref  string
}

// parse reads every YAML (or JSON) document of a file. Empty documents are dropped, so comments, blank documents, whitespace and
// key order never count as a change.
func parse(name string, raw []byte) (*parsed, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	p := &parsed{}
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		budget := maxNodes
		v, err := fromNode(&n, 0, &budget)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		doc := len(p.docs)
		p.docs = append(p.docs, v)
		var refs []imageRef
		p.stripped = append(p.stripped, walk(v, "", &refs))
		for _, r := range refs {
			r.Doc = doc
			p.refs = append(p.refs, r)
		}
	}
	return p, nil
}

// scalarText is a scalar that is not a string, kept as written with its YAML tag: a tag of 2.0 stays "2.0" (as yq prints it),
// and 1 and "1" still differ.
type scalarText struct{ Tag, Text string }

func (s scalarText) String() string { return s.Text }

// maxDepth bounds nesting of a document; maxNodes bounds the nodes one document expands to, aliases included, so a document
// built of aliases of aliases cannot grow without bound.
const (
	maxDepth = 100
	maxNodes = 200000
)

// fromNode turns a YAML node into maps, lists, strings and scalarText. Aliases are expanded and merge keys (<<) applied; a
// duplicate key is an error, as it is to the API server.
func fromNode(n *yaml.Node, depth int, budget *int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("line %d: nested more than %d levels deep", n.Line, maxDepth)
	}
	if *budget--; *budget < 0 {
		return nil, fmt.Errorf("the document expands to more than %d nodes (aliases included)", maxNodes)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return fromNode(n.Content[0], depth+1, budget)
	case yaml.AliasNode:
		return fromNode(n.Alias, depth+1, budget)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromNode(c, depth+1, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		out := map[string]any{}
		var merges []map[string]any
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, vn := n.Content[i], n.Content[i+1]
			v, err := fromNode(vn, depth+1, budget)
			if err != nil {
				return nil, err
			}
			if k.Tag == "!!merge" {
				switch t := v.(type) {
				case map[string]any:
					merges = append(merges, t)
				case []any:
					for _, x := range t {
						if m, ok := x.(map[string]any); ok {
							merges = append(merges, m)
						}
					}
				}
				continue
			}
			if _, dup := out[k.Value]; dup {
				return nil, fmt.Errorf("line %d: mapping key %q already defined", k.Line, k.Value)
			}
			out[k.Value] = v
		}
		for _, m := range merges {
			for k, v := range m {
				if _, set := out[k]; !set {
					out[k] = v
				}
			}
		}
		return out, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!null":
			return nil, nil
		case "!!str", "!!binary", "":
			return n.Value, nil
		}
		return scalarText{n.Tag, n.Value}, nil
	}
	return nil, fmt.Errorf("line %d: unexpected YAML node", n.Line)
}

func join(p, k string) string {
	if p == "" {
		return k
	}
	return p + "." + k
}

func index(p string, i int) string { return fmt.Sprintf("%s[%d]", p, i) }

// walk copies v without its image references, collecting them. An image reference is any value under a key named image or
// images (spec section 5.2, step 4): values, release and base files' release.image and rollout.image and component images,
// a raw manifest's container image strings, a kustomization's images entries and the image keys of helmCharts[].valuesInline.
func walk(v any, p string, refs *[]imageRef) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if k == "image" || k == "images" {
				if rest := imageNode(x, join(p, k), k == "images", refs); rest != nil {
					out[k] = rest
				}
				continue
			}
			out[k] = walk(x, join(p, k), refs)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = walk(x, index(p, i), refs)
		}
		return out
	}
	return v
}

// refKeys are the fields that make up an image reference: Helm-style repository/tag/digest (and Glidepath's source, ADR-0029)
// and kustomize's name/newName/newTag/digest.
var refKeys = map[string]bool{"registry": true, "repository": true, "source": true, "tag": true, "digest": true,
	"name": true, "newName": true, "newTag": true}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

func scalar(v any) bool {
	switch v.(type) {
	case nil, map[string]any, []any:
		return false
	}
	return true
}

// imageNode takes the references out of a value under an image key and returns what is left (settings such as pullPolicy stay
// configuration), or nil when nothing is.
//
// many: the value is under an images key (a map of component images): a string in it is a reference, where under an image key a
// string beside the reference fields is a setting.
func imageNode(v any, p string, many bool, refs *[]imageRef) any {
	switch t := v.(type) {
	case string:
		*refs = append(*refs, imageRef{Path: p, Ref: t})
		return nil
	case map[string]any:
		isRef := false
		for k, x := range t {
			if refKeys[k] && scalar(x) {
				isRef = true
			}
		}
		if isRef {
			*refs = append(*refs, imageRef{Path: p, Ref: compose(t)})
		}
		out := map[string]any{}
		for k, x := range t {
			switch {
			case isRef && refKeys[k] && scalar(x):
			case !isRef && many && isString(x):
				*refs = append(*refs, imageRef{Path: join(p, k), Ref: x.(string)}) // images: {worker: busybox:1}
			case !scalar(x) && x != nil:
				// a map or list below an image key is image data too (component images, kustomize's images list)
				if rest := imageNode(x, join(p, k), many, refs); rest != nil {
					out[k] = rest
				}
			default:
				out[k] = x
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = imageNode(x, index(p, i), many, refs)
		}
		return out
	}
	return v
}

// compose is a reference's normalised text, with every part that names the image in it: [registry/]repository or a kustomize
// name (=> newName), :tag or :newTag, @digest, and Glidepath's source when it differs from the repository.
func compose(m map[string]any) string {
	s := func(k string) string {
		if x, ok := m[k]; ok && scalar(x) {
			return fmt.Sprint(x)
		}
		return ""
	}
	ref := s("repository")
	if r := s("registry"); r != "" {
		ref = strings.TrimSuffix(r, "/") + "/" + ref
	}
	if n := s("name"); n != "" {
		if ref == "" {
			ref = n
		} else {
			ref += " name=" + n
		}
	}
	if n := s("newName"); n != "" {
		ref += "=>" + n
	}
	if t := s("tag"); t != "" {
		ref += ":" + t
	}
	if t := s("newTag"); t != "" {
		ref += ":" + t
	}
	if d := s("digest"); d != "" {
		ref += "@" + d
	}
	if src := s("source"); src != "" && src != s("repository") {
		ref += " source=" + src
	}
	return ref
}

// firstScalar is yq's a // b // c over dotted paths: the first that is set and neither null nor false, as text.
func firstScalar(doc any, paths ...string) string {
	for _, p := range paths {
		v := doc
		for _, k := range strings.Split(p, ".") {
			m, ok := v.(map[string]any)
			if !ok {
				v = nil
				break
			}
			v = m[k]
		}
		if st, ok := v.(scalarText); ok && st.Tag == "!!bool" && st.Text == "false" {
			continue // yq's // passes over false
		}
		if v == nil || !scalar(v) {
			continue
		}
		return fmt.Sprint(v)
	}
	return ""
}

// diffPaths appends the paths where a and b differ: a key added, removed or changed, a list element changed, a list that
// grew or shrank.
func diffPaths(a, b any, p string, out *[]string) {
	name := p
	if name == "" {
		name = "(the whole document)"
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			x, inA := am[k]
			y, inB := bm[k]
			switch {
			case !inA || !inB:
				*out = append(*out, join(p, k))
			default:
				diffPaths(x, y, join(p, k), out)
			}
		}
		return
	}
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok && bok {
		n := max(len(al), len(bl))
		for i := 0; i < n; i++ {
			if i >= len(al) || i >= len(bl) {
				*out = append(*out, index(p, i))
				continue
			}
			diffPaths(al[i], bl[i], index(p, i), out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, name)
	}
}
