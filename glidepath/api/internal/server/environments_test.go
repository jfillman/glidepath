package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/environments"
	"github.com/glidepath/api/internal/github"
	"github.com/glidepath/api/internal/pipelineruns"
	"github.com/glidepath/api/internal/releases"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	devSHA     = "abc1234def5678900000000000000000000000ff"
	stagingSHA = "5555aaaa0000000000000000000000000000beef"
)

// ghStub is GitHub and the broker in one server: /github-read-token mints "ghs-<repo>" for the glidepath-api token
// only, and the GitHub routes check the request carries that repo's token.
type ghStub struct {
	mu     sync.Mutex
	files  map[string]string // owner/repo/path -> content
	prs    map[string]string // owner/repo head -> html_url
	mints  []string          // repos a token was minted for
	calls  []string
	server *httptest.Server
}

func newGH(t *testing.T) *ghStub {
	g := &ghStub{files: map[string]string{}, prs: map[string]string{}}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	return g
}

func (g *ghStub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.RequestURI())
	if r.URL.Path == "/github-read-token" {
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"bad token"}`)
			return
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["app"] != "shop" || (req["repo"] != "shop" && req["repo"] != "gitops-shop") {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":"not the app's repo"}`)
			return
		}
		g.mints = append(g.mints, req["owner"]+"/"+req["repo"])
		json.NewEncoder(w).Encode(map[string]string{"token": "ghs-" + req["repo"], "expiresAt": "x"})
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repos/"), "/", 4) // owner, repo, kind, rest
	if len(parts) < 3 || r.Header.Get("Authorization") != "Bearer ghs-"+parts[1] {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	owner, repo := parts[0], parts[1]
	switch parts[2] {
	case "contents":
		content, ok := g.files[owner+"/"+repo+"/"+parts[3]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Accept") == "application/vnd.github.raw+json" {
			io.WriteString(w, content)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64", "sha": "s"})
	case "commits":
		for _, full := range []string{devSHA, stagingSHA} {
			if strings.HasPrefix(full, parts[3]) {
				json.NewEncoder(w).Encode(map[string]string{"sha": full})
				return
			}
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
	case "pulls":
		if u, ok := g.prs[owner+"/"+repo+" "+r.URL.Query().Get("head")]; ok && r.URL.Query().Get("state") == "open" {
			json.NewEncoder(w).Encode([]map[string]string{{"html_url": u}})
			return
		}
		io.WriteString(w, "[]")
	default:
		http.NotFound(w, r)
	}
}

const shopCicd = `
deploy:
  environments:
    - { name: dev, tier: ground }
    - { name: staging, tier: flight, cluster: hangar-prod }
    - { name: prod, tier: flight, cluster: hangar-prod, production: true }
`

// fakeTemplates serves promote-release and rollback-release as glidepath-app renders them (the shape read live from
// app-boarding-api-cicd, 2026-10-28).
type fakeTemplates struct{}

func (fakeTemplates) Get(_ context.Context, ns, name string) (map[string]any, error) {
	if ns != "app-shop-cicd" || (name != environments.PromoteTemplate && name != environments.RollbackTemplate) {
		return nil, apierrors.NewNotFound(environments.TemplateGVR.GroupResource(), name)
	}
	flow := "promote"
	params := []any{
		map[string]any{"name": "is-flow-root", "value": "true"},
		map[string]any{"name": "app-namespace", "value": "app-shop-cicd"},
		map[string]any{"name": "env", "value": "$(tt.params.env)"},
		map[string]any{"name": "cluster", "value": "$(tt.params.cluster)"},
		map[string]any{"name": "git-url", "value": "$(tt.params.git-url)"},
		map[string]any{"name": "git-revision", "value": "$(tt.params.git-revision)"},
		map[string]any{"name": "image-ref", "value": "$(tt.params.image-ref)"},
		map[string]any{"name": "initiator", "value": "$(tt.params.initiator)"},
	}
	if name == environments.RollbackTemplate {
		flow = "rollback"
		params = append(params,
			map[string]any{"name": "release-kind", "value": "rollback"},
			map[string]any{"name": "rollback-of", "value": "$(tt.params.rollback-of)"},
			map[string]any{"name": "rollback-reason", "value": "$(tt.params.rollback-reason)"},
			map[string]any{"name": "config-revision", "value": "$(tt.params.config-revision)"})
	}
	return copyObj(map[string]any{"apiVersion": "triggers.tekton.dev/v1beta1", "kind": "TriggerTemplate",
		"spec": map[string]any{"resourcetemplates": []any{map[string]any{
			"apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
			"metadata": map[string]any{"name": "$(tt.params.pipeline-run-name)",
				"labels":      map[string]any{"hangar.io/app": "shop", "hangar.io/flow": flow, "hangar.io/stage": "release"},
				"annotations": map[string]any{"hangar.io/initiator": "$(tt.params.initiator)"}},
			"spec": map[string]any{
				"pipelineRef": map[string]any{"resolver": "cluster", "params": []any{
					map[string]any{"name": "kind", "value": "pipeline"}, map[string]any{"name": "name", "value": "release"},
					map[string]any{"name": "namespace", "value": "platform-catalog"}}},
				"params":          params,
				"workspaces":      []any{map[string]any{"name": "source", "emptyDir": map[string]any{}}},
				"taskRunTemplate": map[string]any{"serviceAccountName": "pipeline-runner"},
			}}}}}), nil
}

func record(name string, labels map[string]string, data map[string]string) *corev1.ConfigMap {
	l := map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": "shop"}
	for k, v := range labels {
		l[k] = v
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "release-tracking-" + name, Namespace: "app-shop-cicd", Labels: l}, Data: data}
}

var prodLabels = map[string]string{"hangar.io/env": "prod", "hangar.io/cluster": "hangar-prod"}

// withEnvs is withRuns plus promote and rollback: GitHub and the broker stubbed, three prod release records.
func withEnvs(t *testing.T, allow map[string]bool) (*Server, *fakeStore, *ghStub, *strings.Builder) {
	s, store, _ := withRuns(t, allow)
	gh := newGH(t)
	gh.files["jfillman/shop/cicd.yaml"] = shopCicd
	gh.files["jfillman/shop/glidepath/envs/dev.release.yaml"] = "release:\n  image: { repository: ghcr.io/jfillman/shop, tag: 1.2.3-abc1234 }\n"
	gh.files["jfillman/gitops-shop/hangar-prod/staging/release.yaml"] = "release:\n  image: { repository: ghcr.io/jfillman/shop, tag: 1.3.0-5555aaa }\n"
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa-token\n"), 0o600)
	broker := &github.Broker{URL: gh.server.URL, TokenPath: tok, HTTP: gh.server.Client(), API: gh.server.URL}
	cs := fake.NewSimpleClientset(
		record("r1", prodLabels, map[string]string{"releaseId": "rel-1", "image": "ghcr.io/jfillman/shop:1.0.0-1111111", "gitRevision": "1111111aaaa",
			"gitUrl": "https://github.com/jfillman/shop", "state": "superseded", "healthyAt": "2026-10-20T10:00:00Z", "prCreatedAt": "2026-10-20T09:00:00Z"}),
		record("r2", prodLabels, map[string]string{"releaseId": "rel-2", "image": "ghcr.io/jfillman/shop:1.1.0-2222222", "gitRevision": "2222222bbbb",
			"gitUrl": "https://github.com/jfillman/shop", "state": "superseded", "healthyAt": "2026-10-22T10:00:00Z", "prCreatedAt": "2026-10-22T09:00:00.123456789Z"}),
		record("r3", prodLabels, map[string]string{"releaseId": "rel-3", "image": "ghcr.io/jfillman/shop:1.2.0-3333333", "gitRevision": "3333333cccc",
			"gitUrl": "https://github.com/jfillman/shop", "state": "healthy", "stateAt": "2026-10-24T10:00:00Z", "healthyAt": "2026-10-24T10:00:00Z", "prCreatedAt": "2026-10-24T09:00:00Z"}),
		// Another environment's record never shows in prod's plan.
		record("s1", map[string]string{"hangar.io/env": "staging", "hangar.io/cluster": "hangar-prod"},
			map[string]string{"releaseId": "s-1", "image": "x:1-4444444", "gitRevision": "4444444", "gitUrl": "u", "state": "healthy", "healthyAt": "2026-10-25T10:00:00Z"}),
	)
	s.Envs = &environments.Client{
		Owner:     "jfillman",
		GitHub:    func(app string) environments.GitHub { return broker.Session(app) },
		Templates: fakeTemplates{},
		Runs:      store,
		Records:   releases.Reader{CS: cs},
	}
	logs := &strings.Builder{}
	s.Log = slog.New(slog.NewJSONHandler(logs, nil))
	return s, store, gh, logs
}

var releaser = map[string]bool{
	"rel promote environments app-shop-cicd": true, "rel rollback environments app-shop-cicd": true,
	"rel get environments app-shop-cicd": true, "rel list environments app-shop-cicd": true,
}

func postJSON(t *testing.T, s *Server, path string, hdr map[string]string, body string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func param(run map[string]any, name string) any {
	for _, p := range run["spec"].(map[string]any)["params"].([]any) {
		if m := p.(map[string]any); m["name"] == name {
			return m["value"]
		}
	}
	return nil
}

func TestReleasePlan(t *testing.T) {
	s, _, gh, _ := withEnvs(t, releaser)
	st, _, body := call(t, s, "/v1/apps/shop/environments", hdr("rel", ""))
	want := `{"order":["dev","staging","prod"],"orderSource":"promotionOrder","lower":["dev"],"upper":[{"name":"staging","cluster":"hangar-prod"},{"name":"prod","cluster":"hangar-prod","production":true}]}` + "\n"
	if st != 200 || body != want {
		t.Fatalf("plan: %d\n%s\nwant\n%s", st, body, want)
	}
	// A freshly onboarded app with no cicd.yaml has an empty plan, not an error (Tower's 2026-09-21 fix).
	delete(gh.files, "jfillman/shop/cicd.yaml")
	if st, _, body := call(t, s, "/v1/apps/shop/environments", hdr("rel", "")); st != 200 ||
		body != `{"order":[],"orderSource":"pipeline-steps","lower":[],"upper":[]}`+"\n" {
		t.Fatalf("no cicd.yaml: %d %s", st, body)
	}
	if strings.Contains(body, "ghs-") {
		t.Fatal("a GitHub token in a response")
	}
}

func TestPromoteFromGround(t *testing.T) {
	s, store, gh, logs := withEnvs(t, releaser)
	h := hdr("rel", "k1")
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", h, `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
	if st != 201 || out["mode"] != "pipeline" {
		t.Fatalf("promote: %d %s", st, body)
	}
	name := out["pipelineRun"].(string)
	want := environments.RunName("promote", "staging", devSHA, environments.KeyHash("shop", "promote", "staging", "hangar-prod", "k1"))
	if name != want || !strings.HasPrefix(name, "promote-staging-abc1234d-") || len(name) > 63 {
		t.Fatalf("run name %s, want %s", name, want)
	}
	if body != `{"mode":"pipeline","pipelineRun":"`+name+`"}`+"\n" {
		t.Fatalf("Tower's PromoteResult shape: %s", body)
	}
	run := store.objs[name]
	// The source is the dev release file's image, its tag's sha expanded against the app repo; never the client's.
	for p, v := range map[string]string{"env": "staging", "cluster": "hangar-prod", "image-ref": "ghcr.io/jfillman/shop:1.2.3-abc1234",
		"git-revision": devSHA, "git-url": "https://github.com/jfillman/shop", "initiator": "rel"} {
		if param(run, p) != v {
			t.Errorf("param %s = %v, want %s", p, param(run, p), v)
		}
	}
	meta := run["metadata"].(map[string]any)
	ann := meta["annotations"].(map[string]any)
	if meta["namespace"] != "app-shop-cicd" || ann["hangar.io/initiator"] != "rel" ||
		ann[pipelineruns.AnnotationKeyHash] != environments.KeyHash("shop", "promote", "staging", "hangar-prod", "k1") {
		t.Fatalf("metadata: %v", meta)
	}
	for k := range ann {
		if strings.HasPrefix(k, "pipelinesascode.tekton.dev/") {
			t.Fatal("glidepath-api-pipelinerun-shape refuses PaC annotations")
		}
	}

	// The same key again: one run, answered as a replay.
	st, out, body = postJSON(t, s, "/v1/apps/shop/environments/staging/promote", h, `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
	if st != 200 || out["pipelineRun"] != name || out["replayed"] != true {
		t.Fatalf("replay: %d %s", st, body)
	}
	// A new key is a new run.
	if st, out, _ := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k2"), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`); st != 201 || out["pipelineRun"] == name {
		t.Fatalf("second key: %d %v", st, out)
	}
	// Once the release PR is open, a promotion of the same commit answers it and starts nothing.
	gh.prs["jfillman/gitops-shop jfillman:release-staging-abc1234d"] = "https://github.com/jfillman/gitops-shop/pull/7"
	before := len(store.writes())
	st, _, body = postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k3"), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
	if st != 200 || body != `{"mode":"pr","prUrl":"https://github.com/jfillman/gitops-shop/pull/7","alreadyOpen":true}`+"\n" || len(store.writes()) != before {
		t.Fatalf("open PR: %d %s", st, body)
	}

	audit := logs.String()
	for _, f := range []string{`"op":"promoteEnvironment"`, `"tier":"T2"`, `"env":"staging"`, `"created":"` + name + `"`, `"mode":"pr"`, `"replayed":true`} {
		if !strings.Contains(audit, f) {
			t.Errorf("audit lacks %s", f)
		}
	}
	if strings.Contains(audit, "ghs-") || strings.Contains(audit, "sa-token") || strings.Contains(audit, `"k1"`) {
		t.Fatalf("a credential or key in the audit: %s", audit)
	}
	// Tokens are minted per request and per repo, never reused from an earlier request: 4 requests read the app repo
	// (cicd.yaml, dev's file, the commit) and gitops-shop (the PR search).
	if len(gh.mints) != 8 {
		t.Fatalf("mints: %v", gh.mints)
	}
}

func TestPromoteFromFlight(t *testing.T) {
	s, store, gh, _ := withEnvs(t, releaser)
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/prod/promote", hdr("rel", "k1"), `{"cluster":"hangar-prod","sourceEnv":"staging","sourceCluster":"hangar-prod"}`)
	if st != 201 {
		t.Fatalf("promote: %d %s", st, body)
	}
	run := store.objs[out["pipelineRun"].(string)]
	if param(run, "image-ref") != "ghcr.io/jfillman/shop:1.3.0-5555aaa" || param(run, "git-revision") != stagingSHA {
		t.Fatalf("staging's release file: %v %v", param(run, "image-ref"), param(run, "git-revision"))
	}

	// release.yaml without an image falls back to values.yaml's deprecated rollout.image, and a recorded
	// releaseTracking.gitRevision is used as is.
	gh.files["jfillman/gitops-shop/hangar-prod/staging/release.yaml"] = "releaseTracking: { cluster: hangar-prod }\n"
	gh.files["jfillman/gitops-shop/hangar-prod/staging/values.yaml"] = "rollout:\n  image: { repository: ghcr.io/jfillman/shop, tag: 1.0.0-0000000 }\nreleaseTracking: { gitRevision: recorded123 }\n"
	st, out, body = postJSON(t, s, "/v1/apps/shop/environments/prod/promote", hdr("rel", "k2"), `{"cluster":"hangar-prod","sourceEnv":"staging","sourceCluster":"hangar-prod"}`)
	if st != 201 {
		t.Fatalf("fallback: %d %s", st, body)
	}
	run = store.objs[out["pipelineRun"].(string)]
	if param(run, "image-ref") != "ghcr.io/jfillman/shop:1.0.0-0000000" || param(run, "git-revision") != "recorded123" {
		t.Fatalf("fallback source: %v %v", param(run, "image-ref"), param(run, "git-revision"))
	}

	// A duplicate key in the release file is refused, naming the file (Tower's assertValidYamlDoc).
	gh.files["jfillman/gitops-shop/hangar-prod/staging/release.yaml"] = "release: {}\nrelease: {}\n"
	if st, out, _ := postJSON(t, s, "/v1/apps/shop/environments/prod/promote", hdr("rel", "k3"), `{"cluster":"hangar-prod","sourceEnv":"staging","sourceCluster":"hangar-prod"}`); st != 502 ||
		!strings.Contains(out["error"].(string), "hangar-prod/staging/release.yaml has invalid YAML") {
		t.Fatalf("invalid YAML: %d %v", st, out)
	}
	// Nothing recorded anywhere: nothing to promote.
	delete(gh.files, "jfillman/gitops-shop/hangar-prod/staging/release.yaml")
	delete(gh.files, "jfillman/gitops-shop/hangar-prod/staging/values.yaml")
	if st, out, _ := postJSON(t, s, "/v1/apps/shop/environments/prod/promote", hdr("rel", "k4"), `{"cluster":"hangar-prod","sourceEnv":"staging","sourceCluster":"hangar-prod"}`); st != 422 ||
		out["error"] != "staging (hangar-prod/staging/release.yaml) has no release.image recorded yet - nothing to promote" {
		t.Fatalf("nothing recorded: %d %v", st, out)
	}
}

func TestPromoteRefusals(t *testing.T) {
	s, store, _, _ := withEnvs(t, releaser)
	p := "/v1/apps/shop/environments/"
	for _, c := range []struct {
		path, body string
		status     int
		in         string
	}{
		// Skipping a stage.
		{p + "prod/promote", `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`, 422, "is not the environment immediately after dev"},
		// Backwards.
		{p + "staging/promote", `{"cluster":"hangar-prod","sourceEnv":"prod","sourceCluster":"hangar-prod"}`, 422, "is not the environment immediately after prod"},
		// A first deploy only goes to the first environment, and dev is Ground: refused either way.
		{p + "staging/promote", `{"cluster":"hangar-prod","sourceImageRepo":"ghcr.io/jfillman/shop","sourceImageTag":"1.0.0-abc1234"}`, 422, "first environment"},
		{p + "dev/promote", `{"cluster":"hangar-dev","sourceImageRepo":"ghcr.io/jfillman/shop","sourceImageTag":"1.0.0-abc1234"}`, 422, "is a Ground environment"},
		// Both source pairs, or neither.
		{p + "staging/promote", `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev","sourceImageRepo":"ghcr.io/x/y","sourceImageTag":"1-abc1234"}`, 400, "exactly one of"},
		{p + "staging/promote", `{"cluster":"hangar-prod"}`, 400, "exactly one of"},
		{p + "staging/promote", `{"sourceEnv":"dev","sourceCluster":"hangar-dev"}`, 400, "exactly one of"},
		{p + "staging/promote", ``, 400, "a JSON body is required"},
		{p + "staging/promote", `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"bad cluster"}`, 400, "plain names"},
	} {
		st, out, body := postJSON(t, s, c.path, hdr("rel", "k1"), c.body)
		if st != c.status || !strings.Contains(body, c.in) {
			t.Errorf("%s %s: %d %s, want %d %q", c.path, c.body, st, body, c.status, c.in)
		}
		if st == 422 && out["rule"] != "GP-API-422" {
			t.Errorf("rule: %v", out)
		}
	}
	if st, out, _ := postJSON(t, s, p+"staging/promote", hdr("rel", ""), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`); st != 400 || out["rule"] != "GP-API-400" {
		t.Fatalf("no Idempotency-Key: %d %v", st, out)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("a refusal wrote: %v", w)
	}
}

func TestPromoteDryRun(t *testing.T) {
	s, store, _, logs := withEnvs(t, releaser)
	h := hdr("rel", "k1")
	h[HeaderDryRun] = "true"
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", h, `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
	if st != 200 || out["dryRun"] != true || out["mode"] != "pipeline" {
		t.Fatalf("dry run: %d %s", st, body)
	}
	obj := out["object"].(map[string]any)
	if obj["metadata"].(map[string]any)["name"] != out["pipelineRun"] || param(obj, "image-ref") != "ghcr.io/jfillman/shop:1.2.3-abc1234" {
		t.Fatalf("the run it would create: %s", body)
	}
	if w := store.writes(); len(w) != 0 || len(store.actions) == 0 || !store.actions[len(store.actions)-1].dryRun {
		t.Fatalf("a dry run wrote, or skipped the server-side dry run: %v", store.actions)
	}
	if !strings.Contains(logs.String(), `"dryRun":true`) {
		t.Fatal("audited as a dry run")
	}
	// The cluster refusing the run (the shape policy) is 422.
	store.refuse = apierrors.NewForbidden(pipelineruns.GVR.GroupResource(), "x", nil)
	if st, out, _ := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k2"), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`); st != 422 || out["rule"] != "GP-API-422" {
		t.Fatalf("admission refusal: %d %v", st, out)
	}
}

func TestRollbackPlanAndRollback(t *testing.T) {
	s, store, gh, logs := withEnvs(t, releaser)
	st, _, body := call(t, s, "/v1/apps/shop/environments/prod/rollback-plan?cluster=hangar-prod", hdr("rel", ""))
	want := `{"current":{"releaseId":"rel-3","image":"ghcr.io/jfillman/shop:1.2.0-3333333","state":"healthy","stateAt":"2026-10-24T10:00:00Z"},` +
		`"targets":[{"releaseId":"rel-2","image":"ghcr.io/jfillman/shop:1.1.0-2222222","gitRevision":"2222222bbbb","gitUrl":"https://github.com/jfillman/shop","healthyAt":"2026-10-22T10:00:00Z","state":"superseded"},` +
		`{"releaseId":"rel-1","image":"ghcr.io/jfillman/shop:1.0.0-1111111","gitRevision":"1111111aaaa","gitUrl":"https://github.com/jfillman/shop","healthyAt":"2026-10-20T10:00:00Z","state":"superseded"}]}` + "\n"
	if st != 200 || body != want {
		t.Fatalf("plan: %d\n%s\nwant\n%s", st, body, want)
	}
	if st, _, body := call(t, s, "/v1/apps/shop/environments/qa/rollback-plan?cluster=hangar-prod", hdr("rel", "")); st != 200 || body != `{"targets":[]}`+"\n" {
		t.Fatalf("nothing released: %d %s", st, body)
	}
	if st, _, _ := call(t, s, "/v1/apps/shop/environments/prod/rollback-plan", hdr("rel", "")); st != 400 {
		t.Fatalf("no cluster: %d", st)
	}

	path := "/v1/apps/shop/environments/prod/rollback"
	st, out, body := postJSON(t, s, path, hdr("rel", "k1"), `{"cluster":"hangar-prod","targetReleaseId":"rel-1","reason":"  error budget  "}`)
	if st != 201 || out["replaces"] != "rel-3" || out["alreadyOpen"] != false {
		t.Fatalf("rollback: %d %s", st, body)
	}
	name := out["pipelineRun"].(string)
	if !strings.HasPrefix(name, "rollback-prod-1111111a-") {
		t.Fatalf("name %s", name)
	}
	run := store.objs[name]
	for p, v := range map[string]string{"image-ref": "ghcr.io/jfillman/shop:1.0.0-1111111", "git-revision": "1111111aaaa", "rollback-of": "rel-3",
		"rollback-reason": "error budget", "config-revision": "main", "release-kind": "rollback", "env": "prod", "cluster": "hangar-prod"} {
		if param(run, p) != v {
			t.Errorf("param %s = %v, want %s", p, param(run, p), v)
		}
	}
	if st, out, _ := postJSON(t, s, path, hdr("rel", "k1"), `{"cluster":"hangar-prod","targetReleaseId":"rel-1","reason":"error budget"}`); st != 200 || out["replayed"] != true || out["pipelineRun"] != name {
		t.Fatalf("replay: %d %v", st, out)
	}

	for _, c := range []struct {
		body   string
		status int
		in     string
	}{
		{`{"cluster":"hangar-prod","targetReleaseId":"rel-3","reason":"x"}`, 422, "not an eligible rollback target"}, // the current release
		{`{"cluster":"hangar-prod","targetReleaseId":"rel-9","reason":"x"}`, 422, "not an eligible rollback target"},
		{`{"cluster":"hangar-dev","targetReleaseId":"rel-1","reason":"x"}`, 422, "nothing has been released to prod on hangar-dev"},
		{`{"cluster":"hangar-prod","targetReleaseId":"rel-1","reason":"   "}`, 400, "reason"},
		{`{"cluster":"hangar-prod","reason":"x"}`, 400, "targetReleaseId"},
	} {
		if st, _, body := postJSON(t, s, path, hdr("rel", "k2"), c.body); st != c.status || !strings.Contains(body, c.in) {
			t.Errorf("%s: %d %s", c.body, st, body)
		}
	}

	// The rollback's PR already open: answered with it, nothing started.
	gh.prs["jfillman/gitops-shop jfillman:release-rollback-prod-2222222b"] = "https://github.com/jfillman/gitops-shop/pull/9"
	before := len(store.writes())
	st, _, body = postJSON(t, s, path, hdr("rel", "k3"), `{"cluster":"hangar-prod","targetReleaseId":"rel-2","reason":"x"}`)
	if st != 200 || len(store.writes()) != before || !strings.HasPrefix(body, `{"prUrl":"https://github.com/jfillman/gitops-shop/pull/9","alreadyOpen":true,"target":{"releaseId":"rel-2"`) ||
		!strings.HasSuffix(body, `"replaces":"rel-3"}`+"\n") {
		t.Fatalf("open PR: %d %s", st, body)
	}
	if !strings.Contains(logs.String(), `"op":"rollbackEnvironment"`) || !strings.Contains(logs.String(), `"target":"rel-1"`) {
		t.Fatalf("audit: %s", logs)
	}
}

func TestEnvironmentAuthorization(t *testing.T) {
	allow := map[string]bool{
		"reader get environments app-shop-cicd": true, "reader list environments app-shop-cicd": true,
		"promoter promote environments app-shop-cicd": true, "promoter get environments app-shop-cicd": true,
		"op rerun runs app-shop-cicd": true,
		"tower *":                     true,
	}
	s, store, _, logs := withEnvs(t, allow)
	promote, rollback := "/v1/apps/shop/environments/staging/promote", "/v1/apps/shop/environments/prod/rollback"
	pb, rb := `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`, `{"cluster":"hangar-prod","targetReleaseId":"rel-1","reason":"x"}`
	if st, _, _ := call(t, s, "/v1/apps/shop/environments", hdr("reader", "")); st != 200 {
		t.Fatalf("reader reads the plan: %d", st)
	}
	for _, c := range []struct {
		user, path, body string
		status           int
	}{
		{"reader", promote, pb, 403},
		{"reader", rollback, rb, 403},
		{"op", promote, pb, 403}, // an operator (re-run, cancel) is not a releaser
		{"promoter", rollback, rb, 403},
		{"promoter", promote, pb, 201},
	} {
		st, out, body := postJSON(t, s, c.path, hdr(c.user, "k-"+c.user), c.body)
		if st != c.status {
			t.Fatalf("%s %s: %d %s", c.user, c.path, st, body)
		}
		if st == 403 && (out["rule"] != "GP-AUTHZ-001" || !strings.Contains(out["hint"].(string), "glidepath-api-releaser")) {
			t.Fatalf("hint names the releaser role: %v", out)
		}
	}
	if st, out, _ := call(t, s, "/v1/apps/shop/environments", hdr("op", "")); st != 403 || !strings.Contains(out["hint"].(string), "glidepath-api-reader") {
		t.Fatalf("read without environments: %d %v", st, out)
	}

	// Tower may promote, but only naming the person, who is then the run's initiator.
	if st, out, _ := postJSON(t, s, promote, hdr("tower", "k-t1"), pb); st != 400 || out["rule"] != "GP-AUTH-004" {
		t.Fatalf("delegate without on-behalf-of: %d %v", st, out)
	}
	if st, out, _ := postJSON(t, s, rollback, hdr("tower", "k-t1"), rb); st != 400 || out["rule"] != "GP-AUTH-004" {
		t.Fatalf("delegate rollback without on-behalf-of: %d %v", st, out)
	}
	h := hdr("tower", "k-t2")
	h[auth.HeaderOnBehalf] = "user:default/jfillman"
	st, out, body := postJSON(t, s, promote, h, pb)
	if st != 201 {
		t.Fatalf("delegate with on-behalf-of: %d %s", st, body)
	}
	if run := store.objs[out["pipelineRun"].(string)]; param(run, "initiator") != "user:default/jfillman" {
		t.Fatalf("initiator: %v", param(run, "initiator"))
	}
	if !strings.Contains(logs.String(), `"caller":"tower","onBehalfOf":"user:default/jfillman"`) {
		t.Fatalf("audit names both: %s", logs)
	}
}
