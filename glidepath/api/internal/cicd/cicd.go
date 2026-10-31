// Package cicd is POST /v1/apps/{app}/cicd/validations: what a change to an app's cicd.yaml would mean, without
// writing anything (docs/admin/api.md#validate-a-cicdyaml-change). The caller sends a JSON merge patch (RFC 7386) or
// the whole file; the API reads the file as it is in the app's repo (the broker's read-only token, at a ref or the
// default branch), applies the patch, and asks the validator for the schema, convention and, for an agent, GP-SCOPE
// findings, plus every field the change touches. The validator is `glidepath validate --serve`, the same code the
// cicd-validate PR check and Clearance run, in a sidecar: the API never has a second copy of the rules.
package cicd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/environments"
	"github.com/glidepath/api/internal/github"

	"gopkg.in/yaml.v3"
)

// Source is the two GitHub reads a validation makes (internal/github's Session).
type Source interface {
	RawAt(ctx context.Context, owner, repo, path, ref string) (string, error)
	CommitSHA(ctx context.Context, owner, repo, ref string) (string, error)
}

// Invalid is a request the API refuses as malformed (400).
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

// Request is the body: exactly one of Patch and Content.
type Request struct {
	// Patch is an RFC 7386 JSON merge patch on the file at BaseRef: an object; null deletes a key.
	Patch json.RawMessage `json:"patch,omitempty"`
	// Content is the whole file after the change, as YAML text.
	Content *string `json:"content,omitempty"`
	// BaseRef is the branch, tag or commit the file is read at; "" is the default branch.
	BaseRef string `json:"baseRef,omitempty"`
	// Agent also applies the GP-SCOPE rules (what an agent may change) to the change from the base.
	Agent bool `json:"agent"`
}

// Problem is one finding, as glidepath validate reports it.
type Problem struct {
	Pointer  string `json:"pointer"`
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Hint     string `json:"hint"`
}

// Change is one field the change touches, with the contract's agentScope for it.
type Change struct {
	Pointer    string          `json:"pointer"`
	Change     string          `json:"change"` // added, removed, changed
	Before     json.RawMessage `json:"before,omitempty"`
	After      json.RawMessage `json:"after,omitempty"`
	AgentScope string          `json:"agentScope"`
}

// Base is the file the change was applied to.
type Base struct {
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	SHA     string `json:"sha,omitempty"`
	Present bool   `json:"present"`
}

// Result is the answer. Valid is the validator's verdict on the resulting file; the request itself succeeded.
type Result struct {
	Valid           bool      `json:"valid"`
	Problems        []Problem `json:"problems"`
	Changes         []Change  `json:"changes"`
	Summary         string    `json:"summary"`
	Base            Base      `json:"base"`
	Resulting       string    `json:"resulting"`
	ContractVersion int       `json:"contractVersion"`
}

// verdict is what the validator answers.
type verdict struct {
	Valid           bool      `json:"valid"`
	Problems        []Problem `json:"problems"`
	Changes         []Change  `json:"changes"`
	ContractVersion int       `json:"contractVersion"`
}

// Validator calls `glidepath validate --serve` (the sidecar).
type Validator struct {
	URL  string // e.g. http://127.0.0.1:8081
	HTTP *http.Client
}

func (v *Validator) validate(ctx context.Context, head string, base *string, agent bool) (*verdict, error) {
	body, _ := json.Marshal(map[string]any{"head": head, "base": base, "agent": agent})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(v.URL, "/")+"/validate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := v.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the validator (glidepath validate --serve) did not answer: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the validator answered %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out verdict
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("the validator's answer is not what the API expects: %w", err)
	}
	if out.Problems == nil {
		out.Problems = []Problem{}
	}
	if out.Changes == nil {
		out.Changes = []Change{}
	}
	return &out, nil
}

// Client validates changes.
type Client struct {
	Owner     string                  // the GitHub owner of every app repo (the chart's githubOwner)
	GitHub    func(app string) Source // a new Session per request
	Validator *Validator
}

var refOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)

// Validate reads the base, applies the change and asks the validator.
func (c *Client) Validate(ctx context.Context, app *apps.App, req Request) (*Result, error) {
	hasPatch, hasContent := len(bytes.TrimSpace(req.Patch)) > 0, req.Content != nil
	if hasPatch == hasContent {
		return nil, Invalid{"send exactly one of patch (a JSON merge patch object) or content (the whole cicd.yaml)"}
	}
	var patch map[string]any
	if hasPatch {
		dec := json.NewDecoder(bytes.NewReader(req.Patch))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, Invalid{"patch is not JSON: " + err.Error()}
		}
		p, ok := v.(map[string]any)
		if !ok {
			return nil, Invalid{"patch must be a JSON object (RFC 7386: keys to set, null to delete)"}
		}
		patch = numbers(p).(map[string]any)
	}
	if req.BaseRef != "" && (!refOK.MatchString(req.BaseRef) || strings.Contains(req.BaseRef, "..")) {
		return nil, Invalid{"baseRef must be a branch, tag or commit"}
	}
	repos := environments.ReposOf(app)
	gh := c.GitHub(app.Name)
	out := &Result{Base: Base{Repo: c.Owner + "/" + repos.Config, Ref: req.BaseRef}}
	if out.Base.Ref == "" {
		out.Base.Ref = "HEAD"
	}

	sha, err := gh.CommitSHA(ctx, c.Owner, repos.Config, out.Base.Ref)
	var he github.HTTPError
	if errors.As(err, &he) && (he.Status == 404 || he.Status == 422) {
		return nil, Invalid{fmt.Sprintf("baseRef %q is not a ref of %s", out.Base.Ref, out.Base.Repo)}
	}
	if err != nil {
		return nil, err
	}
	out.Base.SHA = sha

	baseText, err := gh.RawAt(ctx, c.Owner, repos.Config, "cicd.yaml", req.BaseRef)
	var base *string
	switch {
	case errors.As(err, &he) && he.Status == 404:
		// No file yet: the change adds it. The validator compares with an empty file.
	case err != nil:
		return nil, err
	default:
		out.Base.Present = true
		base = &baseText
	}

	if hasContent {
		out.Resulting = *req.Content
	} else {
		resulting, err := applyPatch(base, out.Base.Ref, patch)
		if err != nil {
			return nil, err
		}
		out.Resulting = resulting
	}

	v, err := c.Validator.validate(ctx, out.Resulting, base, req.Agent)
	if err != nil {
		return nil, err
	}
	out.Valid, out.Problems, out.Changes, out.ContractVersion = v.Valid, v.Problems, v.Changes, v.ContractVersion
	out.Summary = Summarize(out.Problems, out.Changes)
	return out, nil
}

// applyPatch applies the merge patch to the file's own text. The YAML is edited as a node tree (gopkg.in/yaml.v3
// keeps comments, key order and quoting), encoded with the files' two-space indent, and the blank lines yaml.v3
// drops are put back where they were, so `resulting` is the file with only the patched lines changed and a caller
// can commit it as it is (2026-10-31: the first agent PR rewrote all 73 lines of a cicd.yaml to flip one). A missing
// base is a fresh file made from the patch alone.
func applyPatch(base *string, ref string, patch map[string]any) (string, error) {
	if base == nil || strings.TrimSpace(*base) == "" {
		return encode(MergePatch(map[string]any{}, patch))
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(*base), &doc); err != nil {
		return "", Invalid{"the cicd.yaml at " + ref + " is not valid YAML, so a patch cannot be applied to it: " + err.Error()}
	}
	if len(doc.Content) == 0 { // comments only: an empty mapping with those comments kept
		doc.Kind, doc.Content = yaml.DocumentNode, []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return "", Invalid{"the cicd.yaml at " + ref + " is not a mapping, so a patch cannot be applied to it"}
	}
	mergeNode(root, patch)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	enc.Close()
	out := restoreText(*base, buf.String())
	if !strings.HasSuffix(*base, "\n") {
		out = strings.TrimSuffix(out, "\n")
	}
	return out, nil
}

// mergeNode applies an RFC 7386 merge patch to a mapping node in place: a nested object merges into a nested
// mapping, null removes the key (with its comments), anything else replaces the value (keeping the line comment),
// and a new key goes at the end of the mapping, in the patch's key order.
func mergeNode(m *yaml.Node, patch map[string]any) {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := patch[k]
		i, cur := findKey(m, k)
		if v == nil {
			if i >= 0 {
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
			}
			continue
		}
		if sub, ok := v.(map[string]any); ok && cur != nil && cur.Kind == yaml.MappingNode {
			mergeNode(cur, sub)
			continue
		}
		nn := &yaml.Node{}
		if err := nn.Encode(v); err != nil {
			continue
		}
		if cur != nil {
			nn.LineComment = cur.LineComment
			m.Content[i+1] = nn
		} else {
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, nn)
		}
	}
}

func findKey(m *yaml.Node, key string) (int, *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i, m.Content[i+1]
		}
	}
	return -1, nil
}

// restoreText puts back what yaml.v3's encoder changes on lines the patch did not touch: the blank lines it drops
// (each remembered by the first non-blank line after it, and put back before the output's first such line), the
// spaces inside a flow collection (`{ a: 1 }` comes out `{a: 1}`), and a comment's column (a trailing comment is
// re-indented to the entry it follows). An output line that is an original line up to that spelling is replaced by
// the original. A line the patch changed matches nothing and keeps the encoder's form.
func restoreText(orig, out string) string {
	ol := strings.Split(orig, "\n")
	blankBefore := map[string]bool{}
	spell := map[string]string{}
	for i, l := range ol {
		if strings.TrimSpace(l) == "" {
			for j := i + 1; i > 0 && j < len(ol); j++ {
				if strings.TrimSpace(ol[j]) != "" {
					blankBefore[ol[j]] = true
					break
				}
			}
			continue
		}
		spell[spelling(l)] = l
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	res := make([]string, 0, len(lines)+len(blankBefore))
	for _, l := range lines {
		if o, ok := spell[spelling(l)]; ok {
			l = o
		}
		if blankBefore[l] && len(res) > 0 && strings.TrimSpace(res[len(res)-1]) != "" {
			res = append(res, "")
		}
		delete(blankBefore, l)
		res = append(res, l)
	}
	return strings.Join(res, "\n") + "\n"
}

var flowSpace = regexp.MustCompile(`([\[{,])\s+|\s+([\]}])`)

// spelling is a line up to what the encoder may change: a comment at any column, a flow collection's inner spaces.
func spelling(l string) string {
	t := strings.TrimLeft(l, " ")
	if strings.HasPrefix(t, "#") {
		return "#" + strings.Join(strings.Fields(t[1:]), " ")
	}
	return l[:len(l)-len(t)] + flowSpace.ReplaceAllString(t, "$1$2")
}

func encode(v any) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	enc.Close()
	return buf.String(), nil
}

// MergePatch applies an RFC 7386 JSON merge patch to decoded data: objects merge key by key, null deletes, anything
// else replaces. For a file that doesn't exist yet; an existing file is patched as text (applyPatch).
func MergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, _ := target.(map[string]any)
	out := make(map[string]any, len(t)+len(p))
	for k, v := range t {
		out[k] = v
	}
	for k, v := range p {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = MergePatch(out[k], v)
	}
	return out
}

// numbers turns json.Number into int64 where it is integral, else float64, so the YAML the patch produces says
// `replicas: 2`, not `replicas: "2"`.
func numbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, e := range x {
			x[k] = numbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = numbers(e)
		}
	}
	return v
}

// Summarize is the one-line effect: the verdict, then what changed.
func Summarize(problems []Problem, changes []Change) string {
	var errs, warns int
	rules := map[string]bool{}
	for _, p := range problems {
		if p.Severity == "error" {
			errs++
			rules[p.Rule] = true
		} else {
			warns++
		}
	}
	s := "valid"
	if errs > 0 {
		list := make([]string, 0, len(rules))
		for r := range rules {
			list = append(list, r)
		}
		sort.Strings(list)
		s = fmt.Sprintf("%d error(s): %s", errs, strings.Join(list, ", "))
	}
	if warns > 0 {
		s += fmt.Sprintf(", %d warning(s)", warns)
	}
	if len(changes) == 0 {
		return s + "; no change"
	}
	parts := []string{}
	for i, c := range changes {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(changes)-5))
			break
		}
		switch c.Change {
		case "added":
			parts = append(parts, c.Pointer+" added "+string(c.After))
		case "removed":
			parts = append(parts, c.Pointer+" removed")
		default:
			parts = append(parts, c.Pointer+" "+string(c.Before)+" -> "+string(c.After))
		}
	}
	return fmt.Sprintf("%s; %d change(s): %s", s, len(changes), strings.Join(parts, "; "))
}
