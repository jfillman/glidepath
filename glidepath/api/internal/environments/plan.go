package environments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/glidepath/api/internal/github"

	"gopkg.in/yaml.v3"
)

// UpperEnvironment is Tower's UpperEnvironment: a Flight environment, its cluster when it names one, and
// production: true only on the one cicd.yaml marks so (ADR-0024).
type UpperEnvironment struct {
	Name       string `json:"name"`
	Cluster    string `json:"cluster,omitempty"`
	Production bool   `json:"production,omitempty"`
}

// Plan is Tower's ReleasePlan (backstage releasePlan.ts), field for field and in its key order.
type Plan struct {
	Order       []string           `json:"order"`
	OrderSource string             `json:"orderSource"` // promotionOrder | pipeline-steps
	Lower       []string           `json:"lower"`
	Upper       []UpperEnvironment `json:"upper"`
	ReleaseFile any                `json:"releaseFile,omitempty"` // deploy.releaseFile as written, when it is
}

// isLower and isUpper are Tower's case-insensitive tier checks.
func (p *Plan) isLower(env string) bool {
	for _, e := range p.Lower {
		if strings.EqualFold(e, env) {
			return true
		}
	}
	return false
}

func (p *Plan) index(env string) int {
	for i, e := range p.Order {
		if strings.EqualFold(e, env) {
			return i
		}
	}
	return -1
}

// mapGet is doc.get(key) on a YAML mapping node, nil for anything else (as Tower's optional chaining reads it).
func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			v := n.Content[i+1]
			if v.Kind == yaml.AliasNode {
				v = v.Alias
			}
			return v
		}
	}
	return nil
}

// getIn is the yaml package's doc.getIn(path): the node at path, nil when any step is missing or not a mapping.
func getIn(n *yaml.Node, path ...string) *yaml.Node {
	for _, k := range path {
		if n = mapGet(n, k); n == nil {
			return nil
		}
	}
	return n
}

// scalar reads a node the way Tower's `as string` cast does: a scalar's text; nothing for a missing node or null
// (which `??` falls through).
func scalar(n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || n.ShortTag() == "!!null" {
		return "", false
	}
	return n.Value, true
}

// truthy is JavaScript's truthiness of a scalar, with its text: a non-empty string, true, a non-zero number.
func truthy(n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", n != nil && n.Kind != yaml.ScalarNode // a mapping or list is truthy, but has no text
	}
	switch n.ShortTag() {
	case "!!str":
		return n.Value, n.Value != ""
	case "!!bool":
		var b bool
		n.Decode(&b)
		return n.Value, b
	case "!!int", "!!float":
		var f float64
		n.Decode(&f)
		return n.Value, f != 0 && f == f
	}
	return "", false
}

// parse is Tower's parseDocument + assertValidYamlDoc: invalid YAML (a duplicate key among them) is an error naming
// the file, never a guess at which duplicate wins.
func parse(content, path string) (*yaml.Node, error) {
	doc, err := unmarshal(content)
	if err != nil {
		return nil, errors.New(path + " has invalid YAML and can't be safely read or written: " + err.Error())
	}
	return doc, nil
}

// unmarshal parses YAML into a node, refusing duplicate mapping keys as the yaml package Tower uses does
// (uniqueKeys): decoding into a yaml.Node does not check them itself.
func unmarshal(content string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	var check func(n *yaml.Node) error
	check = func(n *yaml.Node) error {
		if n.Kind == yaml.MappingNode {
			seen := map[string]int{}
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				if line, dup := seen[k.Value]; dup && k.Kind == yaml.ScalarNode {
					return fmt.Errorf("line %d: mapping key %q already defined at line %d", k.Line, k.Value, line)
				}
				seen[k.Value] = k.Line
			}
		}
		for _, c := range n.Content {
			if err := check(c); err != nil {
				return err
			}
		}
		return nil
	}
	return &doc, check(&doc)
}

// fetchPlan is Tower's fetchReleasePlan: the app's environments and their promotion order, from its own cicd.yaml
// (the source repo's, or an InfraService's gitops repo's). A missing cicd.yaml is a plan with nothing in it, as
// Tower answers a freshly onboarded app; any other GitHub failure is an error.
func fetchPlan(ctx context.Context, gh GitHub, owner string, r Repos) (*Plan, error) {
	raw, err := gh.Raw(ctx, owner, r.Config, "cicd.yaml")
	var he github.HTTPError
	if errors.As(err, &he) && he.Status == 404 {
		raw, err = "", nil
	}
	if err != nil {
		return nil, err
	}
	doc, err := unmarshal(raw)
	if err != nil {
		return nil, errors.New("cicd.yaml in " + owner + "/" + r.Config + ": " + err.Error())
	}
	return planOf(doc), nil
}

// planOf is fetchReleasePlan after the read, pure: deployEnvironments.ts's environmentsOf, then deploy.promotionOrder
// when it names only declared environments, else the order the release pipeline's deploy/release steps run in.
func planOf(doc *yaml.Node) *Plan {
	deploy := mapGet(doc, "deploy")
	p := &Plan{Order: []string{}, Lower: []string{}, Upper: []UpperEnvironment{}}
	declared := []string{}
	if envs := mapGet(deploy, "environments"); envs != nil && envs.Kind == yaml.SequenceNode {
		for _, e := range envs.Content {
			nameNode := mapGet(e, "name")
			if nameNode == nil || nameNode.Kind != yaml.ScalarNode || nameNode.ShortTag() != "!!str" {
				continue // Tower keeps only entries whose name is a string
			}
			name := nameNode.Value
			declared = append(declared, name)
			tier, _ := scalar(mapGet(e, "tier"))
			switch tier {
			case "ground":
				p.Lower = append(p.Lower, name)
			case "flight":
				u := UpperEnvironment{Name: name}
				if c, ok := truthy(mapGet(e, "cluster")); ok {
					u.Cluster = c
				}
				if pr := mapGet(e, "production"); pr != nil && pr.ShortTag() == "!!bool" {
					pr.Decode(&u.Production) // production === true only
				}
				p.Upper = append(p.Upper, u)
			}
		}
	}
	if rf := mapGet(deploy, "releaseFile"); rf != nil {
		var v any
		if rf.Decode(&v) == nil && v != nil {
			p.ReleaseFile = v
		}
	}
	known := map[string]bool{}
	for _, e := range p.Lower {
		known[e] = true
	}
	for _, e := range p.Upper {
		known[e.Name] = true
	}
	allKnown := len(declared) > 0
	for _, e := range declared {
		allKnown = allKnown && known[e]
	}
	if allKnown {
		p.Order, p.OrderSource = declared, "promotionOrder"
		return p
	}

	// Whichever pipeline has the most deploy/release steps with an env wins; a tie goes to the one defined first
	// (Tower's strict >, kept: >= would flip the tiebreak to last-defined-wins).
	var best []string
	if pipelines := mapGet(doc, "pipelines"); pipelines != nil && pipelines.Kind == yaml.MappingNode {
		for i := 1; i < len(pipelines.Content); i += 2 {
			var envSteps []string
			if steps := mapGet(pipelines.Content[i], "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
				for _, s := range steps.Content {
					env, _ := scalar(mapGet(s, "env"))
					stage, _ := scalar(mapGet(s, "stage"))
					if env != "" && (stage == "deploy" || stage == "release") {
						envSteps = append(envSteps, env)
					}
				}
			}
			if len(envSteps) > len(best) {
				best = envSteps
			}
		}
	}
	seen := map[string]bool{}
	for _, e := range best {
		if !seen[e] {
			seen[e] = true
			p.Order = append(p.Order, e)
		}
	}
	p.OrderSource = "pipeline-steps"
	return p
}
