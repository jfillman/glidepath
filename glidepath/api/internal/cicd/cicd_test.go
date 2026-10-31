package cicd

import (
	"encoding/json"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergePatch(t *testing.T) {
	// RFC 7386's examples, with YAML as the target.
	target := map[string]any{"a": "b", "c": map[string]any{"d": "e", "f": "g"}, "list": []any{1, 2}}
	cases := []struct {
		patch string
		want  string
	}{
		{`{"a": "z", "c": {"f": null}}`, "a: z\nc:\n    d: e\nlist:\n    - 1\n    - 2\n"},
		{`{"c": "flat"}`, "a: b\nc: flat\nlist:\n    - 1\n    - 2\n"},
		{`{"list": [3]}`, "a: b\nc:\n    d: e\n    f: g\nlist:\n    - 3\n"},
		{`{"new": {"deep": {"k": true}}}`, "a: b\nc:\n    d: e\n    f: g\nlist:\n    - 1\n    - 2\nnew:\n    deep:\n        k: true\n"},
		{`{"a": null, "c": null, "list": null}`, "{}\n"},
	}
	for _, c := range cases {
		var p any
		dec := json.NewDecoder(stringsReader(c.patch))
		dec.UseNumber()
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		cp := map[string]any{}
		for k, v := range target {
			cp[k] = v
		}
		out, err := yaml.Marshal(MergePatch(cp, numbers(p)))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.patch, out, c.want)
		}
	}
}

func TestNumbersKeepIntegersIntegral(t *testing.T) {
	var p any
	dec := json.NewDecoder(stringsReader(`{"replicas": 2, "ratio": 0.5, "nested": [1, 2.5]}`))
	dec.UseNumber()
	dec.Decode(&p)
	got := numbers(p).(map[string]any)
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
