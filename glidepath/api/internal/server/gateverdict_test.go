package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/gateverdict"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// audiencesSeen is what the last audience TokenReview asked for.
var audiencesSeen []string

// TokenReviewAudiences: "pod-<user>" authenticates as <user>, bound to the comment-pr pod of sast-abcde, for the gate-verdict
// audience only (any other audience: not authenticated, as the API server answers). "t-<user>" (a default token) is never valid
// for it.
func (f fakeReviewer) TokenReviewAudiences(_ context.Context, token string, aud []string) (*authenticationv1.TokenReviewStatus, error) {
	audiencesSeen = aud
	if !strings.HasPrefix(token, "pod-") || len(aud) != 1 || aud[0] != gateverdict.Audience {
		return &authenticationv1.TokenReviewStatus{Authenticated: false, Error: "token audiences do not match"}, nil
	}
	return &authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: aud, User: authenticationv1.UserInfo{
		Username: token[4:],
		Extra: map[string]authenticationv1.ExtraValue{
			auth.ExtraPodName: {"sast-abcde-comment-pr-pod"},
			auth.ExtraPodUID:  {"pod-uid"},
		},
	}}, nil
}

const gvSha = "0123456789abcdef0123456789abcdef01234567"

func withGateVerdict(t *testing.T) (*Server, *fake.Clientset) {
	s, done := newServer(t, map[string]bool{"tower *": true})
	t.Cleanup(done)
	ns := "app-shop-cicd"
	pr := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
		"metadata": map[string]any{"name": "sast-abcde", "namespace": ns, "uid": "run-uid", "creationTimestamp": "2026-10-25T11:55:00Z",
			"labels":      map[string]any{"pipelinesascode.tekton.dev/pull-request": "12"},
			"annotations": map[string]any{"pipelinesascode.tekton.dev/sha": gvSha, "pipelinesascode.tekton.dev/repo-url": "https://github.com/jfillman/gitops-shop"}},
		"spec":   map[string]any{"timeouts": map[string]any{"tasks": "14m0s"}},
		"status": map[string]any{"startTime": "2026-10-25T11:55:00Z", "pipelineSpec": map[string]any{"tasks": []any{map[string]any{"name": "scan"}}}}}}
	tr := func(name, task, status string, extra map[string]any) *unstructured.Unstructured {
		o := map[string]any{"apiVersion": "tekton.dev/v1", "kind": "TaskRun",
			"metadata": map[string]any{"name": name, "namespace": ns,
				"labels":          map[string]any{"tekton.dev/pipelineRun": "sast-abcde", "tekton.dev/pipelineTask": task},
				"ownerReferences": []any{map[string]any{"apiVersion": "tekton.dev/v1", "kind": "PipelineRun", "name": "sast-abcde", "uid": "run-uid"}}},
			"status": map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": status}}}}
		for k, v := range extra {
			o[k] = v
		}
		return &unstructured.Unstructured{Object: o}
	}
	scan := tr("sast-abcde-scan", "scan", "False", nil)
	comment := tr("sast-abcde-comment-pr", "comment-pr", "Unknown", map[string]any{
		"spec": map[string]any{"params": []any{map[string]any{"name": "gate-name", "value": "sast"}}}})
	comment.Object["status"].(map[string]any)["podName"] = "sast-abcde-comment-pr-pod"
	gvr := func(r string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: r}
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr("pipelineruns"): "PipelineRunList", gvr("taskruns"): "TaskRunList"}, pr, scan, comment)
	cs := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sast-abcde-comment-pr-pod", Namespace: ns, UID: types.UID("pod-uid"),
			Labels: map[string]string{"tekton.dev/pipelineRun": "sast-abcde", "tekton.dev/pipelineTask": "comment-pr",
				"tekton.dev/task": "comment-pr-check-result", "tekton.dev/memberOf": "finally", "tekton.dev/taskRun": "sast-abcde-comment-pr"}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "release-tracking-shop-1", Namespace: ns, ResourceVersion: "7",
			Labels: map[string]string{"hangar.io/subcomponent": "release-tracking"}},
			Data: map[string]string{"prUrl": "https://github.com/jfillman/gitops-shop/pull/12", "gate.0123456789ab.sbom": "success"}},
	)
	s.GateVerdict = &gateverdict.Service{Store: gateverdict.KubeStore{Dyn: dyn, CS: cs}, ServiceAccounts: []string{"pipeline-runner"},
		Now: func() time.Time { return now }}
	return s, cs
}

func postGV(t *testing.T, s *Server, path, token string, body string, hdr ...string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

const gvPath = "/v1/apps/shop/pipelineruns/sast-abcde/gate-verdict"

func TestGateVerdictRecordsTheClustersVerdictNotTheBodys(t *testing.T) {
	s, cs := withGateVerdict(t)
	// The body claims success; the scan TaskRun failed. The body is never read.
	code, out := postGV(t, s, gvPath, "pod-system:serviceaccount:app-shop-cicd:pipeline-runner",
		`{"verdict":"success","gate":"image-scan","sha":"ffffffffffffffffffffffffffffffffffffffff","pullRequest":99}`)
	if code != 200 || out["outcome"] != "recorded" || out["verdict"] != "failure" || out["gate"] != "sast" || out["sha"] != gvSha || out["pullRequest"] != float64(12) {
		t.Fatalf("%d %v", code, out)
	}
	if len(audiencesSeen) != 1 || audiencesSeen[0] != "glidepath-gate-verdict" {
		t.Fatalf("the TokenReview names the route's audience: %v", audiencesSeen)
	}
	cm, _ := cs.CoreV1().ConfigMaps("app-shop-cicd").Get(context.Background(), "release-tracking-shop-1", metav1.GetOptions{})
	if cm.Data["gate.0123456789ab.sast"] != "failure" || cm.Data["gateRun.0123456789ab.sast"] != "2026-10-25T11:55:00Z sast-abcde" || cm.Data["gate.0123456789ab.sbom"] != "success" {
		t.Fatalf("the record: %v", cm.Data)
	}
	ledger, _ := out["ledger"].(map[string]any)
	if len(ledger) != 2 {
		t.Fatalf("the answer's ledger: %v", out)
	}
	// Again: nothing more is written.
	if code, out := postGV(t, s, gvPath, "pod-system:serviceaccount:app-shop-cicd:pipeline-runner", ""); code != 200 || out["outcome"] != "already-recorded" {
		t.Fatalf("a retry: %d %v", code, out)
	}
}

func TestGateVerdictRefusals(t *testing.T) {
	s, cs := withGateVerdict(t)
	cases := []struct {
		name, token, path string
		hdr               []string
		status            int
		rule              string
	}{
		{"no token", "", gvPath, nil, 401, "GP-AUTH-001"},
		{"the pod's default token (no audience)", "t-system:serviceaccount:app-shop-cicd:pipeline-runner", gvPath, nil, 401, "GP-AUTH-002"},
		{"on behalf of someone", "pod-system:serviceaccount:app-shop-cicd:pipeline-runner", gvPath, []string{auth.HeaderOnBehalf, "user:default/x"}, 403, "GP-AUTH-003"},
		{"another account", "pod-system:serviceaccount:app-shop-cicd:release-record-writer", gvPath, nil, 403, "GP-GV-403"},
		{"tower", "pod-tower", gvPath, nil, 403, "GP-GV-403"},
		{"another run's verdict", "pod-system:serviceaccount:app-shop-cicd:pipeline-runner", "/v1/apps/shop/pipelineruns/sbom-zzzzz/gate-verdict", nil, 403, "GP-GV-403"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := postGV(t, s, c.path, c.token, "", c.hdr...)
			if code != c.status || out["rule"] != c.rule {
				t.Fatalf("got %d %v, want %d %s", code, out, c.status, c.rule)
			}
		})
	}
	cm, _ := cs.CoreV1().ConfigMaps("app-shop-cicd").Get(context.Background(), "release-tracking-shop-1", metav1.GetOptions{})
	if _, ok := cm.Data["gate.0123456789ab.sast"]; ok {
		t.Fatalf("a refused call wrote: %v", cm.Data)
	}
	s.GateVerdict = nil
	if code, out := postGV(t, s, gvPath, "pod-system:serviceaccount:app-shop-cicd:pipeline-runner", ""); code != 503 {
		t.Fatalf("unconfigured: %d %v", code, out)
	}
}

func TestGateVerdictDryRun(t *testing.T) {
	s, cs := withGateVerdict(t)
	code, out := postGV(t, s, gvPath+"?dryRun=true", "pod-system:serviceaccount:app-shop-cicd:pipeline-runner", "")
	if code != 200 || out["dryRun"] != true || out["verdict"] != "failure" {
		t.Fatalf("%d %v", code, out)
	}
	cm, _ := cs.CoreV1().ConfigMaps("app-shop-cicd").Get(context.Background(), "release-tracking-shop-1", metav1.GetOptions{})
	if _, ok := cm.Data["gate.0123456789ab.sast"]; ok {
		t.Fatalf("a dry run wrote: %v", cm.Data)
	}
}
