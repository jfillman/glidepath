package pipelineruns

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// ---- the origin of a rerun (pipeline trust T3): the guard of the re-run's pipeline verifies the ref and sha the original
// came from, so the API copies exactly that and no other annotation.

type memStore struct {
	runs    map[string]map[string]any
	created []map[string]any
}

func (m *memStore) Get(_ context.Context, _, name string) (map[string]any, error) {
	r, ok := m.runs[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pipelineruns"}, name)
	}
	return r, nil
}
func (m *memStore) Create(_ context.Context, _ string, obj map[string]any, _ bool) error {
	m.created = append(m.created, obj)
	return nil
}
func (m *memStore) Patch(context.Context, string, string, []byte, bool) error { return nil }

func runWith(ann map[string]any) map[string]any {
	a := map[string]any{}
	for k, v := range ann {
		a[k] = v
	}
	return map[string]any{"metadata": map[string]any{"name": "src", "namespace": "ns", "annotations": a},
		"spec": map[string]any{"pipelineRef": map[string]any{}}}
}

func rerunAnnotations(t *testing.T, src map[string]any) map[string]any {
	t.Helper()
	m := &memStore{runs: map[string]map[string]any{"src": src}}
	if _, _, err := (&Client{Store: m}).Rerun(context.Background(), "ns", "app", "src", "key-1", false); err != nil {
		t.Fatal(err)
	}
	return m.created[0]["metadata"].(map[string]any)["annotations"].(map[string]any)
}

// A rerun keeps every per-task account of the original (the T5 rule): taskRunSpecs, and the run-wide taskRunTemplate, come through unchanged.
func TestRerunKeepsTaskRunSpecs(t *testing.T) {
	src := runWith(nil)
	spec := src["spec"].(map[string]any)
	specs := []any{
		map[string]any{"pipelineTaskName": "open-release-pr", "serviceAccountName": "release-record-writer"},
		map[string]any{"pipelineTaskName": "run-testworkflow", "serviceAccountName": "test-runner"},
		map[string]any{"pipelineTaskName": "clearance-gate", "serviceAccountName": "clearance-gate"},
	}
	spec["taskRunSpecs"] = specs
	spec["taskRunTemplate"] = map[string]any{"serviceAccountName": "pipeline-runner"}
	m := &memStore{runs: map[string]map[string]any{"src": src}}
	if _, _, err := (&Client{Store: m}).Rerun(context.Background(), "ns", "app", "src", "key-1", false); err != nil {
		t.Fatal(err)
	}
	got := m.created[0]["spec"].(map[string]any)
	if !reflect.DeepEqual(got["taskRunSpecs"], specs) {
		t.Fatalf("taskRunSpecs changed by the rerun: %v", got["taskRunSpecs"])
	}
	if !reflect.DeepEqual(got["taskRunTemplate"], map[string]any{"serviceAccountName": "pipeline-runner"}) {
		t.Fatalf("taskRunTemplate changed by the rerun: %v", got["taskRunTemplate"])
	}
}

func TestRerunOfAMainBuildCarriesItsPushOrigin(t *testing.T) {
	a := rerunAnnotations(t, runWith(map[string]any{
		"pipelinesascode.tekton.dev/event-type": "push", "pipelinesascode.tekton.dev/source-branch": "refs/heads/main", "pipelinesascode.tekton.dev/sha": "a1",
		"pipelinesascode.tekton.dev/repo-url": "https://github.com/o/r", "other/annotation": "x"}))
	if a[AnnotationOriginRef] != "refs/heads/main" || a[AnnotationOriginSha] != "a1" {
		t.Fatalf("origin not carried: %v", a)
	}
	if _, has := a[AnnotationOriginEvent]; has {
		t.Fatalf("a push origin has no event marker: %v", a)
	}
	// nothing else from the original's annotations: not PaC's, not an arbitrary one
	for k := range a {
		if k != AnnotationOriginRef && k != AnnotationOriginSha && k != AnnotationRerunOf && k != AnnotationKeyHash {
			t.Fatalf("annotation %s was copied: %v", k, a)
		}
	}
	if a[AnnotationRerunOf] != "src" {
		t.Fatalf("%v", a)
	}
}

func TestRerunOfAChainedStageKeepsItsStamp(t *testing.T) {
	a := rerunAnnotations(t, runWith(map[string]any{AnnotationOriginRef: "refs/tags/v1.2.3", AnnotationOriginSha: "b2", "glidepath.io/forged": "y"}))
	if a[AnnotationOriginRef] != "refs/tags/v1.2.3" || a[AnnotationOriginSha] != "b2" {
		t.Fatalf("%v", a)
	}
	if _, has := a["glidepath.io/forged"]; has {
		t.Fatalf("only the origin annotations are copied: %v", a)
	}
}

func TestRerunOfAPullRequestRunIsMarked(t *testing.T) {
	a := rerunAnnotations(t, runWith(map[string]any{"pipelinesascode.tekton.dev/event-type": "pull_request", "pipelinesascode.tekton.dev/source-branch": "feature",
		"pipelinesascode.tekton.dev/sha": "c3"}))
	if a[AnnotationOriginEvent] != "pull_request" {
		t.Fatalf("%v", a)
	}
	if _, has := a[AnnotationOriginRef]; has {
		t.Fatalf("a pull request has no ref origin: %v", a)
	}
}

// A rerun's origin is the original's, whatever it was asked: a rerun of a rerun keeps it, and a run with no origin gets none.
func TestRerunCannotChangeTheOrigin(t *testing.T) {
	first := rerunAnnotations(t, runWith(map[string]any{"pipelinesascode.tekton.dev/event-type": "push", "pipelinesascode.tekton.dev/source-branch": "refs/heads/main", "pipelinesascode.tekton.dev/sha": "a1"}))
	second := rerunAnnotations(t, runWith(first))
	if second[AnnotationOriginRef] != "refs/heads/main" || second[AnnotationOriginSha] != "a1" {
		t.Fatalf("%v", second)
	}
	none := rerunAnnotations(t, runWith(map[string]any{"pipelinesascode.tekton.dev/sha": "a1"}))
	if _, has := none[AnnotationOriginRef]; has {
		t.Fatalf("%v", none)
	}
	// a PaC push run whose own stamp disagrees is read from PaC's annotations, never from a stamp a file could not have set
	mixed := rerunAnnotations(t, runWith(map[string]any{"pipelinesascode.tekton.dev/event-type": "push", "pipelinesascode.tekton.dev/source-branch": "refs/heads/feature",
		"pipelinesascode.tekton.dev/sha": "d4", AnnotationOriginRef: "refs/heads/main", AnnotationOriginSha: "d4"}))
	if mixed[AnnotationOriginRef] != "refs/heads/feature" {
		t.Fatalf("PaC's own record wins: %v", mixed)
	}
}
