package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const rel1 = "chain1:kind-prod/staging"

func record1(t *testing.T, h *handler) map[string]string {
	t.Helper()
	cm, err := h.clientset.CoreV1().ConfigMaps("app-gate-api-cicd").Get(context.Background(), "release-tracking-chain1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return cm.Data
}

func TestStateIsPersistedOnTheRecord(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Progressing", 3, "3"))
	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3"))
	d := record1(t, h)
	if d["state"] != "healthy" || d["podHash"] != "h1" || d["lastFactAt"] == "" || d["stateAt"] == "" {
		t.Fatalf("state not persisted: %v", d)
	}
	if d["emittedLive"] != "deployed-success,deploying" || d["emittedShadow"] != "" {
		t.Errorf("emitted set wrong: live=%q shadow=%q", d["emittedLive"], d["emittedShadow"])
	}
	if len(*sent) != 2 {
		t.Errorf("sent %d", len(*sent))
	}
	// the record keeps everything open-release-pr wrote
	if d["gitRevision"] != "abc123" || d["configJson"] == "" {
		t.Errorf("record lost its original keys: %v", d)
	}
}

func TestForwardFailureIsRetriedNotLost(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	fail := true
	h.forward = func(_ context.Context, b []byte) error {
		if fail {
			return errors.New("broker down")
		}
		*sent = append(*sent, b)
		return nil
	}
	if c := post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3")).Code; c != 502 {
		t.Fatalf("want 502 so the sender knows, got %d", c)
	}
	if d := record1(t, h); d["state"] != "" || d["emittedLive"] != "" {
		t.Fatalf("a failed forward must not mark the event as sent: %v", d)
	}
	fail = false // the heartbeat fact arrives later
	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3"))
	if len(*sent) != 2 || record1(t, h)["state"] != "healthy" {
		t.Errorf("the retry must send both events and persist: sent=%d %v", len(*sent), record1(t, h))
	}
}

func TestShadowThenEmitSendsTheHistory(t *testing.T) {
	h, sent := newTestHandler(t, "")
	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3"))
	if len(*sent) != 0 || record1(t, h)["emittedShadow"] == "" || record1(t, h)["emittedLive"] != "" {
		t.Fatalf("shadow must record under its own key: %v", record1(t, h))
	}
	live := &handler{clientset: h.clientset, factsMode: "emit", forward: h.forward}
	post(live, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3"))
	if len(*sent) != 2 {
		t.Errorf("flipping to emit must send what shadow only pretended to: sent %d", len(*sent))
	}
}

func TestNoPermissionToWriteStillEmits(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	h.clientset.(*fake.Clientset).PrependReactor("update", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "release-tracking-chain1", errors.New("no"))
	})
	if c := post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3")).Code; c != 202 || len(*sent) != 2 {
		t.Fatalf("an unwritable record must degrade to stateless, got %d sent=%d", c, len(*sent))
	}
}

func TestConflictReReadsAndRetries(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	conflicts := 1
	h.clientset.(*fake.Clientset).PrependReactor("update", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "release-tracking-chain1", errors.New("raced"))
		}
		return false, nil, nil
	})
	if c := post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3")).Code; c != 202 {
		t.Fatalf("code %d", c)
	}
	if record1(t, h)["state"] != "healthy" {
		t.Errorf("state not persisted after the retry: %v", record1(t, h))
	}
	// the first attempt's events went out before the conflict; the retry resends them with
	// the same ids, which downstream collapses
	ids := map[string]int{}
	for _, b := range *sent {
		ids[between(string(b), `"id":"`, `"`)]++
	}
	if len(ids) != 2 {
		t.Errorf("expected exactly the two event ids, got %v", ids)
	}
}

func TestDriftRaisesAnEventAndChangesNothing(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3"))
	n := len(*sent)
	drifted := strings.Replace(factBody(rel1, "Progressing", 4, "4"), `"currentPodHash":"h1"`, `"currentPodHash":"h2"`, 1)
	post(h, "kind-prod", "Bearer tok", drifted)
	if len(*sent) != n {
		t.Errorf("drift must not emit a CDEvent")
	}
	if d := record1(t, h); d["state"] != "healthy" || !strings.Contains(d["drift"], "h1 -> h2") {
		t.Errorf("record: %v", d)
	}
	evs, _ := h.clientset.CoreV1().Events("app-gate-api-cicd").List(context.Background(), metav1.ListOptions{})
	if len(evs.Items) != 1 || evs.Items[0].Reason != "ReleaseDrift" {
		t.Errorf("want one ReleaseDrift event, got %+v", evs.Items)
	}
}

func TestNewReleaseSupersedesTheOldOne(t *testing.T) {
	h, _ := newTestHandler(t, "emit")
	cs := h.clientset
	lbl := map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": "gate-api", "hangar.io/env": "staging", "hangar.io/cluster": "kind-prod"}
	mkCM := func(name, state, prAt, relID string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app-gate-api-cicd", Labels: lbl},
			Data: map[string]string{"state": state, "prCreatedAt": prAt, "releaseId": relID,
				"appNamespace": "app-gate-api-cicd", "appName": "gate-api", "env": "staging", "cluster": "kind-prod"}}
	}
	for _, cm := range []*corev1.ConfigMap{
		mkCM("release-tracking-old-healthy", "healthy", "2026-10-01T00:00:00Z", "old-healthy:kind-prod/staging"),
		mkCM("release-tracking-old-proposed", "proposed", "2026-10-02T00:00:00Z", "old-proposed:kind-prod/staging"),
		mkCM("release-tracking-newer", "merged", "2026-10-09T00:00:00Z", "newer:kind-prod/staging"),
	} {
		if _, err := cs.CoreV1().ConfigMaps("app-gate-api-cicd").Create(context.Background(), cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// label the fixture record chain1 the same way and give it a prCreatedAt in the middle
	c1, _ := cs.CoreV1().ConfigMaps("app-gate-api-cicd").Get(context.Background(), "release-tracking-chain1", metav1.GetOptions{})
	c1.Labels, c1.Data["prCreatedAt"], c1.Data["releaseId"] = lbl, "2026-10-05T00:00:00Z", rel1
	cs.CoreV1().ConfigMaps("app-gate-api-cicd").Update(context.Background(), c1, metav1.UpdateOptions{})

	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Progressing", 3, "3"))
	get := func(n string) map[string]string {
		cm, _ := cs.CoreV1().ConfigMaps("app-gate-api-cicd").Get(context.Background(), n, metav1.GetOptions{})
		return cm.Data
	}
	if d := get("release-tracking-old-healthy"); d["state"] != "superseded" || d["supersededBy"] != rel1 {
		t.Errorf("the older healthy release must be superseded by %s: %v", rel1, d)
	}
	if get("release-tracking-old-proposed")["state"] != "proposed" {
		t.Errorf("a proposal that never ran is stale, not superseded")
	}
	if get("release-tracking-newer")["state"] != "merged" {
		t.Errorf("a newer release must not be superseded by an older one")
	}
}
