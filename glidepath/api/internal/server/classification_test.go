package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glidepath/api/internal/changekind"
	"github.com/glidepath/api/internal/releases"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	ckBase = "1111111111111111111111111111111111111111"
	ckHead = "2222222222222222222222222222222222222222"
)

// ckHost is GitHub for gitops-shop#7, whose one change is its README: a no-op.
type ckHost struct{ calls []string }

func (h *ckHost) GetJSON(_ context.Context, owner, repo, p string) (int, []byte, error) {
	h.calls = append(h.calls, owner+"/"+repo+" "+p)
	switch p {
	case "pulls/7":
		return 200, []byte(`{"number":7,"html_url":"https://github.com/acme/gitops-shop/pull/7","base":{"sha":"` + ckBase + `"},"head":{"sha":"` + ckHead + `"}}`), nil
	case "compare/" + ckBase + "..." + ckHead:
		return 200, []byte(`{"merge_base_commit":{"sha":"` + ckBase + `"},"files":[{"filename":"README.md","status":"modified"}]}`), nil
	}
	return 404, nil, nil
}

func withChangeKind(t *testing.T, allow map[string]bool) (*Server, *ckHost, *fake.Clientset) {
	s, done := newServer(t, allow)
	t.Cleanup(done)
	h := &ckHost{}
	cs := fake.NewSimpleClientset()
	s.ChangeKind = &changekind.Service{Owner: "acme", GitHub: func(string) changekind.Reader { return h }, Records: releases.Reader{CS: cs},
		Store: changekind.KubeStore{CS: cs}, Now: s.Now}
	return s, h, cs
}

func ckCall(t *testing.T, s *Server, method, path string) (int, map[string]any) {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer t-dev")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

var ckGate = map[string]bool{"dev create classifications app-shop-cicd": true, "dev get classifications app-shop-cicd": true}

func TestClassificationRoutes(t *testing.T) {
	s, h, cs := withChangeKind(t, ckGate)
	path := "/v1/apps/shop/pulls/7/classification?head=" + ckHead
	if code, out := ckCall(t, s, http.MethodGet, path); code != http.StatusNotFound || out["rule"] != "GP-API-404" {
		t.Fatalf("nothing stored yet: %d %v", code, out)
	}
	code, out := ckCall(t, s, http.MethodPost, path)
	if code != http.StatusOK || out["kind"] != "no-op" || out["outcome"] != "computed" || out["head"] != ckHead || out["recordCopy"] != "no-record" {
		t.Fatalf("%d %v", code, out)
	}
	cm, err := cs.CoreV1().ConfigMaps("app-shop-cicd").Get(context.Background(), "change-kind-pr-7", metav1.GetOptions{})
	if err != nil || cm.Labels["hangar.io/subcomponent"] != "change-kind" || !strings.Contains(cm.Data[ckHead[:12]], `"kind":"no-op"`) {
		t.Fatalf("%v %+v", err, cm)
	}
	n := len(h.calls)
	if code, out = ckCall(t, s, http.MethodGet, path); code != http.StatusOK || out["kind"] != "no-op" || out["outcome"] != "stored" || len(h.calls) != n {
		t.Fatalf("GET reads the store and nothing else: %d %v", code, out)
	}
	if code, out = ckCall(t, s, http.MethodPost, path); code != http.StatusOK || out["outcome"] != "stored" || len(h.calls) != n {
		t.Fatalf("a second POST computes nothing: %d %v", code, out)
	}
	if h.calls[0] != "acme/gitops-shop pulls/7" {
		t.Fatalf("only the app's gitops repository is read: %v", h.calls)
	}
}

func TestClassificationDryRunStoresNothing(t *testing.T) {
	s, _, cs := withChangeKind(t, ckGate)
	code, out := ckCall(t, s, http.MethodPost, "/v1/apps/shop/pulls/7/classification?dryRun=true&head="+ckHead)
	if code != http.StatusOK || out["outcome"] != "dry-run" {
		t.Fatalf("%d %v", code, out)
	}
	if l, _ := cs.CoreV1().ConfigMaps("app-shop-cicd").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatalf("%+v", l.Items)
	}
}

func TestClassificationAuthorizationAndInput(t *testing.T) {
	// A reader may GET, not POST: storing an answer is for the gates' task accounts.
	s, h, _ := withChangeKind(t, map[string]bool{"dev get classifications app-shop-cicd": true})
	if code, _ := ckCall(t, s, http.MethodPost, "/v1/apps/shop/pulls/7/classification?head="+ckHead); code != http.StatusForbidden {
		t.Fatalf("%d", code)
	}
	s, h, _ = withChangeKind(t, ckGate)
	for name, p := range map[string]string{
		"no head":    "/v1/apps/shop/pulls/7/classification",
		"short head": "/v1/apps/shop/pulls/7/classification?head=abc",
		"number 0":   "/v1/apps/shop/pulls/0/classification?head=" + ckHead,
		"not a num":  "/v1/apps/shop/pulls/x/classification?head=" + ckHead,
	} {
		if code, out := ckCall(t, s, http.MethodPost, p); code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}
	if len(h.calls) != 0 {
		t.Fatalf("a refused request reaches no host: %v", h.calls)
	}
	if code, out := ckCall(t, s, http.MethodPost, "/v1/apps/shop/pulls/8/classification?head="+ckHead); code != http.StatusNotFound {
		t.Fatalf("no such pull request: %d %v", code, out)
	}
	s, done := newServer(t, ckGate)
	defer done()
	if code, _ := ckCall(t, s, http.MethodPost, "/v1/apps/shop/pulls/7/classification?head="+ckHead); code != http.StatusServiceUnavailable {
		t.Fatalf("%d", code)
	}
}
