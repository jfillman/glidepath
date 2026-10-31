package cicd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func patchOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var p any
	dec := json.NewDecoder(stringsReader(s))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	return numbers(p).(map[string]any)
}

// The first agent PR (gate-api#36) rewrote every line of a 73-line cicd.yaml to flip one field. Patching the text
// keeps comments, key order, quoting and blank lines: only the patched lines change.
const base = `# Pipeline definition for the Hangar/Glidepath CI/CD flow.
apiVersion: platform/v1
kind: PipelineConfig

build:
  agent: nodejs-20 # pinned until the 22 image is multi-arch
  unitTest:
    enabled: true
    command: ./test.sh
  cache:
    enabled: false
    size: small

governance:
  sast: true
  imageScan: true
  sbom: false

pipelines:
  ci:
    trigger: { source: git, event: push, branch: main }
    steps:
      - stage: build
      - stage: deploy
        env: dev
# resync now`

func TestApplyPatchChangesOnlyThePatchedLines(t *testing.T) {
	b := base
	out, err := applyPatch(&b, "HEAD", patchOf(t, `{"build": {"cache": {"enabled": true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(base, "    enabled: false\n", "    enabled: true\n", 1)
	if out != want {
		t.Fatalf("\n got:\n%s\nwant:\n%s", out, want)
	}
}

func TestApplyPatchAddsRemovesAndReplaces(t *testing.T) {
	b := base + "\n"
	out, err := applyPatch(&b, "main", patchOf(t, `{"governance": {"sbom": null, "policyCheck": true}, "build": {"unitTest": "off"}, "notifications": {"slack": {"channel": "#ci"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		// "off" is a YAML 1.1 boolean when bare, so the encoder quotes the string: the one spelling it must change.
		"# Pipeline definition for the Hangar/Glidepath CI/CD flow.\napiVersion: platform/v1\nkind: PipelineConfig\n\nbuild:\n  agent: nodejs-20 # pinned until the 22 image is multi-arch\n  unitTest: \"off\"\n  cache:\n",
		"\ngovernance:\n  sast: true\n  imageScan: true\n  policyCheck: true\n\npipelines:\n",
		// A new top-level key lands at the end; the trailing comment stays with the entry it followed.
		"        env: dev\n# resync now\n",
		"\nnotifications:\n  slack:\n    channel: '#ci'\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing:\n%s\nin:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sbom") {
		t.Errorf("sbom was not removed:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("the base ended with a newline, so the result must too")
	}
}

func TestApplyPatchKeepsNumbersIntegralAndReplacesSubtrees(t *testing.T) {
	b := "a:\n  b: 1\n  c: [1, 2]\n"
	out, err := applyPatch(&b, "HEAD", patchOf(t, `{"a": {"b": 2, "c": {"d": 2.5}}, "replicas": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "a:\n  b: 2\n  c:\n    d: 2.5\nreplicas: 3\n" {
		t.Fatalf("got %q", out)
	}
}

func TestApplyPatchWithoutABaseIsAFreshFile(t *testing.T) {
	out, err := applyPatch(nil, "HEAD", patchOf(t, `{"apiVersion": "platform/v1", "build": {"agent": "nodejs-22"}}`))
	if err != nil || out != "apiVersion: platform/v1\nbuild:\n  agent: nodejs-22\n" {
		t.Fatalf("%q %v", out, err)
	}
	empty := "# nothing yet\n"
	out, err = applyPatch(&empty, "HEAD", patchOf(t, `{"kind": "PipelineConfig"}`))
	if err != nil || !strings.Contains(out, "kind: PipelineConfig") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestApplyPatchRefusesABaseThatIsNotAMapping(t *testing.T) {
	for _, b := range []string{"- a\n- b\n", "just a scalar\n", "a: [b\n"} {
		bb := b
		_, err := applyPatch(&bb, "main", patchOf(t, `{"a": 1}`))
		var inv Invalid
		if err == nil || !strings.Contains(err.Error(), "cicd.yaml at main") || !errorsAs(err, &inv) {
			t.Errorf("%q: %v", b, err)
		}
	}
}

func TestMergePatch(t *testing.T) {
	// RFC 7386's examples on decoded data, for the fresh-file path.
	target := map[string]any{"a": "b", "c": map[string]any{"d": "e", "f": "g"}, "list": []any{1, 2}}
	cases := []struct {
		patch string
		want  string
	}{
		{`{"a": "z", "c": {"f": null}}`, "a: z\nc:\n  d: e\nlist:\n  - 1\n  - 2\n"},
		{`{"c": "flat"}`, "a: b\nc: flat\nlist:\n  - 1\n  - 2\n"},
		{`{"list": [3]}`, "a: b\nc:\n  d: e\n  f: g\nlist:\n  - 3\n"},
		{`{"new": {"deep": {"k": true}}}`, "a: b\nc:\n  d: e\n  f: g\nlist:\n  - 1\n  - 2\nnew:\n  deep:\n    k: true\n"},
		{`{"a": null, "c": null, "list": null}`, "{}\n"},
	}
	for _, c := range cases {
		cp := map[string]any{}
		for k, v := range target {
			cp[k] = v
		}
		out, err := encode(MergePatch(cp, patchOf(t, c.patch)))
		if err != nil {
			t.Fatal(err)
		}
		if out != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.patch, out, c.want)
		}
	}
}

func TestNumbersKeepIntegersIntegral(t *testing.T) {
	got := patchOf(t, `{"replicas": 2, "ratio": 0.5, "nested": [1, 2.5]}`)
	if !reflect.DeepEqual(got["replicas"], int64(2)) || !reflect.DeepEqual(got["ratio"], 0.5) {
		t.Fatalf("got %#v", got)
	}
	if !reflect.DeepEqual(got["nested"], []any{int64(1), 2.5}) {
		t.Fatalf("got %#v", got["nested"])
	}
	out, _ := yaml.Marshal(got)
	if string(out) != "nested:\n    - 1\n    - 2.5\nratio: 0.5\nreplicas: 2\n" {
		t.Fatalf("got %q", out)
	}
}

func TestSummarize(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	if got := Summarize(nil, nil); got != "valid; no change" {
		t.Fatal(got)
	}
	problems := []Problem{{Rule: "GP-SCOPE-002", Severity: "error"}, {Rule: "GP-ENV-001", Severity: "warning"}, {Rule: "GP-SCHEMA-001", Severity: "error"}}
	changes := []Change{{Pointer: "/governance/sast", Change: "changed", Before: raw("true"), After: raw("false")},
		{Pointer: "/build/cach", Change: "added", After: raw(`{"enabled":true}`)}, {Pointer: "/secrets/0", Change: "removed"}}
	want := "2 error(s): GP-SCHEMA-001, GP-SCOPE-002, 1 warning(s); 3 change(s): /governance/sast true -> false; /build/cach added {\"enabled\":true}; /secrets/0 removed"
	if got := Summarize(problems, changes); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	many := make([]Change, 8)
	for i := range many {
		many[i] = Change{Pointer: "/p", Change: "removed"}
	}
	if got := Summarize(nil, many); got != "valid; 8 change(s): /p removed; /p removed; /p removed; /p removed; /p removed; and 3 more" {
		t.Fatal(got)
	}
}
