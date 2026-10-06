package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReduceSyncFailed(t *testing.T) {
	s := &relState{State: stMerged}
	d := reduceSyncFailed(s, "boom", t0)
	if !eq(kinds(d), []string{kindDeploying, kindFailure}) || s.State != stSyncFailed || s.LastError != "boom" {
		t.Fatalf("first failure: %v %s %q", kinds(d), s.State, s.LastError)
	}
	// every further retry, and the final Failed, is the same failure
	if d := reduceSyncFailed(s, "boom again", t0.Add(time.Minute)); len(d.Emits) != 0 || d.Ignore == "" {
		t.Errorf("a repeat must emit nothing: %+v", d)
	}
	if s.LastError != "boom again" {
		t.Errorf("the latest error should be kept for the record")
	}
	// Argo's retry succeeds: the Rollout facts take it from here
	got := replay(s, []*fact{mk("Progressing", "h1", false), mk("Healthy", "h1", false)})
	want(t, got, none, []string{kindSuccess})
	if s.State != stHealthy {
		t.Errorf("state %s", s.State)
	}
}

func TestSyncFailureIsNotAFailedRelease_WhenTheReleaseAlreadyRan(t *testing.T) {
	for _, st := range []string{stHealthy, stSuperseded, stRolledBack, stClosed} {
		s := &relState{State: st}
		if d := reduceSyncFailed(s, "x", t0); len(d.Emits) != 0 || s.State != st {
			t.Errorf("sync failure on a %s release must change nothing: %+v state=%s", st, d, s.State)
		}
	}
}

func argoPost(h *handler, cluster, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/argocd/"+cluster, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.handleArgoFacts(rec, req)
	return rec
}

func labelRecord(t *testing.T, h *handler, state string) {
	t.Helper()
	cm, err := h.clientset.CoreV1().ConfigMaps("app-gate-api-cicd").Get(context.Background(), "release-tracking-chain1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Labels = map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": "gate-api", "hangar.io/env": "staging", "hangar.io/cluster": "kind-prod"}
	cm.Data["state"], cm.Data["prCreatedAt"] = state, "2026-10-05T10:00:00Z"
	if _, err := h.clientset.CoreV1().ConfigMaps("app-gate-api-cicd").Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func argoBody(phase string, retry int, app, env string) string {
	return `{"source":"argocd","name":"gate-api-staging","app":"` + app + `","env":"` + env + `","phase":"` + phase +
		`","retryCount":` + itoa(retry) + `,"message":"one or more synchronization tasks completed unsuccessfully"}`
}

func TestArgoSyncFailureReachesTheRelease(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	labelRecord(t, h, "merged")

	if c := argoPost(h, "kind-prod", "Bearer tok", argoBody("Running", 1, "gate-api", "staging")).Code; c != 202 {
		t.Fatalf("code %d", c)
	}
	if len(*sent) != 2 || !strings.Contains(string((*sent)[1]), `"outcome":"failure"`) {
		t.Fatalf("the first failed attempt must emit deploying and a failure: %d events", len(*sent))
	}
	d := record1(t, h)
	if d["state"] != "sync-failed" || !strings.Contains(d["lastError"], "completed unsuccessfully") {
		t.Errorf("record: %v", d)
	}
	// later retries and the final Failed are the same failure
	argoPost(h, "kind-prod", "Bearer tok", argoBody("Running", 3, "gate-api", "staging"))
	argoPost(h, "kind-prod", "Bearer tok", argoBody("Failed", 4, "gate-api", "staging"))
	if len(*sent) != 2 {
		t.Errorf("retries must not send more failure events, sent %d", len(*sent))
	}
	// Argo's retry works: the Rollout reports, the release recovers and is reported healthy
	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Progressing", 3, "3"))
	post(h, "kind-prod", "Bearer tok", factBody(rel1, "Healthy", 3, "3"))
	if len(*sent) != 3 || record1(t, h)["state"] != "healthy" {
		t.Errorf("recovery: sent %d state %s", len(*sent), record1(t, h)["state"])
	}
}

func TestArgoFactsThatAreNotAReleaseFailing(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	labelRecord(t, h, "merged")
	cases := []struct {
		name, auth, body string
		code             int
	}{
		{"bad token", "Bearer nope", argoBody("Running", 1, "gate-api", "staging"), 401},
		{"a healthy sync", "Bearer tok", argoBody("Succeeded", 0, "gate-api", "staging"), 202},
		{"a running sync with no retries", "Bearer tok", argoBody("Running", 0, "gate-api", "staging"), 202},
		{"an unlabelled application", "Bearer tok", argoBody("Failed", 5, "", ""), 202},
		{"an app name that is not a label value", "Bearer tok", argoBody("Failed", 5, "../x", "staging"), 202},
		{"another environment", "Bearer tok", argoBody("Failed", 5, "gate-api", "prod"), 202},
		{"an application with no records", "Bearer tok", argoBody("Failed", 5, "nobody", "staging"), 202},
		{"not json", "Bearer tok", `{`, 400},
	}
	for _, c := range cases {
		if got := argoPost(h, "kind-prod", c.auth, c.body).Code; got != c.code {
			t.Errorf("%s: got %d want %d", c.name, got, c.code)
		}
	}
	if len(*sent) != 0 {
		t.Errorf("none of these is a release failing, sent %d", len(*sent))
	}
}

func TestArgoSyncFailureIgnoresAnUnmergedProposal(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	labelRecord(t, h, "proposed") // PR open, not merged: a sync now is not this release
	argoPost(h, "kind-prod", "Bearer tok", argoBody("Failed", 5, "gate-api", "staging"))
	if len(*sent) != 0 || record1(t, h)["state"] != "proposed" {
		t.Errorf("a proposed release must not be marked failed: sent %d %v", len(*sent), record1(t, h))
	}
}
