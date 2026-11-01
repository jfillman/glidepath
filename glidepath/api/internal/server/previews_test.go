package server

import (
	"testing"

	"github.com/glidepath/api/internal/previews"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListPreviews(t *testing.T) {
	pr := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "shop-pr-9", "namespace": "app-shop-cicd",
			"labels": map[string]any{"hangar.io/app": "shop", "hangar.io/env": "pr-9", "hangar.io/ephemeral-env": "true"}},
		"spec": map[string]any{"destination": map[string]any{"namespace": "app-shop-pr-9"}}}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{previews.GVRApplications: "ApplicationList"}, pr)

	s, done := newServer(t, map[string]bool{"tower *": true, "agent list runs app-shop-cicd": true})
	defer done()
	s.Previews = &previews.Client{Dyn: dyn, CS: fake.NewSimpleClientset()}

	st, out, body := call(t, s, "/v1/apps/shop/previews", map[string]string{"Authorization": "Bearer t-tower"})
	ps, _ := out["previews"].([]any)
	if st != 200 || len(ps) != 1 || ps[0].(map[string]any)["pr"] != float64(9) {
		t.Fatalf("tower: %d %s", st, body)
	}
	// A preview is an environment: a caller who may list runs but not environments may not read it.
	if st, out, _ := call(t, s, "/v1/apps/shop/previews", map[string]string{"Authorization": "Bearer t-agent"}); st != 403 || out["rule"] != "GP-AUTHZ-001" {
		t.Fatalf("agent without environments: %d %v", st, out)
	}
}
