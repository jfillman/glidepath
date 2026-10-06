package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testConfigJSON = `{"slack": {"enabled": true}, "deploy": {"releaseFile": "x"}}`

func newTestHandler(t *testing.T, mode string) (*handler, *[][]byte) {
	t.Helper()
	cs := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: registryConfigMap, Namespace: platformNamespace},
			Data:       map[string]string{"kind-prod": `{"relaySecretName":"relay-kind-prod"}`},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "relay-kind-prod", Namespace: platformNamespace},
			Data:       map[string][]byte{"token": []byte("tok")},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "release-tracking-chain1", Namespace: "app-gate-api-cicd"},
			Data: map[string]string{
				"appNamespace": "app-gate-api-cicd", "appName": "gate-api", "env": "staging", "cluster": "kind-prod",
				"gitUrl": "https://github.com/o/gate-api.git", "gitRevision": "abc123",
				"flowStartTime": "2026-10-05T10:00:00Z", "configJson": testConfigJSON, "releaseId": "chain1:kind-prod/staging",
			},
		},
	)
	var sent [][]byte
	h := &handler{clientset: cs, factsMode: mode}
	h.forward = func(_ context.Context, b []byte) error { sent = append(sent, b); return nil }
	return h, &sent
}

func factBody(releaseID, phase string, gen int, obs string) string {
	return `{"ns":"app-gate-api-staging","name":"gate-api","uid":"u1","generation":` + itoa(gen) +
		`,"annotations":{"hangar.io/release-id":"` + releaseID + `","hangar.io/app-namespace":"app-gate-api-cicd"},` +
		`"status":{"phase":"` + phase + `","observedGeneration":"` + obs + `","currentPodHash":"h1"}}`
}

func itoa(i int) string { return strconv.Itoa(i) }

func post(h *handler, cluster, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/facts/"+cluster, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.handleFacts(rec, req)
	return rec
}

func TestFactsEmitAndIdempotency(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	rel := "chain1:kind-prod/staging"

	if c := post(h, "kind-prod", "Bearer tok", factBody(rel, "Progressing", 3, "3")).Code; c != 202 {
		t.Fatalf("progressing: %d", c)
	}
	post(h, "kind-prod", "Bearer tok", factBody(rel, "Healthy", 3, "3"))
	post(h, "kind-prod", "Bearer tok", factBody(rel, "Healthy", 4, "4")) // scale or heartbeat: new generation
	if len(*sent) != 2 {
		t.Fatalf("want 2 forwarded events (the repeat is suppressed by release state), got %d", len(*sent))
	}
	id := func(b []byte) string { return between(string(b), `"id":"`, `"`) }
	if id((*sent)[0]) == id((*sent)[1]) {
		t.Errorf("deploying and deployed must differ")
	}
	if !strings.Contains(string((*sent)[0]), "environment.deploying") || !strings.Contains(string((*sent)[1]), `"outcome":"success"`) {
		t.Errorf("unexpected event shapes:\n%s\n%s", (*sent)[0], (*sent)[1])
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	return s[:strings.Index(s, b)]
}

func TestFactsShadowForwardsNothing(t *testing.T) {
	h, sent := newTestHandler(t, "")
	post(h, "kind-prod", "Bearer tok", factBody("chain1:kind-prod/staging", "Healthy", 3, "3"))
	if len(*sent) != 0 {
		t.Fatalf("shadow mode must not forward, got %d", len(*sent))
	}
}

func TestFactsIgnoredAndRejected(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	cases := []struct {
		name, cluster, auth, body string
		code                      int
	}{
		{"bad token", "kind-prod", "Bearer nope", factBody("chain1:kind-prod/staging", "Healthy", 3, "3"), 401},
		{"no token", "kind-prod", "", factBody("chain1:kind-prod/staging", "Healthy", 3, "3"), 401},
		{"untracked rollout", "kind-prod", "Bearer tok", `{"ns":"x","name":"y","generation":1,"annotations":{},"status":{"phase":"Healthy","observedGeneration":"1"}}`, 202},
		{"stale generation", "kind-prod", "Bearer tok", factBody("chain1:kind-prod/staging", "Healthy", 4, "3"), 202},
		{"no phase we map", "kind-prod", "Bearer tok", factBody("chain1:kind-prod/staging", "Unknown", 3, "3"), 202},
		{"unknown record", "kind-prod", "Bearer tok", factBody("nochain:kind-prod/staging", "Healthy", 3, "3"), 202},
		{"release-id names another cluster", "kind-prod", "Bearer tok", factBody("chain1:other/staging", "Healthy", 3, "3"), 400},
		{"malformed release-id", "kind-prod", "Bearer tok", factBody("garbage", "Healthy", 3, "3"), 400},
		{"record env differs from release-id", "kind-prod", "Bearer tok", factBody("chain1:kind-prod/prod", "Healthy", 3, "3"), 400},
		{"not json", "kind-prod", "Bearer tok", `{`, 400},
	}
	for _, c := range cases {
		if got := post(h, c.cluster, c.auth, c.body).Code; got != c.code {
			t.Errorf("%s: got %d want %d", c.name, got, c.code)
		}
	}
	if len(*sent) != 0 {
		t.Errorf("nothing above should have been forwarded, got %d", len(*sent))
	}
}

func TestFactsAppNamespaceCannotEscape(t *testing.T) {
	h, sent := newTestHandler(t, "emit")
	b := strings.Replace(factBody("chain1:kind-prod/staging", "Healthy", 3, "3"), "app-gate-api-cicd", "kube-system", 1)
	if c := post(h, "kind-prod", "Bearer tok", b).Code; c != 202 || len(*sent) != 0 {
		t.Fatalf("a fact naming a non-app namespace must be ignored, code=%d sent=%d", c, len(*sent))
	}
}
