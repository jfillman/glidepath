package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/cel/environment"
	"sigs.k8s.io/yaml"
)

func policyFrom(t *testing.T, y string) policy {
	t.Helper()
	var p policy
	if err := yaml.Unmarshal([]byte(y), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// The two defects that kept glidepath-control-plane from syncing on hangar-dev (2026-11-15) must fail here, and their fixed forms must pass:
// the checker is only worth having if it can say no.
func TestTheCheckerRefusesWhatTheAPIServerRefuses(t *testing.T) {
	env := environment.MustBaseEnvSet(version.MajorMinor(1, 36))
	cases := []struct {
		name string
		spec string
		bad  int
	}{
		{"a map literal whose values have different shapes", `variables: [{name: catalog, expression: '{"a": {"params": ["x"]}, "b": {"anyBranch": true, "params": ["x"]}}'}]`, 1},
		{"the same data, one shape per map", `variables: [{name: catalog, expression: '{"a": ["x"], "b": ["x"]}'}, {name: anyBranch, expression: '["b"]'}]`, 0},
		{"a regex escape in an ordinary string", `validations: [{expression: "'refs/tags/v1.2.3'.matches('^refs/tags/v[0-9]+\\.[0-9]+$')"}]`, 1},
		{"the same regex as a raw string", `validations: [{expression: "'refs/tags/v1.2.3'.matches(r'^refs/tags/v[0-9]+\\.[0-9]+$')"}]`, 0},
		{"an expression that is not a bool", `validations: [{expression: "1 + 1"}]`, 1},
	}
	for _, c := range cases {
		p := policyFrom(t, "kind: ValidatingAdmissionPolicy\nmetadata: {name: t}\nspec:\n  "+c.spec+"\n")
		if got := check(env, p); (got > 0) != (c.bad > 0) {
			t.Errorf("%s: %d invalid expression(s), want invalid=%v", c.name, got, c.bad > 0)
		}
	}
}

// The rendered control-plane chart: every ValidatingAdmissionPolicy compiles under the API server's own checker, with the default values and with a
// values file that sets every optional list. This is the check that was missing when cel-python accepted a policy the API server refused.
func TestTheRenderedControlPlanePoliciesCompile(t *testing.T) {
	chart, err := filepath.Abs("../../../charts/glidepath-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("helm is required: without it this check would pass by doing nothing")
	}
	env := environment.MustBaseEnvSet(version.MajorMinor(1, 36))
	for name, values := range map[string][]string{
		"default values":                   {"-f", filepath.Join(chart, "tests", "values.example.yaml")},
		"every optional list":              {"-f", filepath.Join(chart, "tests", "values.example.yaml"), "-f", filepath.Join(chart, "tests", "values.vap-full.yaml")},
		"the upgrade window (Warn, Audit)": {"-f", filepath.Join(chart, "tests", "values.example.yaml"), "-f", filepath.Join(chart, "tests", "values.vap-warn.yaml"), "--api-versions", "monitoring.coreos.com/v1"},
	} {
		out, err := exec.Command("helm", append([]string{"template", "cp", chart}, values...)...).Output()
		if err != nil {
			t.Fatalf("%s: helm template: %v", name, err)
		}
		ps := policies(out)
		if len(ps) < 4 {
			t.Fatalf("%s: found %d policies, expected at least the 4 the chart renders: the check would be vacuous", name, len(ps))
		}
		for _, p := range ps {
			if bad := check(env, p); bad > 0 {
				t.Errorf("%s: %s has %d expression(s) the API server would refuse (run `go run .` over the render for the errors)", name, p.Metadata.Name, bad)
			}
		}
	}
}

// The compiler echoes the source under each error behind a "|". A long echo line is shortened, never dropped: the position caret under it is
// what tells a person where the expression is wrong.
func TestCondenseKeepsAShortenedEchoLine(t *testing.T) {
	long := strings.Repeat("a", 400)
	in := "ERROR: <input>:1:5: undeclared reference to 'x'\n | " + long + "\n | ....^"
	got := condense(in)
	if !strings.Contains(got, "ERROR: <input>:1:5: undeclared reference to 'x'") || !strings.Contains(got, "| ....^") {
		t.Fatalf("the error line and the caret line are kept:\n%s", got)
	}
	var echo string
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "aaaa") {
			echo = strings.TrimSpace(l)
		}
	}
	if echo == "" {
		t.Fatalf("the echo line was dropped:\n%s", got)
	}
	if len(echo) > 170 || !strings.HasSuffix(echo, " ...") {
		t.Fatalf("a long echo line is cut to 160 characters and marked: %d %q", len(echo), echo[len(echo)-10:])
	}
	// a short echo line is untouched
	if g := condense("ERROR: e\n | x + 1"); !strings.Contains(g, "| x + 1") || strings.Contains(g, "...") {
		t.Fatalf("%q", g)
	}
}

// The package comment documents the flag the program defines (it once named -version, which does not exist).
func TestThePackageCommentNamesTheRealFlag(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, `flag.Int("minor"`) {
		t.Fatal("the flag this test guards is gone: update the test with the program")
	}
	head := text[:strings.Index(text, "package main")]
	if strings.Contains(head, "-version") || !strings.Contains(head, "[-minor 36]") {
		t.Fatalf("the package comment must show `[-minor 36]` and not `-version`:\n%s", head)
	}
}
