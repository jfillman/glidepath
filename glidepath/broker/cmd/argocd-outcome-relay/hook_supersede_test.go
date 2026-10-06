package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// outcomePost sends the body the hook script would send for a release, built with the same
// buildEvent the fact path uses (they are proven identical in facts_test.go).
func outcomePost(t *testing.T, h *handler, em emission, mutate func(map[string]string)) *httptest.ResponseRecorder {
	t.Helper()
	cm, err := h.clientset.CoreV1().ConfigMaps("app-gate-api-cicd").Get(context.Background(), "release-tracking-chain1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		mutate(cm.Data)
		if _, err := h.clientset.CoreV1().ConfigMaps("app-gate-api-cicd").Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	rec := &record{AppNamespace: "app-gate-api-cicd", AppName: "gate-api", Env: "staging", Cluster: "kind-prod", ChainID: "chain1", ConfigJSON: "{}"}
	ev, err := buildEvent(rec, rel1, em)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/outcome/kind-prod", strings.NewReader(string(ev)))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	h.handleOutcome(w, req)
	return w
}

func TestShadowModeStillForwardsTheHookEvent(t *testing.T) {
	h, sent := newTestHandler(t, "shadow")
	if c := outcomePost(t, h, emSuccess, nil).Code; c != 202 || len(*sent) != 1 {
		t.Fatalf("shadow mode is the hooks' world: code %d sent %d", c, len(*sent))
	}
}

func TestEmitModeDropsTheHookCopyWhenFactsOwnTheRelease(t *testing.T) {
	// the PreSync deploying event: facts cannot have arrived, the first Progressing sends it
	h, sent := newTestHandler(t, "emit")
	if outcomePost(t, h, emDeploying, nil); len(*sent) != 0 {
		t.Errorf("a PreSync deploying for a release with a releaseId must be dropped, sent %d", len(*sent))
	}
	// a terminal event once the facts have moved the record
	for _, st := range []string{"progressing", "healthy", "aborted", "sync-failed"} {
		h, sent := newTestHandler(t, "emit")
		em := emSuccess
		if st == "aborted" || st == "sync-failed" {
			em = emFailure
		}
		outcomePost(t, h, em, func(d map[string]string) { d["state"] = st })
		if len(*sent) != 0 {
			t.Errorf("a %s hook event for a release the facts reported (%s) must be dropped, sent %d", em.Kind, st, len(*sent))
		}
	}
}

func TestEmitModeFallsBackToTheHookWhenNoFactArrived(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	// the record is still "merged": the Rollout reported nothing (lost facts, or no annotation)
	if c := outcomePost(t, h, emSuccess, func(d map[string]string) { d["state"] = "merged" }).Code; c != 202 || len(*sent) != 1 {
		t.Fatalf("the hook's success is the fallback: code %d sent %d", c, len(*sent))
	}
	if d := record1(t, h); !strings.Contains(d["emittedLive"], "deployed-success") || !strings.Contains(d["emittedLive"], "deploying") {
		t.Fatalf("the fallback must be recorded as sent: %q", d["emittedLive"])
	}
	// the heartbeat fact that arrives later must not send the same outcome a second time
	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3"))
	if len(*sent) != 1 {
		t.Errorf("a late fact must not duplicate the fallback event, sent %d", len(*sent))
	}
}

func TestEmitModeForwardsReleasesTheFactPathDoesNotOwn(t *testing.T) {
	// a record from before releaseId existed
	h, sent := newTestHandler(t, "emit")
	outcomePost(t, h, emSuccess, func(d map[string]string) { delete(d, "releaseId"); d["state"] = "healthy" })
	if len(*sent) != 1 {
		t.Errorf("a release with no releaseId is the hooks' alone, sent %d", len(*sent))
	}
	// no record at all
	h2, sent2 := newTestHandler(t, "emit")
	if err := h2.clientset.CoreV1().ConfigMaps("app-gate-api-cicd").Delete(context.Background(), "release-tracking-chain1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/outcome/kind-prod", strings.NewReader(`{"context":{"chainId":"gone","type":"dev.cdevents.environment.deployed.0.1.0"},"subject":{"content":{"cluster":"kind-prod","appNamespace":"app-gate-api-cicd","appName":"gate-api","outcome":"success"}}}`))
	req.Header.Set("Authorization", "Bearer tok")
	h2.handleOutcome(httptest.NewRecorder(), req)
	if len(*sent2) != 1 {
		t.Errorf("a release with no record is the hooks' alone, sent %d", len(*sent2))
	}
}
