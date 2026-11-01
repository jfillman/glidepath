package previews

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

// An Application as the shop-pr-envs ApplicationSet generates it (ephemeral-envs.yaml), trimmed to what is read.
func app(ns, app string, pr int, labels map[string]any, status map[string]any) *unstructured.Unstructured {
	if labels == nil {
		labels = map[string]any{"hangar.io/app": app, "hangar.io/env": "pr-" + itoa(pr), "hangar.io/ephemeral-env": "true"}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": app + "-pr-" + itoa(pr), "namespace": ns, "labels": labels,
			"creationTimestamp": "2026-10-26T21:38:47Z"},
		"spec": map[string]any{
			"destination": map[string]any{"namespace": "app-" + app + "-pr-" + itoa(pr)},
			"sources": []any{
				map[string]any{"repoURL": "https://github.com/o/airframe.git", "helm": map[string]any{"valuesObject": map[string]any{
					"release": map[string]any{"image": map[string]any{"repository": "ghcr.io/o/" + app + "-pr", "tag": "a516f39785a9"}}}}},
				map[string]any{"repoURL": "https://github.com/o/" + app + ".git", "targetRevision": "a516f39785a9393fc6d1c6820e093c4944501369", "ref": "appsrc"},
			},
		},
		"status": status,
	}}
}

func itoa(n int) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }

func TestList(t *testing.T) {
	healthy := map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Healthy", "lastTransitionTime": "2026-10-31T17:14:14Z"}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVRApplications: "ApplicationList"},
		app("app-shop-cicd", "shop", 12, nil, healthy),
		app("app-shop-cicd", "shop", 55, nil, nil), // just generated: no status yet
		app("app-shop-cicd", "shop", 7, map[string]any{"hangar.io/app": "shop", "hangar.io/env": "dev"}, healthy), // not a preview
		app("app-cart-cicd", "cart", 3, nil, healthy), // another app
	)
	cs := fake.NewSimpleClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "app-shop-pr-12"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}}})
	c := &Client{Dyn: dyn, CS: cs, Owner: "o"}

	ps, err := c.List(context.Background(), "app-shop-cicd", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].PR != 55 || ps[1].PR != 12 {
		t.Fatalf("the two shop previews, newest PR first: %+v", ps)
	}
	if p := ps[0]; p.Sync != "Unknown" || p.Health != "Unknown" || p.Service != "" || p.PortForward != "" {
		t.Fatalf("a preview Argo CD has not reported on yet: %+v", p)
	}
	p := ps[1]
	want := Preview{PR: 12, PRURL: "https://github.com/o/shop/pull/12", Application: "shop-pr-12", Namespace: "app-shop-pr-12",
		Image: "ghcr.io/o/shop-pr:a516f39785a9", HeadSha: "a516f39785a9393fc6d1c6820e093c4944501369",
		Sync: "Synced", Health: "Healthy", HealthSince: "2026-10-31T17:14:14Z", CreatedAt: "2026-10-26T21:38:47Z",
		Service: "shop", Port: 8080, PortForward: "kubectl port-forward -n app-shop-pr-12 svc/shop 8080:8080"}
	if p != want {
		t.Fatalf("got  %+v\nwant %+v", p, want)
	}
}
