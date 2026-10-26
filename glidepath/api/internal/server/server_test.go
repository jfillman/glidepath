package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/releases"
	"github.com/glidepath/api/internal/results"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeReviewer: token "t-<user>" authenticates as <user>; access is allowed per "user verb resource/sub ns".
type fakeReviewer struct{ allow map[string]bool }

func (f fakeReviewer) TokenReview(_ context.Context, token string) (*authenticationv1.TokenReviewStatus, error) {
	if !strings.HasPrefix(token, "t-") {
		return &authenticationv1.TokenReviewStatus{Authenticated: false, Error: "bad token"}, nil
	}
	return &authenticationv1.TokenReviewStatus{Authenticated: true, User: authenticationv1.UserInfo{Username: token[2:]}}, nil
}

func (f fakeReviewer) AccessReview(_ context.Context, c *auth.Caller, a authorizationv1.ResourceAttributes) (*authorizationv1.SubjectAccessReviewStatus, error) {
	res := a.Resource
	if a.Subresource != "" {
		res += "/" + a.Subresource
	}
	return &authorizationv1.SubjectAccessReviewStatus{Allowed: f.allow[c.User+" "+a.Verb+" "+res+" "+a.Namespace] || f.allow[c.User+" *"]}, nil
}

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func b64(v any) string {
	raw, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(raw)
}

const resultID = "0b9d1a2e-1111-4c4c-9999-aaaaaaaaaaaa"
const logID = "0b9d1a2e-2222-4c4c-9999-bbbbbbbbbbbb"

// resultsStub answers the Results queries results.Client makes.
func resultsStub(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/apis/results.tekton.dev/v1alpha2")
		switch {
		case p == "/parents/app-shop-cicd/results":
			json.NewEncoder(w).Encode(map[string]any{"results": []any{
				map[string]any{"name": "app-shop-cicd/results/" + resultID, "create_time": now.Add(-2 * time.Hour).Format(time.RFC3339),
					"annotations": map[string]string{"object.metadata.name": "shop-push-abc", "tekton.dev/pipeline": "build"},
					"summary": map[string]any{"type": "tekton.dev/v1.PipelineRun", "status": "FAILURE", "start_time": "s", "end_time": "e",
						"annotations": map[string]string{"commit": "abc123", "eventType": "push"}}},
				map[string]any{"name": "app-shop-cicd/results/x", "create_time": now.Add(-1 * time.Hour).Format(time.RFC3339),
					"summary": map[string]any{"type": "tekton.dev/v1.TaskRun"}},
				map[string]any{"name": "app-shop-cicd/results/old", "create_time": now.Add(-48 * time.Hour).Format(time.RFC3339),
					"summary": map[string]any{"type": "tekton.dev/v1.PipelineRun", "status": "SUCCESS"}},
			}})
		case p == "/parents/app-shop-cicd/results/"+resultID+"/records":
			pr := map[string]any{"apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
				"metadata": map[string]any{"name": "shop-push-abc", "namespace": "app-shop-cicd", "uid": resultID, "managedFields": []any{1},
					"annotations": map[string]any{"tekton.dev/pipeline": "build", "kubectl.kubernetes.io/last-applied-configuration": "{}"}},
				"spec":   map[string]any{"params": []any{map[string]any{"name": "a", "value": "b"}}, "taskRunTemplate": map[string]any{}},
				"status": map[string]any{"completionTime": "e", "provenance": map[string]any{}, "pipelineSpec": map[string]any{"tasks": []any{map[string]any{"name": "build", "taskSpec": map[string]any{}}}}}}
			tr := map[string]any{"apiVersion": "tekton.dev/v1", "kind": "TaskRun", "metadata": map[string]any{"name": "shop-push-abc-build", "labels": map[string]any{"x": "y"}},
				"status": map[string]any{"podName": "p", "steps": []any{map[string]any{"name": "compile", "imageID": "sha", "terminated": map[string]any{"exitCode": 1}}}}}
			lg := map[string]any{"spec": map[string]any{"resource": map[string]any{"name": "shop-push-abc-build"}}}
			json.NewEncoder(w).Encode(map[string]any{"records": []any{
				map[string]any{"name": "app-shop-cicd/results/" + resultID + "/records/a", "data": map[string]any{"type": "tekton.dev/v1.PipelineRun", "value": b64(pr)}},
				map[string]any{"name": "app-shop-cicd/results/" + resultID + "/records/b", "data": map[string]any{"type": "tekton.dev/v1.TaskRun", "value": b64(tr)}},
				map[string]any{"name": "app-shop-cicd/results/" + resultID + "/records/" + logID, "data": map[string]any{"type": "results.tekton.dev/v1alpha3.Log", "value": b64(lg)}},
			}})
		case p == "/parents/app-shop-cicd/results/"+resultID+"/logs/"+logID:
			io.WriteString(w, "[compile] error: no space left on device\n")
		default:
			t.Logf("results stub: unexpected %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
}

func newServer(t *testing.T, allow map[string]bool) (*Server, func()) {
	stub := resultsStub(t)
	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-shop-cicd", CreationTimestamp: metav1.NewTime(now.Add(-30 * 24 * time.Hour))}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "release-tracking-shop-1", Namespace: "app-shop-cicd",
			Labels: map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": "shop"}}, Data: map[string]string{"state": "healthy"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "release-tracking-other-1", Namespace: "app-shop-cicd",
			Labels: map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": "other"}}},
	)
	s := &Server{
		Auth:     &auth.Auth{Reviewer: fakeReviewer{allow}, Delegates: map[string]bool{"tower": true}},
		Apps:     &apps.Resolver{CS: cs, TTL: time.Minute},
		Results:  &results.Client{Base: stub.URL, HTTP: stub.Client()},
		Releases: releases.Reader{CS: cs},
		Contract: []byte(`{"contractVersion":1}`),
		Log:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:      func() time.Time { return now },
	}
	return s, stub.Close
}

func call(t *testing.T, s *Server, path string, hdr map[string]string) (int, map[string]any, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	return rec.Code, out, body
}

var reader = map[string]bool{"tower *": true}

func TestAuthentication(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	cases := []struct {
		name   string
		hdr    map[string]string
		status int
		rule   string
	}{
		{"no token", nil, 401, "GP-AUTH-001"},
		{"rejected token", map[string]string{"Authorization": "Bearer nope"}, 401, "GP-AUTH-002"},
		{"authorization header", map[string]string{"Authorization": "Bearer t-tower"}, 200, ""},
		{"through the service proxy", map[string]string{auth.HeaderProxied: "Bearer t-tower"}, 200, ""},
		{"delegate on behalf of", map[string]string{"Authorization": "Bearer t-tower", auth.HeaderOnBehalf: "user:default/jfillman"}, 200, ""},
		{"non-delegate on behalf of", map[string]string{"Authorization": "Bearer t-agent", auth.HeaderOnBehalf: "user:default/jfillman"}, 403, "GP-AUTH-003"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, out, _ := call(t, s, "/v1/whoami", c.hdr)
			if status != c.status || (c.rule != "" && out["rule"] != c.rule) {
				t.Fatalf("got %d %v, want %d %s", status, out, c.status, c.rule)
			}
		})
	}
	_, out, _ := call(t, s, "/v1/whoami", map[string]string{"Authorization": "Bearer t-tower", auth.HeaderOnBehalf: "user:default/jfillman"})
	if out["user"] != "tower" || out["onBehalfOf"] != "user:default/jfillman" {
		t.Fatalf("whoami: %v", out)
	}
}

func TestAuthorization(t *testing.T) {
	s, done := newServer(t, map[string]bool{"agent list runs app-shop-cicd": true})
	defer done()
	h := map[string]string{"Authorization": "Bearer t-agent"}
	if st, _, _ := call(t, s, "/v1/apps/shop/runs", h); st != 200 {
		t.Fatalf("list runs: %d", st)
	}
	if st, out, _ := call(t, s, "/v1/apps/shop/releases", h); st != 403 || out["rule"] != "GP-AUTHZ-001" {
		t.Fatalf("releases without the grant: %d %v", st, out)
	}
	if st, _, _ := call(t, s, "/v1/apps/shop/runs/"+resultID+"/logs/shop-push-abc-build", h); st != 403 {
		t.Fatalf("logs need runs/logs: %d", st)
	}
	// An app that doesn't exist reads as forbidden to someone who couldn't read it anyway...
	if st, _, _ := call(t, s, "/v1/apps/nope/runs", h); st != 403 {
		t.Fatalf("missing app, no grant: %d", st)
	}
	// ...and as missing to someone who could.
	s2, done2 := newServer(t, reader)
	defer done2()
	if st, out, _ := call(t, s2, "/v1/apps/nope/runs", map[string]string{"Authorization": "Bearer t-tower"}); st != 404 || out["rule"] != "GP-API-404" {
		t.Fatalf("missing app: %d %v", st, out)
	}
	if st, _, _ := call(t, s2, "/v1/apps/Bad_Name/runs", map[string]string{"Authorization": "Bearer t-tower"}); st != 400 {
		t.Fatalf("invalid app: %d", st)
	}
}

func TestRuns(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	h := map[string]string{"Authorization": "Bearer t-tower"}
	_, out, body := call(t, s, "/v1/apps/shop/runs?sinceHours=24", h)
	runs := out["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("want the one PipelineRun inside the window, got %s", body)
	}
	row := runs[0].(map[string]any)
	want := map[string]any{"result": resultID, "name": "shop-push-abc", "pipeline": "build", "status": "failed", "startTime": "s", "endTime": "e", "commit": "abc123", "eventType": "push"}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %v, want %v", k, row[k], v)
		}
	}
	if _, ok := row["prNumber"]; ok {
		t.Errorf("an absent field is omitted, as Tower's undefined is: %v", row)
	}
	if _, out, _ := call(t, s, "/v1/apps/shop/runs?sinceHours=72", h); len(out["runs"].([]any)) != 2 {
		t.Fatalf("72 h window: %v", out)
	}
}

func TestRunTrimmedLikeTower(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	h := map[string]string{"Authorization": "Bearer t-tower"}
	st, out, body := call(t, s, "/v1/apps/shop/runs/"+resultID, h)
	if st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	pr := out["pipelineRun"].(map[string]any)
	meta := pr["metadata"].(map[string]any)
	if _, ok := meta["managedFields"]; ok {
		t.Error("metadata keeps only name, namespace, uid, creationTimestamp, labels, annotations")
	}
	ann := meta["annotations"].(map[string]any)
	if _, ok := ann["kubectl.kubernetes.io/last-applied-configuration"]; ok || ann["tekton.dev/pipeline"] != "build" {
		t.Errorf("annotations filtered by prefix: %v", ann)
	}
	if _, ok := meta["labels"].(map[string]any); !ok {
		t.Error("labels default to {}")
	}
	stat := pr["status"].(map[string]any)
	if _, ok := stat["provenance"]; ok {
		t.Error("status keeps only Tower's fields")
	}
	task := stat["pipelineSpec"].(map[string]any)["tasks"].([]any)[0].(map[string]any)
	if _, ok := task["taskSpec"]; ok || task["name"] != "build" {
		t.Errorf("pipelineSpec tasks trimmed: %v", task)
	}
	if fin := stat["pipelineSpec"].(map[string]any)["finally"].([]any); len(fin) != 0 {
		t.Error("finally defaults to []")
	}
	spec := pr["spec"].(map[string]any)
	if _, ok := spec["taskRunTemplate"]; ok {
		t.Error("spec keeps params and pipelineRef only")
	}
	step := out["taskRuns"].([]any)[0].(map[string]any)["status"].(map[string]any)["steps"].([]any)[0].(map[string]any)
	if _, ok := step["imageID"]; ok || step["name"] != "compile" {
		t.Errorf("steps trimmed: %v", step)
	}
	st, _, text := call(t, s, "/v1/apps/shop/runs/"+resultID+"/logs/shop-push-abc-build", h)
	if st != 200 || !strings.Contains(text, "no space left on device") {
		t.Fatalf("log: %d %s", st, text)
	}
	if st, out, _ := call(t, s, "/v1/apps/shop/runs/"+resultID+"/logs/missing", h); st != 404 || out["rule"] != "GP-LOG-404" {
		t.Fatalf("missing log: %d %v", st, out)
	}
	if st, out, _ := call(t, s, "/v1/apps/shop/runs/"+strings.Repeat("0", 8)+"-0000-0000-0000-000000000000/logs/x", h); st != 404 || out["rule"] != "GP-API-404" {
		t.Fatalf("missing run is not 'no log': %d %v", st, out)
	}
	if st, _, _ := call(t, s, "/v1/apps/shop/runs/not-a-uid", h); st != 400 {
		t.Fatalf("bad result id: %d", st)
	}
	_, full, _ := call(t, s, "/v1/apps/shop/runs?view=full", h)
	if runs := full["runs"].([]any); len(runs) != 1 || runs[0].(map[string]any)["pipelineRun"] == nil {
		t.Fatalf("view=full: %v", full)
	}
}

func TestReleases(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	h := map[string]string{"Authorization": "Bearer t-tower"}
	_, out, body := call(t, s, "/v1/apps/shop/releases", h)
	recs := out["releases"].([]any)
	if len(recs) != 1 || recs[0].(map[string]any)["name"] != "release-tracking-shop-1" {
		t.Fatalf("only this app's records: %s", body)
	}
	if st, out, _ := call(t, s, "/v1/apps/shop/releases/shop-1", h); st != 200 || out["data"].(map[string]any)["state"] != "healthy" {
		t.Fatalf("get: %d %v", st, out)
	}
	if st, _, _ := call(t, s, "/v1/apps/shop/releases/other-1", h); st != 404 {
		t.Fatalf("another app's record: %d", st)
	}
}

func TestContract(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	if _, out, _ := call(t, s, "/v1/contract", map[string]string{"Authorization": "Bearer t-anyone"}); out["contractVersion"] != float64(1) {
		t.Fatalf("contract: %v", out)
	}
}
