package live

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func taskRun(ns, name, pr string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "tekton.dev/v1", "kind": "TaskRun",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{LabelPipelineRun: pr},
			"managedFields": []any{map[string]any{"manager": "tekton"}}}}}
}

// The kube store's selector and namespace handling, against client-go's fakes.
func TestKubeStore(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVRPipelineRuns: "PipelineRunList", GVRTaskRuns: "TaskRunList"},
		taskRun("app-a-cicd", "a-1-build", "a-1"), taskRun("app-b-cicd", "b-1-build", "b-1"), taskRun("app-b-cicd", "b-2-build", "b-2"))
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "app-a-cicd"},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "prepare"}}, Containers: []corev1.Container{{Name: "step-build"}}}})
	c := &Client{Store: NewKubeStore(dyn, cs)}
	ctx := context.Background()
	trs, err := c.TaskRunsOf(ctx, "", []string{"a-1", "b-2"})
	if err != nil || len(trs) != 2 {
		t.Fatalf("by label, every namespace: %v %v", trs, err)
	}
	for _, tr := range trs {
		if _, ok := tr["metadata"].(map[string]any)["managedFields"]; ok {
			t.Fatal("managedFields dropped")
		}
	}
	if trs, _ := c.TaskRunsOf(ctx, "app-b-cicd", []string{"a-1", "b-1"}); len(trs) != 1 {
		t.Fatalf("one namespace: %v", trs)
	}
	if trs, _ := c.TaskRuns(ctx, "app-b-cicd"); len(trs) != 2 {
		t.Fatalf("all of a namespace: %v", trs)
	}
	ctrs, err := c.Store.Containers(ctx, "app-a-cicd", "p")
	if err != nil || len(ctrs) != 2 || ctrs[0] != "prepare" || ctrs[1] != "step-build" {
		t.Fatalf("containers: %v %v", ctrs, err)
	}
}

func TestParseNames(t *testing.T) {
	got, err := ParseNames(" b-1,a-1,,b-1 ")
	if err != nil || len(got) != 2 || got[0] != "a-1" || got[1] != "b-1" {
		t.Fatalf("trimmed, deduped, sorted: %v %v", got, err)
	}
	if got, err := ParseNames(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"A-1", "a b", "-a", "a)", "x,in (y"} {
		if _, err := ParseNames(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
