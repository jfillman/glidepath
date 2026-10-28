package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/pipelineruns"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// action is one call fakeStore saw.
type action struct {
	verb, name string
	dryRun     bool
	patch      string
}

// fakeStore is PipelineRuns in one namespace, app-shop-cicd. A dry run is checked like a real one
// (AlreadyExists, NotFound) and never stored.
type fakeStore struct {
	mu      sync.Mutex
	objs    map[string]map[string]any
	actions []action
	refuse  error // returned by every create and patch
}

func copyObj(o map[string]any) map[string]any {
	raw, _ := json.Marshal(o)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out
}

func (f *fakeStore) Get(_ context.Context, ns, name string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action{verb: "get", name: name})
	if o, ok := f.objs[name]; ok && ns == "app-shop-cicd" {
		return copyObj(o), nil
	}
	return nil, apierrors.NewNotFound(pipelineruns.GVR.GroupResource(), name)
}

func (f *fakeStore) Create(_ context.Context, ns string, obj map[string]any, dryRun bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := obj["metadata"].(map[string]any)["name"].(string)
	f.actions = append(f.actions, action{verb: "create", name: name, dryRun: dryRun})
	if f.refuse != nil {
		return f.refuse
	}
	if _, ok := f.objs[name]; ok {
		return apierrors.NewAlreadyExists(pipelineruns.GVR.GroupResource(), name)
	}
	if !dryRun {
		f.objs[name] = copyObj(obj)
	}
	return nil
}

func (f *fakeStore) Patch(_ context.Context, ns, name string, patch []byte, dryRun bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action{verb: "patch", name: name, dryRun: dryRun, patch: string(patch)})
	if f.refuse != nil {
		return f.refuse
	}
	o, ok := f.objs[name]
	if !ok {
		return apierrors.NewNotFound(pipelineruns.GVR.GroupResource(), name)
	}
	if !dryRun {
		var p map[string]map[string]any
		json.Unmarshal(patch, &p)
		spec, _ := o["spec"].(map[string]any)
		for k, v := range p["spec"] {
			spec[k] = v
		}
	}
	return nil
}

// writes returns the creates and patches that were not dry runs.
func (f *fakeStore) writes() []action {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []action
	for _, a := range f.actions {
		if a.verb != "get" && !a.dryRun {
			out = append(out, a)
		}
	}
	return out
}

func pipelineRun(name string, cond map[string]any) map[string]any {
	o := map[string]any{
		"apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
		"metadata": map[string]any{"name": name, "namespace": "app-shop-cicd", "uid": "u-" + name,
			"labels":      map[string]any{"hangar.io/app": "shop", "hangar.io/flow": "ci", "triggers.tekton.dev/trigger": "push"},
			"annotations": map[string]any{"pipelinesascode.tekton.dev/sha": "deadbeef"}},
		"spec": map[string]any{
			"pipelineRef": map[string]any{"resolver": "cluster", "params": []any{
				map[string]any{"name": "kind", "value": "pipeline"}, map[string]any{"name": "name", "value": "build"},
				map[string]any{"name": "namespace", "value": "platform-catalog"}}},
			"taskRunTemplate": map[string]any{"serviceAccountName": "pipeline-runner"},
		},
	}
	if cond != nil {
		o["status"] = map[string]any{"conditions": []any{cond}}
	}
	return o
}

func withRuns(t *testing.T, allow map[string]bool) (*Server, *fakeStore, *strings.Builder) {
	s, done := newServer(t, allow)
	t.Cleanup(done)
	store := &fakeStore{objs: map[string]map[string]any{
		"shop-push-abc":  pipelineRun("shop-push-abc", map[string]any{"type": "Succeeded", "status": "False", "reason": "Failed"}),
		"shop-running":   pipelineRun("shop-running", map[string]any{"type": "Succeeded", "status": "Unknown", "reason": "Running"}),
		"shop-succeeded": pipelineRun("shop-succeeded", map[string]any{"type": "Succeeded", "status": "True", "reason": "Succeeded"}),
	}}
	cancelled := pipelineRun("shop-cancelled", map[string]any{"type": "Succeeded", "status": "False", "reason": "Cancelled"})
	cancelled["spec"].(map[string]any)["status"] = "CancelledRunFinally"
	store.objs["shop-cancelled"] = cancelled
	s.Runs = &pipelineruns.Client{Store: store}
	logs := &strings.Builder{}
	s.Log = slog.New(slog.NewJSONHandler(logs, nil))
	return s, store, logs
}

func post(t *testing.T, s *Server, path string, hdr map[string]string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

var operator = map[string]bool{"op rerun runs app-shop-cicd": true, "op cancel runs app-shop-cicd": true}

func hdr(user, key string) map[string]string {
	h := map[string]string{"Authorization": "Bearer t-" + user}
	if key != "" {
		h["Idempotency-Key"] = key
	}
	return h
}

func TestRerunIdempotent(t *testing.T) {
	s, store, logs := withRuns(t, operator)
	const path = "/v1/apps/shop/pipelineruns/shop-push-abc/rerun"
	want := pipelineruns.RerunName("shop-push-abc", pipelineruns.KeyHash("shop", "shop-push-abc", "k1"))

	st, out, body := post(t, s, path, hdr("op", "k1"))
	if st != 201 || out["name"] != want || out["replayed"] != false {
		t.Fatalf("first: %d %s", st, body)
	}
	if _, ok := out["object"]; ok {
		t.Fatal("only a dry run returns the object")
	}
	made := store.objs[want]
	meta := made["metadata"].(map[string]any)
	if ann := meta["annotations"].(map[string]any); ann[pipelineruns.AnnotationRerunOf] != "shop-push-abc" ||
		ann[pipelineruns.AnnotationKeyHash] != pipelineruns.KeyHash("shop", "shop-push-abc", "k1") || len(ann) != 2 {
		t.Fatalf("annotations: %v (no PaC annotation survives)", ann)
	}
	if _, ok := meta["generateName"]; ok {
		t.Fatal("a re-run is named, not generated")
	}
	if l := meta["labels"].(map[string]any); len(l) != 2 || l["hangar.io/flow"] != "ci" {
		t.Fatalf("labels: %v", l)
	}

	// The same key again, as a retry from either replica would send it: one run, answered as a replay.
	st, out, body = post(t, s, path, hdr("op", "k1"))
	if st != 200 || out["name"] != want || out["replayed"] != true {
		t.Fatalf("replay: %d %s", st, body)
	}
	if w := store.writes(); len(w) != 2 || len(store.objs) != 5 {
		t.Fatalf("one run made, the second create refused by the cluster: %v", w)
	}

	// A different key is a different re-run.
	st, out, _ = post(t, s, path, hdr("op", "k2"))
	if st != 201 || out["name"] == want || len(store.objs) != 6 {
		t.Fatalf("second key: %d %v", st, out)
	}

	// A retry after the source was pruned still answers with the run it made.
	delete(store.objs, "shop-push-abc")
	if st, out, body := post(t, s, path, hdr("op", "k1")); st != 200 || out["replayed"] != true {
		t.Fatalf("source pruned: %d %s", st, body)
	}
	if st, out, _ := post(t, s, path, hdr("op", "k3")); st != 404 || out["rule"] != "GP-API-404" {
		t.Fatalf("source gone, new key: %d %v", st, out)
	}

	audit := logs.String()
	for _, f := range []string{`"op":"rerunRun"`, `"tier":"T1"`, `"name":"shop-push-abc"`, `"created":"` + want + `"`, `"dryRun":false`,
		`"idempotencyKeySha256":"` + pipelineruns.KeyHash("shop", "shop-push-abc", "k1")[:12] + `"`, `"replayed":true`} {
		if !strings.Contains(audit, f) {
			t.Errorf("audit lacks %s: %s", f, audit)
		}
	}
}

func TestRerunConflictsAndInput(t *testing.T) {
	s, store, _ := withRuns(t, operator)
	const path = "/v1/apps/shop/pipelineruns/shop-push-abc/rerun"
	// Something else holds the name this key derives.
	taken := pipelineruns.RerunName("shop-push-abc", pipelineruns.KeyHash("shop", "shop-push-abc", "k1"))
	store.objs[taken] = pipelineRun(taken, nil)
	if st, out, _ := post(t, s, path, hdr("op", "k1")); st != 409 || out["rule"] != "GP-API-409" {
		t.Fatalf("foreign object: %d %v", st, out)
	}
	for _, k := range []string{"", "has space", strings.Repeat("k", 129)} {
		if st, out, _ := post(t, s, path, hdr("op", k)); st != 400 || out["rule"] != "GP-API-400" {
			t.Fatalf("key %q: %d %v", k, st, out)
		}
	}
	if st, out, _ := post(t, s, "/v1/apps/shop/pipelineruns/missing/rerun", hdr("op", "k1")); st != 404 || out["rule"] != "GP-API-404" {
		t.Fatalf("no such run: %d %v", st, out)
	}
	if st, _, _ := post(t, s, "/v1/apps/shop/pipelineruns/Bad_Name/rerun", hdr("op", "k1")); st != 400 {
		t.Fatalf("bad name: %d", st)
	}
	if st, _, _ := post(t, s, path+"?dryRun=yes", hdr("op", "k9")); st != 400 {
		t.Fatalf("bad dryRun: %d", st)
	}
	store.refuse = apierrors.NewForbidden(pipelineruns.GVR.GroupResource(), "x", nil)
	if st, out, _ := post(t, s, path, hdr("op", "k8")); st != 422 || out["rule"] != "GP-API-422" {
		t.Fatalf("admission refusal: %d %v", st, out)
	}
	if st, out, _ := post(t, s, "/v1/apps/shop/pipelineruns/shop-running/cancel", hdr("op", "")); st != 422 || out["rule"] != "GP-API-422" {
		t.Fatalf("admission refusal, cancel: %d %v", st, out)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	s, store, logs := withRuns(t, operator)
	st, out, body := post(t, s, "/v1/apps/shop/pipelineruns/shop-push-abc/rerun?dryRun=true", hdr("op", "k1"))
	if st != 200 || out["dryRun"] != true || out["replayed"] != false {
		t.Fatalf("dry rerun: %d %s", st, body)
	}
	obj := out["object"].(map[string]any)
	if obj["metadata"].(map[string]any)["name"] != out["name"] || obj["spec"].(map[string]any)["pipelineRef"] == nil {
		t.Fatalf("the object it would create: %s", body)
	}
	st, out, body = post(t, s, "/v1/apps/shop/pipelineruns/shop-running/cancel?dryRun=true", hdr("op", ""))
	if st != 200 || out["status"] != "cancelled" || out["dryRun"] != true {
		t.Fatalf("dry cancel: %d %s", st, body)
	}
	if p, _ := json.Marshal(out["patch"]); string(p) != `{"spec":{"status":"CancelledRunFinally"}}` {
		t.Fatalf("patch: %s", p)
	}
	if w := store.writes(); len(w) != 0 || len(store.objs) != 4 {
		t.Fatalf("a dry run wrote: %v", w)
	}
	if _, ok := store.objs["shop-running"]["spec"].(map[string]any)["status"]; ok {
		t.Fatal("dry cancel changed the run")
	}
	var dry []action
	for _, a := range store.actions {
		if a.dryRun {
			dry = append(dry, a)
		}
	}
	if len(dry) != 2 || dry[0].verb != "create" || dry[1].verb != "patch" {
		t.Fatalf("both went to the server as dry runs: %v", dry)
	}
	if strings.Count(logs.String(), `"dryRun":true`) != 2 {
		t.Fatalf("audited as dry runs: %s", logs)
	}
}

func TestCancel(t *testing.T) {
	s, store, logs := withRuns(t, operator)
	cases := []struct{ run, status string }{
		{"shop-running", "cancelled"},
		{"shop-running", "already-cancelled"}, // the second cancel sees spec.status
		{"shop-succeeded", "already-finished"},
		{"shop-push-abc", "already-finished"},
		{"shop-cancelled", "already-cancelled"},
	}
	for _, c := range cases {
		st, out, body := post(t, s, "/v1/apps/shop/pipelineruns/"+c.run+"/cancel", hdr("op", ""))
		if st != 200 || out["name"] != c.run || out["status"] != c.status {
			t.Fatalf("%s: %d %s, want %s", c.run, st, body, c.status)
		}
	}
	w := store.writes()
	if len(w) != 1 || w[0].name != "shop-running" || w[0].patch != `{"spec":{"status":"CancelledRunFinally"}}` {
		t.Fatalf("one merge patch, to the running run: %v", w)
	}
	if st, out, _ := post(t, s, "/v1/apps/shop/pipelineruns/missing/cancel", hdr("op", "")); st != 404 || out["rule"] != "GP-API-404" {
		t.Fatalf("no such run: %d %v", st, out)
	}
	if !strings.Contains(logs.String(), `"result":"already-finished"`) {
		t.Fatalf("audit: %s", logs)
	}
}

func TestWriteAuthorization(t *testing.T) {
	allow := map[string]bool{
		"reader list runs app-shop-cicd": true, "reader get runs app-shop-cicd": true,
		"rerunner rerun runs app-shop-cicd": true, "canceller cancel runs app-shop-cicd": true,
		"tower *": true,
	}
	s, store, logs := withRuns(t, allow)
	rerun, cancel := "/v1/apps/shop/pipelineruns/shop-running/rerun", "/v1/apps/shop/pipelineruns/shop-running/cancel"
	for _, c := range []struct {
		user, path string
		status     int
		rule       string
	}{
		{"reader", rerun, 403, "GP-AUTHZ-001"},
		{"reader", cancel, 403, "GP-AUTHZ-001"},
		{"rerunner", cancel, 403, "GP-AUTHZ-001"},
		{"canceller", rerun, 403, "GP-AUTHZ-001"},
		{"rerunner", rerun, 201, ""},
		{"canceller", cancel, 200, ""},
	} {
		st, out, body := post(t, s, c.path, hdr(c.user, "k-"+c.user))
		if st != c.status || (c.rule != "" && out["rule"] != c.rule) {
			t.Fatalf("%s %s: %d %s", c.user, c.path, st, body)
		}
		if st == 403 && !strings.Contains(out["hint"].(string), "glidepath-api-operator") {
			t.Fatalf("hint names the operator role: %v", out)
		}
	}
	// Tower may write, but only naming the person.
	if st, out, _ := post(t, s, rerun, hdr("tower", "k-tower")); st != 400 || out["rule"] != "GP-AUTH-004" {
		t.Fatalf("delegate without on-behalf-of: %d %v", st, out)
	}
	if st, out, _ := post(t, s, cancel, hdr("tower", "")); st != 400 || out["rule"] != "GP-AUTH-004" {
		t.Fatalf("delegate cancel without on-behalf-of: %d %v", st, out)
	}
	h := hdr("tower", "k-tower")
	h[auth.HeaderOnBehalf] = "user:default/jfillman"
	if st, _, body := post(t, s, rerun, h); st != 201 {
		t.Fatalf("delegate with on-behalf-of: %d %s", st, body)
	}
	if !strings.Contains(logs.String(), `"caller":"tower","onBehalfOf":"user:default/jfillman"`) {
		t.Fatalf("audit names both: %s", logs)
	}
	if strings.Contains(logs.String(), "k-tower") {
		t.Fatal("the audit line carries the key's hash, not the key")
	}
	// A read by a delegate still needs no person.
	if st, _, _ := call(t, s, "/v1/apps/shop/releases", map[string]string{"Authorization": "Bearer t-tower"}); st != 200 {
		t.Fatalf("delegate read: %d", st)
	}
	if n := len(store.writes()); n != 3 {
		t.Fatalf("writes: %d", n)
	}
}
