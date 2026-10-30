package server

import (
	"strings"
	"testing"
	"time"

	"github.com/glidepath/api/internal/releases"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// withFleetReleases gives s release records and Events in two apps' namespaces (and one ConfigMap that
// isn't a record), the cluster Tower's Ops Wall reads.
func withFleetReleases(s *Server) {
	rec := func(ns, name, app string, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			Labels: map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": app, "hangar.io/env": "prod"}}, Data: data}
	}
	ev := func(ns, name, reason, object string, at time.Time) *corev1.Event {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Reason: reason, Message: "m " + name,
			InvolvedObject: corev1.ObjectReference{Kind: "ConfigMap", Namespace: ns, Name: object}, LastTimestamp: metav1.NewTime(at)}
	}
	s.Releases = releases.Reader{CS: fake.NewSimpleClientset(
		rec("app-shop-cicd", "release-tracking-shop-1", "shop", map[string]string{"state": "healthy"}),
		rec("app-shop-cicd", "release-tracking-other-1", "other", nil),
		rec("app-cart-cicd", "release-tracking-cart-1", "cart", map[string]string{"state": "degraded"}),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "app-shop-cicd"}},
		ev("app-shop-cicd", "e1", "ReleaseStalled", "release-tracking-shop-1", now),
		ev("app-cart-cicd", "e2", "ReleaseDrift", "release-tracking-cart-1", now.Add(-time.Hour)),
		ev("app-cart-cicd", "e3", "ReleaseDrift", "cart-config", now),
		ev("app-cart-cicd", "e4", "Scheduled", "release-tracking-cart-1", now),
	)}
}

func TestFleetReleases(t *testing.T) {
	s, done := newServer(t, map[string]bool{"tower *": true, "shopper list releases app-shop-cicd": true})
	defer done()
	withFleetReleases(s)
	tower, shopper := map[string]string{"Authorization": "Bearer t-tower"}, map[string]string{"Authorization": "Bearer t-shopper"}
	st, out, body := call(t, s, "/v1/releases", tower)
	if st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	recs := out["releases"].([]any)
	if len(recs) != 3 {
		t.Fatalf("every record, every namespace, nothing else: %s", body)
	}
	byName := map[string]map[string]any{}
	for _, r := range recs {
		byName[r.(map[string]any)["name"].(string)] = r.(map[string]any)
	}
	cart := byName["release-tracking-cart-1"]
	if cart["namespace"] != "app-cart-cicd" || cart["labels"].(map[string]any)["hangar.io/env"] != "prod" || cart["data"].(map[string]any)["state"] != "degraded" {
		t.Fatalf("a record is its name, namespace, labels and data: %v", cart)
	}
	if d, ok := byName["release-tracking-other-1"]["data"].(map[string]any); !ok || len(d) != 0 {
		t.Fatalf("no data is {}, never null: %s", body)
	}
	st, out, body = call(t, s, "/v1/releases?app=shop", shopper)
	if st != 200 || len(out["releases"].([]any)) != 1 || out["releases"].([]any)[0].(map[string]any)["name"] != "release-tracking-shop-1" {
		t.Fatalf("app=: that app's records in its namespace only: %d %s", st, body)
	}
	if st, out, _ := call(t, s, "/v1/releases", shopper); st != 403 || out["rule"] != "GP-AUTHZ-001" || !strings.Contains(out["hint"].(string), "app=") {
		t.Fatalf("a per-app reader can't list cluster-wide: %d %v", st, out)
	}
	if st, _, _ := call(t, s, "/v1/releases?app=nope", tower); st != 404 {
		t.Fatalf("missing app: %d", st)
	}
	// The per-app list is unchanged: {name, data} only.
	_, out, body = call(t, s, "/v1/apps/shop/releases", tower)
	if r := out["releases"].([]any); len(r) != 1 || strings.Contains(body, `"namespace"`) {
		t.Fatalf("per-app list: %s", body)
	}
}

func TestFleetReleaseEvents(t *testing.T) {
	s, done := newServer(t, map[string]bool{"tower *": true, "shopper list releases app-shop-cicd": true})
	defer done()
	withFleetReleases(s)
	tower, shopper := map[string]string{"Authorization": "Bearer t-tower"}, map[string]string{"Authorization": "Bearer t-shopper"}
	st, out, body := call(t, s, "/v1/releases/events", tower)
	if st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	evs := out["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("release reasons about records only: %s", body)
	}
	want := []map[string]any{
		{"reason": "ReleaseStalled", "namespace": "app-shop-cicd", "recordName": "release-tracking-shop-1", "message": "m e1", "at": "2026-10-25T12:00:00Z"},
		{"reason": "ReleaseDrift", "namespace": "app-cart-cicd", "recordName": "release-tracking-cart-1", "message": "m e2", "at": "2026-10-25T11:00:00Z"},
	}
	for i, w := range want {
		for k, v := range w {
			if evs[i].(map[string]any)[k] != v {
				t.Errorf("event %d %s = %v, want %v", i, k, evs[i].(map[string]any)[k], v)
			}
		}
	}
	st, out, body = call(t, s, "/v1/releases/events?app=shop", shopper)
	if st != 200 || len(out["events"].([]any)) != 1 {
		t.Fatalf("app=: %d %s", st, body)
	}
	if st, out, _ := call(t, s, "/v1/releases/events", shopper); st != 403 || out["rule"] != "GP-AUTHZ-001" {
		t.Fatalf("a per-app reader can't list cluster-wide: %d %v", st, out)
	}
	s.Releases = releases.Reader{CS: fake.NewSimpleClientset()}
	if _, _, body := call(t, s, "/v1/releases/events", tower); !strings.Contains(body, `"events":[]`) {
		t.Fatalf("none is [], never null: %s", body)
	}
	if _, _, body := call(t, s, "/v1/releases", tower); !strings.Contains(body, `"releases":[]`) {
		t.Fatalf("none is [], never null: %s", body)
	}
}
