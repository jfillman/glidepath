package pipelineruns

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Tower's tektonActions.test.ts rerunBody case, ported.
func TestBodyLikeTower(t *testing.T) {
	run := map[string]any{
		"metadata": map[string]any{
			"name":      "ci-0-build-abc",
			"namespace": "app-backstage-cicd",
			"labels": map[string]any{
				"hangar.io/app":               "backstage",
				"hangar.io/flow":              "ci",
				"triggers.tekton.dev/trigger": "push",
				"tekton.dev/pipeline":         "build",
			},
			"annotations":     map[string]any{"pipelinesascode.tekton.dev/sha": "deadbeef"},
			"uid":             "u",
			"resourceVersion": "1",
		},
		"spec": map[string]any{
			"pipelineRef":     map[string]any{"resolver": "cluster", "params": []any{map[string]any{"name": "name", "value": "build"}}},
			"taskRunTemplate": map[string]any{"serviceAccountName": "pipeline-runner"},
			"status":          "CancelledRunFinally",
		},
		"status": map[string]any{"conditions": []any{}},
	}
	want := map[string]any{
		"apiVersion": "tekton.dev/v1",
		"kind":       "PipelineRun",
		"metadata": map[string]any{
			"generateName": "ci-0-build-abc-rerun-",
			"namespace":    "app-backstage-cicd",
			"labels":       map[string]any{"hangar.io/app": "backstage", "hangar.io/flow": "ci"},
		},
		"spec": map[string]any{
			"pipelineRef":     map[string]any{"resolver": "cluster", "params": []any{map[string]any{"name": "name", "value": "build"}}},
			"taskRunTemplate": map[string]any{"serviceAccountName": "pipeline-runner"},
		},
	}
	if got := Body(run); !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		t.Fatalf("copies the spec without spec.status, and only the app/flow labels:\n%s", g)
	}
	if _, ok := run["spec"].(map[string]any)["status"]; !ok {
		t.Fatal("the source run is not changed")
	}
	// The pre-rebrand pair is kept too; a run with no labels gets {}.
	b := Body(map[string]any{"metadata": map[string]any{"name": "r", "labels": map[string]any{"platform.io/app": "a", "platform.io/flow": "f", "x": "y"}}})
	if l := b["metadata"].(map[string]any)["labels"]; !reflect.DeepEqual(l, map[string]any{"platform.io/app": "a", "platform.io/flow": "f"}) {
		t.Fatalf("labels: %v", l)
	}
	if l := Body(map[string]any{"metadata": map[string]any{"name": "r"}})["metadata"].(map[string]any)["labels"]; !reflect.DeepEqual(l, map[string]any{}) {
		t.Fatalf("no labels: %v", l)
	}
}

func TestRerunName(t *testing.T) {
	h := KeyHash("shop", "ci-0-build-abc", "k1")
	if n := RerunName("ci-0-build-abc", h); n != "ci-0-build-abc-rerun-"+h[:5] {
		t.Fatalf("name: %s", n)
	}
	if KeyHash("shop", "ci-0-build-abc", "k2") == h || KeyHash("other", "ci-0-build-abc", "k1") == h {
		t.Fatal("the hash covers app, name and key")
	}
	// As long as the name generateName would give: the prefix is cut at 58, like the API server cuts it.
	long := strings.Repeat("a", 60)
	if n := RerunName(long, h); len(n) != 63 || !strings.HasPrefix(n, long[:58]) {
		t.Fatalf("long name: %s (%d)", n, len(n))
	}
}

func TestValidation(t *testing.T) {
	for _, n := range []string{"", "-a", "A", "a/b", strings.Repeat("a", 254)} {
		if ValidName(n) == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	for _, k := range []string{"", "a b", "a/b", strings.Repeat("k", 129), "ü"} {
		if ValidKey(k) == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	for _, k := range []string{"k", "0b9d1a2e-1111-4c4c", "tower:run.1_2", strings.Repeat("k", 128)} {
		if err := ValidKey(k); err != nil {
			t.Errorf("key %q: %v", k, err)
		}
	}
}

func cond(status, reason string) map[string]any {
	return map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Succeeded", "status": status, "reason": reason}}}}
}

func TestCancelState(t *testing.T) {
	cases := []struct {
		name string
		run  map[string]any
		want string
	}{
		{"running", cond("Unknown", "Running"), ""},
		{"no status yet", map[string]any{}, ""},
		{"pending", map[string]any{"spec": map[string]any{"status": "PipelineRunPending"}}, ""},
		{"stopping gracefully can still be cancelled", map[string]any{"spec": map[string]any{"status": "StoppedRunFinally"}}, ""},
		{"cancelling", map[string]any{"spec": map[string]any{"status": "CancelledRunFinally"}, "status": cond("Unknown", "CancelledRunningFinally")["status"]}, AlreadyCancelled},
		{"cancelled bare", map[string]any{"spec": map[string]any{"status": "Cancelled"}}, AlreadyCancelled},
		{"finished cancelled", cond("False", "Cancelled"), AlreadyCancelled},
		{"succeeded", cond("True", "Succeeded"), AlreadyFinished},
		{"failed", cond("False", "Failed"), AlreadyFinished},
	}
	for _, c := range cases {
		if got := state(c.run); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStoreUnusedOnInvalidInput(t *testing.T) {
	c := &Client{Store: nil} // any Store call would panic
	if _, _, err := c.Rerun(context.Background(), "ns", "a", "Bad", "k", false); err == nil {
		t.Fatal("bad name")
	}
	if _, _, err := c.Rerun(context.Background(), "ns", "a", "ok", "", false); err == nil {
		t.Fatal("no key")
	}
	if _, err := c.Cancel(context.Background(), "ns", "Bad", false); err == nil {
		t.Fatal("bad name")
	}
}
