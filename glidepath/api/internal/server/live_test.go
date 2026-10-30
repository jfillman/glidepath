package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/glidepath/api/internal/live"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeLive is the cluster's PipelineRuns, TaskRuns and pods, across namespaces. A pod's log is
// "<timestamp> <pod>/<container>".
type fakeLive struct {
	mu    sync.Mutex
	objs  map[string][]map[string]any // by resource
	pods  map[string][]string         // "<ns>/<pod>" -> containers
	lists int
}

func (f *fakeLive) List(_ context.Context, gvr schema.GroupVersionResource, ns, selector string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	sel, err := labels.Parse(selector)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	var out []map[string]any
	for _, o := range f.objs[gvr.Resource] {
		meta := o["metadata"].(map[string]any)
		l := labels.Set{}
		for k, v := range meta["labels"].(map[string]any) {
			l[k] = v.(string)
		}
		if (ns == "" || meta["namespace"] == ns) && sel.Matches(l) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeLive) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (map[string]any, error) {
	for _, o := range f.objs[gvr.Resource] {
		meta := o["metadata"].(map[string]any)
		if meta["namespace"] == ns && meta["name"] == name {
			return o, nil
		}
	}
	return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
}

func (f *fakeLive) Containers(_ context.Context, ns, pod string) ([]string, error) {
	c, ok := f.pods[ns+"/"+pod]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pod)
	}
	return c, nil
}

func (f *fakeLive) Log(_ context.Context, ns, pod, container string) (io.ReadCloser, error) {
	if container == "step-waiting" {
		return nil, apierrors.NewBadRequest(`container "step-waiting" in pod "` + pod + `" is waiting to start: PodInitializing`)
	}
	return io.NopCloser(strings.NewReader("2026-10-29T12:00:00.000000000Z " + pod + "/" + container + "\n")), nil
}

func liveObj(kind, ns, name string, lbl map[string]any, status map[string]any) map[string]any {
	if lbl == nil {
		lbl = map[string]any{}
	}
	o := map[string]any{"apiVersion": "tekton.dev/v1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": lbl, "resourceVersion": "7",
			"managedFields": []any{map[string]any{"manager": "controller"}}},
		"spec": map[string]any{"params": []any{}}}
	if status != nil {
		o["status"] = status
	}
	return o
}

func withLive(s *Server) *fakeLive {
	f := &fakeLive{
		objs: map[string][]map[string]any{
			"pipelineruns": {
				liveObj("PipelineRun", "app-shop-cicd", "shop-push-1", map[string]any{"hangar.io/app": "shop"}, nil),
				liveObj("PipelineRun", "app-cart-cicd", "cart-push-1", nil, nil),
			},
			"taskruns": {
				liveObj("TaskRun", "app-shop-cicd", "shop-push-1-build", map[string]any{live.LabelPipelineRun: "shop-push-1"},
					map[string]any{"podName": "shop-push-1-build-pod"}),
				liveObj("TaskRun", "app-shop-cicd", "shop-push-1-scan", map[string]any{live.LabelPipelineRun: "shop-push-1"},
					map[string]any{"podName": "shop-push-1-scan-pod"}),
				liveObj("TaskRun", "app-shop-cicd", "shop-push-1-test", map[string]any{live.LabelPipelineRun: "shop-push-1"}, map[string]any{}),
				liveObj("TaskRun", "app-cart-cicd", "cart-push-1-build", map[string]any{live.LabelPipelineRun: "cart-push-1"}, nil),
			},
		},
		pods: map[string][]string{"app-shop-cicd/shop-push-1-build-pod": {"prepare", "step-compile", "step-waiting"}},
	}
	s.Live = &live.Client{Store: f}
	return f
}

func names(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %v", v)
	}
	out := []string{}
	for _, o := range list {
		out = append(out, o.(map[string]any)["metadata"].(map[string]any)["name"].(string))
	}
	return out
}

func TestLiveRuns(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	f := withLive(s)
	h := map[string]string{"Authorization": "Bearer t-tower"}
	st, out, body := call(t, s, "/v1/apps/shop/live", h)
	if st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	if got := names(t, out["pipelineRuns"]); len(got) != 1 || got[0] != "shop-push-1" {
		t.Fatalf("only the app's namespace: %v", got)
	}
	if got := names(t, out["taskRuns"]); len(got) != 3 {
		t.Fatalf("every TaskRun in the namespace: %v", got)
	}
	meta := out["pipelineRuns"].([]any)[0].(map[string]any)["metadata"].(map[string]any)
	if _, ok := meta["managedFields"]; ok || meta["resourceVersion"] != "7" || meta["labels"].(map[string]any)["hangar.io/app"] != "shop" {
		t.Fatalf("managedFields dropped, nothing else: %v", meta)
	}
	if _, ok := f.objs["pipelineruns"][0]["metadata"].(map[string]any)["managedFields"]; !ok {
		t.Fatal("stripping must copy, not change what the store (or the cache) holds")
	}
	if f.lists != 2 {
		t.Fatalf("one list each of PipelineRuns and TaskRuns, got %d", f.lists)
	}
	call(t, s, "/v1/apps/shop/live", h)
	if f.lists != 2 {
		t.Fatalf("a second read inside %s is cached, got %d lists", live.ListTTL, f.lists)
	}
	if st, out, _ := call(t, s, "/v1/apps/nope/live", h); st != 404 || out["rule"] != "GP-API-404" {
		t.Fatalf("missing app: %d %v", st, out)
	}
}

func TestLiveLog(t *testing.T) {
	s, done := newServer(t, map[string]bool{"tower get runs/logs app-shop-cicd": true, "lister list runs app-shop-cicd": true})
	defer done()
	withLive(s)
	req := httptest.NewRequest(http.MethodGet, "/v1/apps/shop/live/taskruns/shop-push-1-build/logs/step-compile", nil)
	req.Header.Set("Authorization", "Bearer t-tower")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "2026-10-29T12:00:00.000000000Z shop-push-1-build-pod/step-compile\n" ||
		rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("log: %d %q %s", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	h := map[string]string{"Authorization": "Bearer t-tower"}
	cases := []struct {
		name, path string
		status     int
		rule       string
	}{
		{"init container", "shop-push-1-build/logs/prepare", 200, ""},
		{"container the pod lacks", "shop-push-1-build/logs/step-nope", 400, "GP-API-400"},
		{"step not started", "shop-push-1-build/logs/step-waiting", 400, "GP-API-400"},
		{"malformed container", "shop-push-1-build/logs/Step_X", 400, "GP-API-400"},
		{"malformed TaskRun", "Shop..1/logs/step-compile", 400, "GP-API-400"},
		{"TaskRun gone", "shop-push-0-build/logs/step-compile", 404, "GP-LIVE-404"},
		{"pod gone", "shop-push-1-scan/logs/step-compile", 404, "GP-LIVE-404"},
		{"no pod yet", "shop-push-1-test/logs/step-compile", 404, "GP-LIVE-404"},
		{"another app's TaskRun", "cart-push-1-build/logs/step-compile", 404, "GP-LIVE-404"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, out, body := call(t, s, "/v1/apps/shop/live/taskruns/"+c.path, h)
			if st != c.status || (c.rule != "" && out["rule"] != c.rule) {
				t.Fatalf("got %d %s, want %d %s", st, body, c.status, c.rule)
			}
		})
	}
	// A live log needs runs/logs, as an archived one does.
	if st, _, _ := call(t, s, "/v1/apps/shop/live/taskruns/shop-push-1-build/logs/step-compile", map[string]string{"Authorization": "Bearer t-lister"}); st != 403 {
		t.Fatalf("live log with list runs only: %d", st)
	}
}

func TestFleetLive(t *testing.T) {
	s, done := newServer(t, map[string]bool{"tower *": true, "shopper list runs app-shop-cicd": true})
	defer done()
	withLive(s)
	tower, shopper := map[string]string{"Authorization": "Bearer t-tower"}, map[string]string{"Authorization": "Bearer t-shopper"}
	if _, out, _ := call(t, s, "/v1/live", tower); len(names(t, out["pipelineRuns"])) != 2 {
		t.Fatalf("every namespace: %v", out)
	}
	if _, out, _ := call(t, s, "/v1/live?app=shop", tower); len(names(t, out["pipelineRuns"])) != 1 {
		t.Fatalf("app=: %v", out)
	}
	if st, out, _ := call(t, s, "/v1/live", shopper); st != 403 || out["rule"] != "GP-AUTHZ-001" || !strings.Contains(out["hint"].(string), "app=") {
		t.Fatalf("a per-app reader can't list cluster-wide: %d %v", st, out)
	}
	if st, _, _ := call(t, s, "/v1/live?app=shop", shopper); st != 200 {
		t.Fatalf("a per-app reader with app=: %d", st)
	}
	_, out, body := call(t, s, "/v1/live/taskruns?pipelineRuns=shop-push-1,cart-push-1,gone-1", tower)
	if got := names(t, out["taskRuns"]); len(got) != 4 {
		t.Fatalf("the named runs' TaskRuns, every namespace: %s", body)
	}
	if strings.Contains(body, "managedFields") {
		t.Fatal("managedFields dropped")
	}
	if _, out, _ := call(t, s, "/v1/live/taskruns?pipelineRuns=cart-push-1&app=shop", tower); len(names(t, out["taskRuns"])) != 0 {
		t.Fatalf("app= keeps to its namespace: %v", out)
	}
	for _, q := range []string{"", "?pipelineRuns=", "?pipelineRuns=,"} {
		if st, out, body := call(t, s, "/v1/live/taskruns"+q, tower); st != 200 || names(t, out["taskRuns"]) == nil || !strings.Contains(body, `"taskRuns":[]`) {
			t.Fatalf("%q: none asked, none answered: %d %s", q, st, body)
		}
	}
	many := strings.TrimSuffix(strings.Repeat("r-x,", live.MaxPipelineRuns+1), ",")
	if st, _, _ := call(t, s, "/v1/live/taskruns?pipelineRuns="+many, tower); st != 200 {
		t.Fatalf("duplicates count once: %d", st)
	}
	var distinct []string
	for i := 0; i <= live.MaxPipelineRuns; i++ {
		distinct = append(distinct, "r-"+strings.Repeat("a", 1+i%50)+"-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	for _, q := range []string{"Bad_Name", "a)b", strings.Repeat("a", 64), strings.Join(distinct, ",")} {
		if st, out, _ := call(t, s, "/v1/live/taskruns?pipelineRuns="+q, tower); st != 400 || out["rule"] != "GP-API-400" {
			t.Fatalf("%.40q: %d %v", q, st, out)
		}
	}
}

func TestFleetRuns(t *testing.T) {
	s, done := newServer(t, map[string]bool{"tower *": true, "shopper list runs app-shop-cicd": true})
	defer done()
	tower := map[string]string{"Authorization": "Bearer t-tower"}
	st, out, body := call(t, s, "/v1/runs?sinceHours=24", tower)
	if st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	runs := out["runs"].([]any)
	if len(runs) != 3 {
		t.Fatalf("every namespace inside the window: %s", body)
	}
	want := []struct{ ns, app, status string }{{"app-shop-cicd", "shop", "failed"}, {"infra-queue-cicd", "queue", "succeeded"}, {"platform-system", "", "cancelled"}}
	for i, w := range want {
		row := runs[i].(map[string]any)
		app, hasApp := row["app"]
		if row["namespace"] != w.ns || row["status"] != w.status || (w.app == "" && hasApp) || (w.app != "" && app != w.app) {
			t.Errorf("row %d: %v, want %+v", i, row, w)
		}
	}
	if row := runs[0].(map[string]any); row["result"] != resultID || row["name"] != "shop-push-abc" {
		t.Errorf("a fleet row is a Row: %v", row)
	}
	if _, out, _ := call(t, s, "/v1/runs?sinceHours=72&limit=2", tower); len(out["runs"].([]any)) != 2 {
		t.Fatalf("limit: %v", out)
	}
	if st, out, _ := call(t, s, "/v1/runs", map[string]string{"Authorization": "Bearer t-shopper"}); st != 403 || out["rule"] != "GP-AUTHZ-001" {
		t.Fatalf("a per-app reader can't list cluster-wide: %d %v", st, out)
	}
	st, out, body = call(t, s, "/v1/runs?app=shop", map[string]string{"Authorization": "Bearer t-shopper"})
	if st != 200 || len(out["runs"].([]any)) != 1 {
		t.Fatalf("app=: %d %s", st, body)
	}
	if row := out["runs"].([]any)[0].(map[string]any); row["namespace"] != "app-shop-cicd" || row["app"] != "shop" || row["commit"] != "abc123" {
		t.Fatalf("app= row: %v", row)
	}
}
