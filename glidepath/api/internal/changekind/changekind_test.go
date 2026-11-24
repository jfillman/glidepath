package changekind

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/releases"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	baseSHA = "1111111111111111111111111111111111111111"
	headSHA = "2222222222222222222222222222222222222222"
	oldHead = "3333333333333333333333333333333333333333"
	prURL   = "https://github.com/acme/gitops-shop/pull/7"
)

var shop = &apps.App{Name: "shop", Namespace: "app-shop-cicd"}

// world is GitHub for one gitops pull request (#7, base baseSHA, head headSHA): the files at each commit, the comparison derived
// from them unless compare is set, and every path asked.
type world struct {
	mu      sync.Mutex
	files   map[string]map[string]string // sha -> path -> content
	compare []ghFile                     // overrides the derived comparison
	commits []string                     // the pull request's commits (pulls/7/commits)
	fail    map[string]int               // a path prefix -> the status GitHub answers
	calls   []string
	hold    chan struct{} // when set, the comparison waits for it
}

func newWorld(base, head map[string]string) *world {
	return &world{files: map[string]map[string]string{baseSHA: base, headSHA: head}, commits: []string{oldHead, headSHA}, fail: map[string]int{}}
}

func (w *world) count(prefix string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (w *world) GetJSON(ctx context.Context, owner, repo, p string) (int, []byte, error) {
	w.mu.Lock()
	w.calls = append(w.calls, p)
	hold := w.hold
	for pre, st := range w.fail {
		if strings.HasPrefix(p, pre) {
			w.mu.Unlock()
			return st, []byte(`{"message":"no"}`), nil
		}
	}
	w.mu.Unlock()
	if owner != "acme" || repo != "gitops-shop" {
		return http.StatusNotFound, nil, nil
	}
	ok := func(v any) (int, []byte, error) {
		b, _ := json.Marshal(v)
		return http.StatusOK, b, nil
	}
	switch {
	case p == "pulls/7":
		return ok(map[string]any{"number": 7, "html_url": prURL, "base": map[string]string{"sha": baseSHA}, "head": map[string]string{"sha": headSHA}})
	case strings.HasPrefix(p, "pulls/7/commits"):
		var out []map[string]string
		if strings.HasSuffix(p, "page=1") {
			for _, c := range w.commits {
				out = append(out, map[string]string{"sha": c})
			}
		}
		return ok(out)
	case strings.HasPrefix(p, "pulls/"):
		return http.StatusNotFound, nil, nil
	case strings.HasPrefix(p, "compare/"):
		if hold != nil {
			<-hold
		}
		if want := "compare/" + baseSHA + "..." + headSHA; p != want && p != "compare/"+baseSHA+"..."+oldHead {
			return http.StatusNotFound, nil, nil
		}
		if w.compare != nil {
			return ok(map[string]any{"files": w.compare})
		}
		return ok(map[string]any{"files": w.derived()})
	case strings.HasPrefix(p, "contents/"):
		rest := strings.TrimPrefix(p, "contents/")
		ep, q, _ := strings.Cut(rest, "?ref=")
		fp, _ := url.PathUnescape(ep)
		ref, _ := url.QueryUnescape(q)
		tree := w.files[ref]
		if c, found := tree[fp]; found {
			return ok(map[string]string{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(c))})
		}
		var entries []map[string]string
		seen := map[string]bool{}
		for f := range tree {
			if strings.HasPrefix(f, fp+"/") {
				name, more, _ := strings.Cut(strings.TrimPrefix(f, fp+"/"), "/")
				typ := "file"
				if more != "" {
					typ = "dir"
				}
				if !seen[name] {
					seen[name] = true
					entries = append(entries, map[string]string{"name": name, "type": typ})
				}
			}
		}
		if entries == nil {
			return http.StatusNotFound, nil, nil
		}
		return ok(entries)
	}
	return http.StatusNotFound, nil, nil
}

func (w *world) derived() []ghFile {
	var out []ghFile
	b, h := w.files[baseSHA], w.files[headSHA]
	for f, c := range h {
		if bc, ok := b[f]; !ok {
			out = append(out, ghFile{Filename: f, Status: "added"})
		} else if bc != c {
			out = append(out, ghFile{Filename: f, Status: "modified"})
		}
	}
	for f := range b {
		if _, ok := h[f]; !ok {
			out = append(out, ghFile{Filename: f, Status: "removed"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// memStore is the cluster: ConfigMaps by name with a resourceVersion.
type memStore struct {
	mu      sync.Mutex
	cms     map[string]*corev1.ConfigMap
	rv      int
	creates int
	patches int
}

func newStore() *memStore { return &memStore{cms: map[string]*corev1.ConfigMap{}} }

func (m *memStore) Get(_ context.Context, ns, name string) (*corev1.ConfigMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cm, ok := m.cms[ns+"/"+name]
	if !ok {
		return nil, nil
	}
	return cm.DeepCopy(), nil
}

func (m *memStore) Create(_ context.Context, ns string, cm *corev1.ConfigMap) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cms[ns+"/"+cm.Name]; ok {
		return ErrExists
	}
	m.rv++
	c := cm.DeepCopy()
	c.Namespace, c.ResourceVersion = ns, strconv.Itoa(m.rv)
	m.cms[ns+"/"+cm.Name] = c
	m.creates++
	return nil
}

func (m *memStore) Patch(_ context.Context, ns, name, rv string, data map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cm, ok := m.cms[ns+"/"+name]
	if !ok {
		return errors.New("not found")
	}
	if cm.ResourceVersion != rv {
		return ErrConflict
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	for k, v := range data {
		cm.Data[k] = v
	}
	m.rv++
	cm.ResourceVersion = strconv.Itoa(m.rv)
	m.patches++
	return nil
}

func (m *memStore) Records(_ context.Context, ns string) ([]corev1.ConfigMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []corev1.ConfigMap
	for _, cm := range m.cms {
		if cm.Namespace == ns && cm.Labels["hangar.io/subcomponent"] == "release-tracking" {
			out = append(out, *cm.DeepCopy())
		}
	}
	return out, nil
}

// records are the release records per <cluster>/<env>.
type records struct {
	mu   sync.Mutex
	recs map[string][]releases.Record
	err  error
}

func (r *records) ListEnv(_ context.Context, ns, app, env, cluster string) ([]releases.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return r.recs[cluster+"/"+env], nil
}

func healthy(id, image, at string) releases.Record {
	return releases.Record{Name: "release-tracking-" + id, Data: map[string]string{"releaseId": id, "image": image, "healthyAt": at}}
}

func service(w *world, st *memStore, rec *records) *Service {
	if rec == nil {
		rec = &records{}
	}
	return &Service{Owner: "acme", GitHub: func(string) Reader { return w }, Records: rec, Store: st,
		Now: func() time.Time { return time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) }}
}

func classify(t *testing.T, w *world, rec *records) *Answer {
	t.Helper()
	a, err := service(w, newStore(), rec).Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func envOf(t *testing.T, a *Answer, cluster, env string) Env {
	t.Helper()
	for _, e := range a.Envs {
		if e.Cluster == cluster && e.Env == env {
			return e
		}
	}
	t.Fatalf("no environment %s/%s in %+v", cluster, env, a.Envs)
	return Env{}
}

const valuesBase = `appName: shop
envName: staging
rollout:
  replicas: 1
  ports:
    - name: http
      containerPort: 8080
`

func release(repo, tag string) string {
	return fmt.Sprintf("# machine-owned\nrelease:\n  image:\n    repository: %s\n    tag: %s\nreleaseTracking:\n  cluster: prod-1\n  releaseId: c1:prod-1/staging\n", repo, tag)
}

// --- the fixtures, one per kind and edge (spec CG-9) ---

func TestReleaseImageTagBumpIsAnImageRelease(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.0.0-aaa")},
		map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.1.0-bbb")})
	a := classify(t, w, nil)
	if a.Kind != ImageRelease || a.FailClosed || a.Outcome != Computed {
		t.Fatalf("%+v", a)
	}
	e := envOf(t, a, "prod-1", "staging")
	if e.Rollback == nil || e.Rollback.Eligible || e.Rollback.Image != "ghcr.io/acme/shop:1.1.0-bbb" {
		t.Fatalf("a new image that never ran healthy is not a rollback: %+v", e.Rollback)
	}
	if fmt.Sprint(e.Images.Base) != "[ghcr.io/acme/shop:1.0.0-aaa]" || fmt.Sprint(e.Images.Head) != "[ghcr.io/acme/shop:1.1.0-bbb]" {
		t.Fatalf("%+v", e.Images)
	}
	if !strings.Contains(a.Reason, "release.image = ghcr.io/acme/shop:1.1.0-bbb") {
		t.Fatalf("the reason names the new reference: %s", a.Reason)
	}
}

// A hand edit of the image in a values.yaml (an app that has not split its release file): the classifier never reads who wrote
// it, the branch or the trailers, so it is an image release and every content gate runs.
func TestHandImageEditIsAnImageRelease(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/prod/values.yaml": valuesBase + "release:\n  image: {repository: ghcr.io/acme/shop, tag: '1.0'}\n"},
		map[string]string{"prod-1/prod/values.yaml": valuesBase + "release:\n  image: {repository: ghcr.io/evil/shop, tag: '1.0'}\n"})
	if a := classify(t, w, nil); a.Kind != ImageRelease {
		t.Fatalf("%+v", a)
	}
}

func TestEligibleReturnIsARollbackAndIneligibleIsAnImageRelease(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "2.0")},
		map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.9")})
	rec := &records{recs: map[string][]releases.Record{"prod-1/staging": {
		healthy("c9", "ghcr.io/acme/shop:2.0", "2026-11-20T10:00:00Z"),
		healthy("c8", "ghcr.io/acme/shop:1.9", "2026-11-19T10:00:00Z"),
	}}}
	a := classify(t, w, rec)
	if a.Kind != Rollback || !envOf(t, a, "prod-1", "staging").Rollback.Eligible || !strings.Contains(a.Reason, "release c8") {
		t.Fatalf("%+v", a)
	}
	// Six newer healthy releases push 1.9 out of the last five: no longer eligible.
	var many []releases.Record
	for i := 0; i < 6; i++ {
		many = append(many, healthy(fmt.Sprintf("n%d", i), fmt.Sprintf("ghcr.io/acme/shop:3.%d", i), fmt.Sprintf("2026-11-2%dT10:00:00Z", i)))
	}
	many = append(many, healthy("c8", "ghcr.io/acme/shop:1.9", "2026-11-19T10:00:00Z"))
	a = classify(t, w, &records{recs: map[string][]releases.Record{"prod-1/staging": many}})
	if a.Kind != ImageRelease || !strings.Contains(a.Reason, "not among the images of the last five healthy releases") {
		t.Fatalf("%+v", a)
	}
	// A record that never reached healthy does not count, and a record of another environment neither.
	a = classify(t, w, &records{recs: map[string][]releases.Record{
		"prod-1/staging": {{Name: "release-tracking-c8", Data: map[string]string{"releaseId": "c8", "image": "ghcr.io/acme/shop:1.9"}}},
		"prod-1/prod":    {healthy("c7", "ghcr.io/acme/shop:1.9", "2026-11-19T10:00:00Z")},
	}})
	if a.Kind != ImageRelease {
		t.Fatalf("%+v", a)
	}
	// Records that cannot be read are "not eligible".
	a = classify(t, w, &records{err: errors.New("forbidden")})
	if a.Kind != ImageRelease || !strings.Contains(a.Reason, "could not read the release records") {
		t.Fatalf("%+v", a)
	}
}

// release.image.source (ADR-0029) is the repository the eligibility compares, as resolve-promoted-image reads it.
func TestRollbackComparesTheSourceRepository(t *testing.T) {
	file := func(tag string) string {
		return "release:\n  image:\n    repository: ghcr.io/flight/shop\n    source: ghcr.io/acme/shop\n    tag: " + tag + "\n"
	}
	w := newWorld(map[string]string{"prod-1/prod/release.yaml": file("2.0")}, map[string]string{"prod-1/prod/release.yaml": file("1.9")})
	a := classify(t, w, &records{recs: map[string][]releases.Record{"prod-1/prod": {healthy("c8", "ghcr.io/acme/shop:1.9", "2026-11-19T10:00:00Z")}}})
	if a.Kind != Rollback {
		t.Fatalf("%+v", a)
	}
}

// gitops-gate-api#96: staging replicas 1 to 2.
func TestReplicasChangeIsConfig(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/staging/values.yaml": valuesBase, "prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.0")},
		map[string]string{"prod-1/staging/values.yaml": strings.Replace(valuesBase, "replicas: 1", "replicas: 2", 1), "prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.0")})
	a := classify(t, w, nil)
	if a.Kind != Config || a.RenderCheck != RenderNone || len(a.Envs) != 1 {
		t.Fatalf("%+v", a)
	}
	if e := envOf(t, a, "prod-1", "staging"); fmt.Sprint(e.Changed) != "[prod-1/staging/values.yaml: rollout.replicas]" || e.Rollback != nil {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(a.Reason, "prod-1/staging: configuration changed (prod-1/staging/values.yaml: rollout.replicas); no new image reference") {
		t.Fatalf("%s", a.Reason)
	}
}

// A setting under an image key (pullPolicy) is configuration: it names no image.
func TestImageSettingIsConfig(t *testing.T) {
	v := "release:\n  image:\n    repository: ghcr.io/acme/shop\n    tag: '1.0'\n    pullPolicy: %s\n"
	w := newWorld(map[string]string{"prod-1/prod/values.yaml": fmt.Sprintf(v, "IfNotPresent")}, map[string]string{"prod-1/prod/values.yaml": fmt.Sprintf(v, "Always")})
	a := classify(t, w, nil)
	if a.Kind != Config || fmt.Sprint(envOf(t, a, "prod-1", "prod").Changed) != "[prod-1/prod/values.yaml: release.image.pullPolicy]" {
		t.Fatalf("%+v", a)
	}
}

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata: {name: worker}
spec:
  template:
    spec:
      initContainers:
        - name: init
          image: busybox:1.36
      containers:
        - name: worker
          image: %s
`

func TestRawManifestImageChangeIsAnImageRelease(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/prod/manifests/worker.yaml": fmt.Sprintf(deployment, "ghcr.io/acme/worker:1")},
		map[string]string{"prod-1/prod/manifests/worker.yaml": fmt.Sprintf(deployment, "ghcr.io/acme/worker:2")})
	a := classify(t, w, nil)
	if a.Kind != ImageRelease || envOf(t, a, "prod-1", "prod").Rollback != nil {
		t.Fatalf("not the release image, so no rollback check: %+v", a)
	}
	if !strings.Contains(a.Reason, "spec.template.spec.containers[0].image = ghcr.io/acme/worker:2") {
		t.Fatalf("%s", a.Reason)
	}
}

func TestKustomizationImagesChangeIsAnImageRelease(t *testing.T) {
	k := "resources: [deploy.yaml]\nimages:\n  - name: shop\n    newName: ghcr.io/acme/shop\n    newTag: %s\n"
	w := newWorld(map[string]string{"prod-1/prod/kustomization.yaml": fmt.Sprintf(k, "1")}, map[string]string{"prod-1/prod/kustomization.yaml": fmt.Sprintf(k, "2")})
	a := classify(t, w, nil)
	if a.Kind != ImageRelease || fmt.Sprint(envOf(t, a, "prod-1", "prod").Images.Head) != "[shop=>ghcr.io/acme/shop:2]" {
		t.Fatalf("%+v", a)
	}
	// helmCharts[].valuesInline image keys count too.
	h := "helmCharts:\n  - name: shop\n    valuesInline:\n      image: {repository: ghcr.io/acme/shop, tag: '%s'}\n"
	w = newWorld(map[string]string{"prod-1/prod/kustomization.yaml": fmt.Sprintf(h, "1")}, map[string]string{"prod-1/prod/kustomization.yaml": fmt.Sprintf(h, "2")})
	if a := classify(t, w, nil); a.Kind != ImageRelease {
		t.Fatalf("%+v", a)
	}
}

// <cluster>/base.yaml feeds every environment of that cluster.
func TestBaseImageChangeIsAnImageReleaseForEveryEnvironment(t *testing.T) {
	b := "release:\n  image: {repository: ghcr.io/acme/sidecar, tag: '%s'}\n"
	w := newWorld(map[string]string{"prod-1/base.yaml": fmt.Sprintf(b, "1"), "prod-1/staging/values.yaml": valuesBase, "prod-1/prod/values.yaml": valuesBase, "dev-1/dev/values.yaml": valuesBase},
		map[string]string{"prod-1/base.yaml": fmt.Sprintf(b, "2"), "prod-1/staging/values.yaml": valuesBase, "prod-1/prod/values.yaml": valuesBase, "dev-1/dev/values.yaml": valuesBase})
	a := classify(t, w, nil)
	if a.Kind != ImageRelease || len(a.Envs) != 2 {
		t.Fatalf("%+v", a)
	}
	for _, env := range []string{"prod", "staging"} {
		if e := envOf(t, a, "prod-1", env); e.Kind != ImageRelease || e.Rollback != nil {
			t.Fatalf("%s: %+v", env, e)
		}
	}
}

func TestCommentAndKeyOrderChangesAreNoOp(t *testing.T) {
	reordered := "envName: staging\n# a comment\nappName: shop\nrollout:\n  ports:\n    - {containerPort: 8080, name: http}\n  replicas: 1\n\n---\n"
	w := newWorld(map[string]string{"prod-1/staging/values.yaml": valuesBase}, map[string]string{"prod-1/staging/values.yaml": reordered})
	a := classify(t, w, nil)
	if a.Kind != NoOp || envOf(t, a, "prod-1", "staging").Kind != NoOp {
		t.Fatalf("%+v", a)
	}
}

func TestNothingDeployableIsNoOp(t *testing.T) {
	for name, path := range map[string]string{
		"an onboarding resync": ".tekton/pull-request-sast.yaml",
		"a Release Record":     "releases/shop@1.2.0-abc.yaml",
		"a README":             "README.md",
		"docs":                 "docs/how.md",
	} {
		w := newWorld(map[string]string{}, map[string]string{path: "x: 1\n"})
		a := classify(t, w, nil)
		if a.Kind != NoOp || len(a.Envs) != 0 || !strings.Contains(a.Reason, "no deployable file changed") || w.count("contents/") != 0 {
			t.Fatalf("%s: %+v (calls %v)", name, a, w.calls)
		}
	}
}

// Deleting a manifest is a change (resolve-promoted-image drops removed files; the classifier must not), but it brings no new
// image: config. A rename is a change too.
func TestRemovedOrRenamedManifestIsConfig(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/prod/manifests/worker.yaml": fmt.Sprintf(deployment, "ghcr.io/acme/worker:1"), "prod-1/prod/values.yaml": valuesBase},
		map[string]string{"prod-1/prod/values.yaml": valuesBase})
	a := classify(t, w, nil)
	if a.Kind != Config || fmt.Sprint(envOf(t, a, "prod-1", "prod").Changed) != "[prod-1/prod/manifests/worker.yaml: removed]" {
		t.Fatalf("%+v", a)
	}
	w = newWorld(map[string]string{"prod-1/prod/manifests/worker.yaml": fmt.Sprintf(deployment, "ghcr.io/acme/worker:1")},
		map[string]string{"prod-1/prod/manifests/jobs.yaml": fmt.Sprintf(deployment, "ghcr.io/acme/worker:1")})
	w.compare = []ghFile{{Filename: "prod-1/prod/manifests/jobs.yaml", Status: "renamed", PreviousFilename: "prod-1/prod/manifests/worker.yaml"}}
	a = classify(t, w, nil)
	if a.Kind != Config || !strings.Contains(a.Reason, "renamed from prod-1/prod/manifests/worker.yaml") {
		t.Fatalf("%+v", a)
	}
	// A renamed file that picks up a new image on the way is an image release.
	w.files[headSHA]["prod-1/prod/manifests/jobs.yaml"] = fmt.Sprintf(deployment, "ghcr.io/acme/worker:2")
	if a = classify(t, w, nil); a.Kind != ImageRelease {
		t.Fatalf("%+v", a)
	}
}

func TestUnparseableYAMLFailsClosedAndIsStored(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/prod/values.yaml": valuesBase}, map[string]string{"prod-1/prod/values.yaml": "rollout: [unclosed\n"})
	st := newStore()
	a, err := service(w, st, nil).Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ImageRelease || !a.FailClosed || !strings.Contains(a.Reason, "does not parse at the head") || a.Outcome != Computed {
		t.Fatalf("%+v", a)
	}
	if st.creates != 1 {
		t.Fatalf("a parse failure is deterministic: stored")
	}
}

func TestMoreThan300FilesFailsClosed(t *testing.T) {
	w := newWorld(map[string]string{}, map[string]string{})
	for i := 0; i < 300; i++ {
		w.compare = append(w.compare, ghFile{Filename: fmt.Sprintf("docs/%d.md", i), Status: "added"})
	}
	a := classify(t, w, nil)
	if a.Kind != ImageRelease || !a.FailClosed || !strings.Contains(a.Reason, "GitHub lists at most 300") {
		t.Fatalf("%+v", a)
	}
}

func TestTooManyDeployableFilesFailsClosed(t *testing.T) {
	w := newWorld(map[string]string{}, map[string]string{})
	for i := 0; i < 101; i++ {
		w.compare = append(w.compare, ghFile{Filename: fmt.Sprintf("prod-1/e%d/values.yaml", i), Status: "added"})
	}
	a := classify(t, w, nil)
	if a.Kind != ImageRelease || !a.FailClosed || w.count("contents/") != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestChartPinAndOwnChartNeedARenderCheck(t *testing.T) {
	v := valuesBase + "chart:\n  repoURL: https://charts.example/airframe\n  targetRevision: %s\n"
	w := newWorld(map[string]string{"prod-1/prod/values.yaml": fmt.Sprintf(v, "v1.2.0")}, map[string]string{"prod-1/prod/values.yaml": fmt.Sprintf(v, "v1.3.0")})
	a := classify(t, w, nil)
	if a.Kind != Config || a.RenderCheck != RenderRequired || envOf(t, a, "prod-1", "prod").RenderCheck != RenderRequired {
		t.Fatalf("%+v", a)
	}
	// A helper of an own chart is deployable though it is not YAML, and is not parsed.
	w = newWorld(map[string]string{"prod-1/prod/chart/templates/_helpers.tpl": "{{- define \"x\" -}}a{{- end -}}"},
		map[string]string{"prod-1/prod/chart/templates/_helpers.tpl": "{{- define \"x\" -}}b{{- end -}}"})
	a = classify(t, w, nil)
	if a.Kind != Config || a.RenderCheck != RenderRequired || a.FailClosed {
		t.Fatalf("%+v", a)
	}
	// A remote kustomize base moving.
	k := "resources:\n  - https://github.com/acme/bases//shop?ref=%s\n"
	w = newWorld(map[string]string{"prod-1/prod/kustomization.yaml": fmt.Sprintf(k, "v1")}, map[string]string{"prod-1/prod/kustomization.yaml": fmt.Sprintf(k, "v2")})
	if a = classify(t, w, nil); a.Kind != Config || a.RenderCheck != RenderRequired {
		t.Fatalf("%+v", a)
	}
}

func TestMixedPullRequestIsAnImageRelease(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/staging/values.yaml": valuesBase, "prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.0")},
		map[string]string{"prod-1/staging/values.yaml": strings.Replace(valuesBase, "replicas: 1", "replicas: 3", 1), "prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.1")})
	a := classify(t, w, nil)
	if a.Kind != ImageRelease || len(a.Envs) != 1 {
		t.Fatalf("%+v", a)
	}
}

// Two environments of different kinds: each is reported, and the pull request takes the highest.
func TestTwoEnvironmentsOfDifferentKinds(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.0"), "prod-1/prod/values.yaml": valuesBase},
		map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.1"), "prod-1/prod/values.yaml": strings.Replace(valuesBase, "replicas: 1", "replicas: 2", 1)})
	a := classify(t, w, nil)
	if a.Kind != ImageRelease || envOf(t, a, "prod-1", "prod").Kind != Config || envOf(t, a, "prod-1", "staging").Kind != ImageRelease {
		t.Fatalf("%+v", a)
	}
	// config + no-op: config
	w = newWorld(map[string]string{"prod-1/staging/values.yaml": valuesBase, "prod-1/prod/values.yaml": valuesBase},
		map[string]string{"prod-1/staging/values.yaml": valuesBase + "# note\n", "prod-1/prod/values.yaml": strings.Replace(valuesBase, "replicas: 1", "replicas: 2", 1)})
	a = classify(t, w, nil)
	if a.Kind != Config || envOf(t, a, "prod-1", "staging").Kind != NoOp {
		t.Fatalf("%+v", a)
	}
}

// --- the store: write-once per head, computed once ---

// Done when (CG-1): a second ask after a release turns healthy returns the frozen answer.
func TestWriteOncePerHeadFreezesRollbackEligibility(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "2.0")},
		map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.9")})
	rec := &records{recs: map[string][]releases.Record{}}
	st := newStore()
	s := service(w, st, rec)
	a, err := s.Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil || a.Kind != ImageRelease || a.Outcome != Computed {
		t.Fatalf("%v %+v", err, a)
	}
	rec.mu.Lock()
	rec.recs["prod-1/staging"] = []releases.Record{healthy("c8", "ghcr.io/acme/shop:1.9", "2026-11-24T11:00:00Z")}
	rec.mu.Unlock()
	compares := w.count("compare/")
	b, err := s.Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil || b.Kind != ImageRelease || b.Outcome != Stored || b.ComputedAt != a.ComputedAt || w.count("compare/") != compares {
		t.Fatalf("the stored answer, not a new one: %v %+v", err, b)
	}
	g, err := s.Get(context.Background(), shop, 7, headSHA)
	if err != nil || g.Kind != ImageRelease || g.Outcome != Stored {
		t.Fatalf("%v %+v", err, g)
	}
	cm, _ := st.Get(context.Background(), shop.Namespace, "change-kind-pr-7")
	if cm.Labels["hangar.io/subcomponent"] != "change-kind" || cm.Labels["hangar.io/app"] != "shop" || cm.Labels["hangar.io/pull-request"] != "7" || len(cm.Data) != 1 || cm.Data[headSHA[:12]] == "" {
		t.Fatalf("%+v", cm)
	}
	// Another head of the same pull request gets its own key, and is computed afresh.
	w.files[oldHead] = w.files[headSHA]
	if _, err := s.Classify(context.Background(), shop, 7, oldHead, false); err != nil {
		t.Fatal(err)
	}
	cm, _ = st.Get(context.Background(), shop.Namespace, "change-kind-pr-7")
	if len(cm.Data) != 2 || cm.Data[headSHA[:12]] == "" || cm.Data[oldHead[:12]] == "" {
		t.Fatalf("%v", cm.Data)
	}
}

// Done when (CG-1): two concurrent asks for one head compute once.
func TestConcurrentAsksComputeOnce(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/prod/values.yaml": valuesBase}, map[string]string{"prod-1/prod/values.yaml": strings.Replace(valuesBase, "replicas: 1", "replicas: 2", 1)})
	w.hold = make(chan struct{})
	st := newStore()
	s := service(w, st, nil)
	const n = 11 // every gate starts at once
	answers := make([]*Answer, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answers[i], errs[i] = s.Classify(context.Background(), shop, 7, headSHA, false)
		}(i)
	}
	// Everyone has joined the flight once all of them are waiting and the one computation is in the comparison.
	deadline := time.Now().Add(5 * time.Second)
	for w.count("compare/") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(w.hold)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || answers[i].Kind != Config || answers[i].ComputedAt != answers[0].ComputedAt {
			t.Fatalf("%d: %v %+v", i, errs[i], answers[i])
		}
	}
	if c := w.count("compare/"); c != 1 {
		t.Fatalf("one computation for the head, got %d comparisons", c)
	}
	if st.creates != 1 || st.patches != 0 {
		t.Fatalf("one write: %d creates, %d patches", st.creates, st.patches)
	}
}

// Two replicas compute at the same time (each has its own single flight): the write-once store keeps the first answer, and the
// second replica returns that one, not its own.
func TestTwoReplicasAgreeThroughTheStore(t *testing.T) {
	st := newStore()
	w1 := newWorld(map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "2.0")}, map[string]string{"prod-1/staging/release.yaml": release("ghcr.io/acme/shop", "1.9")})
	first, err := service(w1, st, nil).Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil {
		t.Fatal(err)
	}
	// The second replica computed a different answer (the release had turned healthy) and only then writes: it must lose.
	s2 := service(w1, st, &records{recs: map[string][]releases.Record{"prod-1/staging": {healthy("c8", "ghcr.io/acme/shop:1.9", "2026-11-24T11:00:00Z")}}})
	c, _, err := s2.compute(context.Background(), shop, 7, headSHA)
	if err != nil || c.Kind != Rollback {
		t.Fatalf("%v %+v", err, c)
	}
	got, err := s2.put(context.Background(), shop, 7, c)
	if err != nil || got.Kind != first.Kind || got.Outcome != Stored {
		t.Fatalf("the first write wins: %v %+v", err, got)
	}
}

// A read that failed is a fail-closed answer that is not stored: a later ask computes again.
func TestReadFailureFailsClosedUnstored(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/prod/values.yaml": valuesBase}, map[string]string{"prod-1/prod/values.yaml": strings.Replace(valuesBase, "replicas: 1", "replicas: 2", 1)})
	w.fail["contents/"] = http.StatusBadGateway
	st := newStore()
	s := service(w, st, nil)
	a, err := s.Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil || a.Kind != ImageRelease || !a.FailClosed || a.Outcome != Unstored || st.creates != 0 {
		t.Fatalf("%v %+v", err, a)
	}
	delete(w.fail, "contents/")
	a, err = s.Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil || a.Kind != Config || a.Outcome != Computed {
		t.Fatalf("%v %+v", err, a)
	}
}

func TestHeadMustBeACommitOfThePullRequest(t *testing.T) {
	w := newWorld(map[string]string{}, map[string]string{"README.md": "x"})
	s := service(w, newStore(), nil)
	// an earlier head of the pull request is fine
	w.files[oldHead] = map[string]string{"README.md": "x"}
	if a, err := s.Classify(context.Background(), shop, 7, oldHead, false); err != nil || a.Head != oldHead {
		t.Fatalf("%v %+v", err, a)
	}
	_, err := s.Classify(context.Background(), shop, 7, strings.Repeat("4", 40), false)
	var r Refused
	if !errors.As(err, &r) {
		t.Fatalf("%v", err)
	}
	_, err = s.Classify(context.Background(), shop, 8, headSHA, false)
	var nf NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("no such pull request: %v", err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("A", 40)} {
		_, err = s.Classify(context.Background(), shop, 7, bad, false)
		var inv Invalid
		if !errors.As(err, &inv) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := ParseNumber("07"); err == nil {
		t.Fatal("a number is written plainly")
	}
	if _, err := ParseNumber("0"); err == nil {
		t.Fatal("zero is no pull request")
	}
}

func TestCopyOntoTheReleaseRecord(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/prod/values.yaml": valuesBase}, map[string]string{"prod-1/prod/values.yaml": strings.Replace(valuesBase, "replicas: 1", "replicas: 2", 1)})
	st := newStore()
	st.Create(context.Background(), shop.Namespace, &corev1.ConfigMap{ObjectMeta: objectMetaRecord("release-tracking-c1"), Data: map[string]string{"prUrl": prURL}})
	st.Create(context.Background(), shop.Namespace, &corev1.ConfigMap{ObjectMeta: objectMetaRecord("release-tracking-c0"), Data: map[string]string{"prUrl": prURL + "0"}})
	s := service(w, st, nil)
	a, err := s.Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil || a.Record != "release-tracking-c1" || a.RecordCopy != "copied" {
		t.Fatalf("%v %+v", err, a)
	}
	rec, _ := st.Get(context.Background(), shop.Namespace, "release-tracking-c1")
	var c Classification
	if json.Unmarshal([]byte(rec.Data["changeKind."+headSHA[:12]]), &c) != nil || c.Kind != Config || c.Head != headSHA {
		t.Fatalf("%v", rec.Data)
	}
	if a, _ = s.Classify(context.Background(), shop, 7, headSHA, false); a.RecordCopy != "already-copied" || a.Outcome != Stored {
		t.Fatalf("%+v", a)
	}
	// No record: said so, and the call still answers.
	a, err = service(w, newStore(), nil).Classify(context.Background(), shop, 7, headSHA, false)
	if err != nil || a.RecordCopy != "no-record" || a.Record != "" {
		t.Fatalf("%v %+v", err, a)
	}
}

func objectMetaRecord(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Labels: map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": "shop"}}
}

func TestDryRunAndGetWriteNothing(t *testing.T) {
	w := newWorld(map[string]string{"prod-1/prod/values.yaml": valuesBase}, map[string]string{"prod-1/prod/values.yaml": strings.Replace(valuesBase, "replicas: 1", "replicas: 2", 1)})
	st := newStore()
	s := service(w, st, nil)
	if _, err := s.Get(context.Background(), shop, 7, headSHA); !errors.As(err, new(NotFound)) {
		t.Fatalf("nothing stored yet: %v", err)
	}
	a, err := s.Classify(context.Background(), shop, 7, headSHA, true)
	if err != nil || a.Kind != Config || a.Outcome != DryRun || st.creates != 0 || st.patches != 0 {
		t.Fatalf("%v %+v", err, a)
	}
	if _, err := s.Get(context.Background(), shop, 7, headSHA); !errors.As(err, new(NotFound)) {
		t.Fatalf("a dry run stores nothing: %v", err)
	}
}

func TestDeployablePredicate(t *testing.T) {
	for f, want := range map[string]bool{
		"prod-1/prod/values.yaml":                  true,
		"prod-1/prod/release.yaml":                 true,
		"prod-1/base.yaml":                         true,
		"prod-1/prod/manifests/cm.json":            true,
		"prod-1/prod/Kustomization":                true,
		"kustomization.yaml":                       true,
		"prod-1/prod/chart/templates/_helpers.tpl": true,
		"prod-1/prod/chart/Chart.lock":             true,
		".tekton/pull-request-values.yaml":         false,
		"releases/shop@1.0.0-abc.yaml":             false,
		"README.md":                                false,
		"prod-1/README.md":                         false,
		"prod-1/prod/notes.md":                     false,
	} {
		if Deployable(f) != want {
			t.Errorf("%s: want %v", f, want)
		}
	}
}
