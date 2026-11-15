package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glidepath/api/internal/lineage"
	"github.com/glidepath/api/internal/pipelineruns"
	"github.com/glidepath/api/internal/releases"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	buildChain = "94e8228a-5cdc-4350-914a-d73802e5b296"
	buildTP    = "00-98f0963d6bc08cb69e6051852fc528de-a71828a66b3e8c7d-01"
)

// fakeLineage answers every lookup with one resolution and remembers what it was asked.
type fakeLineage struct {
	res           lineage.Resolution
	image, commit string
	namespace     string
	digest        string
}

func (f *fakeLineage) Resolve(_ context.Context, ns, image, commit, digest string) lineage.Resolution {
	f.namespace, f.image, f.commit, f.digest = ns, image, commit, digest
	return f.res
}

func anOrigin() lineage.Resolution {
	return lineage.Resolution{Origin: &lineage.Origin{ChainID: buildChain, ChainSlug: "ornate-ram", TraceID: "98f0963d6bc08cb69e6051852fc528de",
		Traceparent: buildTP, FinishedAt: "2026-11-15T20:56:00Z", Run: "ci-0-build-nm682"}}
}

func annotationsOf(store *fakeStore, name string) map[string]any {
	return store.objs[name]["metadata"].(map[string]any)["annotations"].(map[string]any)
}

// A promotion carries the build flow of the image it releases, resolved by the API for the image and commit the source
// environment's file names, never from the body; it keeps its own chain (no chain-id param: stage-preflight mints one).
func TestPromoteCarriesTheBuildsLineage(t *testing.T) {
	s, store, _, _ := withEnvs(t, releaser)
	l := &fakeLineage{res: anOrigin()}
	s.Envs.Lineage = l
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k1"),
		`{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev","originChainId":"`+strings.Repeat("9", 8)+`-0000-4000-8000-000000000000","annotations":{"glidepath.io/origin-chain-id":"forged"}}`)
	if st != 201 {
		t.Fatalf("%d %s", st, body)
	}
	if l.namespace != "app-shop-cicd" || l.image != "ghcr.io/jfillman/shop:1.2.3-abc1234" || l.commit != devSHA {
		t.Fatalf("the lookup is for the release's own image and commit: %+v", l)
	}
	o, _ := out["origin"].(map[string]any)
	if o["chainId"] != buildChain || o["chainSlug"] != "ornate-ram" || o["traceId"] != "98f0963d6bc08cb69e6051852fc528de" || o["run"] != "ci-0-build-nm682" || out["originUnknown"] != nil {
		t.Fatalf("answer: %s", body)
	}
	name := out["pipelineRun"].(string)
	ann := annotationsOf(store, name)
	for k, v := range map[string]string{
		pipelineruns.AnnotationOriginChainID: buildChain, pipelineruns.AnnotationOriginChainSlug: "ornate-ram",
		pipelineruns.AnnotationOriginTraceID: "98f0963d6bc08cb69e6051852fc528de", pipelineruns.AnnotationOriginTraceparent: buildTP,
		pipelineruns.AnnotationOriginFinishedAt: "2026-11-15T20:56:00Z",
	} {
		if ann[k] != v {
			t.Errorf("%s = %v, want %s", k, ann[k], v)
		}
	}
	if param(store.objs[name], "chain-id") != nil || param(store.objs[name], "flow-traceparent") != nil {
		t.Fatalf("the promotion keeps its own chain: nothing is passed as a chain id or trace param: %v", store.objs[name]["spec"])
	}
}

// The source environment's release file pins a digest (promotion.pinDigest): it is what the origin is matched on, read from
// the file and never from the body.
func TestPromoteBindsTheOriginToThePinnedDigest(t *testing.T) {
	s, _, gh, _ := withEnvs(t, releaser)
	pin := "sha256:" + strings.Repeat("a", 64)
	gh.files["jfillman/shop/glidepath/envs/dev.release.yaml"] = "release:\n  image: { repository: ghcr.io/jfillman/shop, tag: 1.2.3-abc1234, digest: " + pin + " }\n"
	l := &fakeLineage{res: anOrigin()}
	s.Envs.Lineage = l
	st, _, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k1"),
		`{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev","digest":"sha256:`+strings.Repeat("b", 64)+`"}`)
	if st != 201 || l.digest != pin {
		t.Fatalf("%d %s digest %q", st, body, l.digest)
	}
}

// Lineage is information, not a gate: nothing resolved still starts the run, and the answer says why.
func TestPromoteWithoutAnOriginStillGoesAheadAndSaysSo(t *testing.T) {
	s, store, _, _ := withEnvs(t, releaser)
	s.Envs.Lineage = &fakeLineage{res: lineage.Resolution{Unknown: "no succeeded build of x at abc1234"}}
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k1"), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
	if st != 201 || out["origin"] != nil || out["originUnknown"] != "origin unknown: no succeeded build of x at abc1234" {
		t.Fatalf("%d %s", st, body)
	}
	for k := range annotationsOf(store, out["pipelineRun"].(string)) {
		if strings.HasPrefix(k, "glidepath.io/origin-chain") || k == pipelineruns.AnnotationOriginTraceID {
			t.Fatalf("no origin, no lineage annotation: %s", k)
		}
	}
}

// strictAnnotations is a cluster whose admission policy for this API's runs does not know the lineage keys yet.
type strictAnnotations struct{ *fakeStore }

func (f strictAnnotations) Create(ctx context.Context, ns string, obj map[string]any, dryRun bool) error {
	for k := range obj["metadata"].(map[string]any)["annotations"].(map[string]any) {
		if strings.HasPrefix(k, "glidepath.io/origin-chain") {
			return apierrors.NewForbidden(pipelineruns.GVR.GroupResource(), "x", errors.New("A run glidepath-api starts may carry only the annotations glidepath.io/origin-ref, origin-sha and origin-event"))
		}
	}
	return f.fakeStore.Create(ctx, ns, obj, dryRun)
}

func TestAnOlderAdmissionPolicyDropsTheLineageNotThePromotion(t *testing.T) {
	s, store, _, _ := withEnvs(t, releaser)
	s.Envs.Runs = strictAnnotations{store}
	s.Envs.Lineage = &fakeLineage{res: anOrigin()}
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k1"), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
	if st != 201 || out["origin"] != nil || !strings.Contains(out["originUnknown"].(string), "admission policy") {
		t.Fatalf("%d %s", st, body)
	}
	ann := annotationsOf(store, out["pipelineRun"].(string))
	if ann[pipelineruns.AnnotationOriginRef] != "refs/heads/main" || ann[pipelineruns.AnnotationOriginChainID] != nil {
		t.Fatalf("the run keeps its guard origin and loses only the lineage: %v", ann)
	}
}

func TestRollbackCarriesTheLineageOfTheImageItGoesBackTo(t *testing.T) {
	s, store, _, _ := withEnvs(t, releaser)
	l := &fakeLineage{res: anOrigin()}
	s.Envs.Lineage = l
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/prod/rollback", hdr("rel", "k1"), `{"cluster":"hangar-prod","targetReleaseId":"rel-1","reason":"budget"}`)
	if st != 201 {
		t.Fatalf("%d %s", st, body)
	}
	if l.image != "ghcr.io/jfillman/shop:1.0.0-1111111" || l.commit != "1111111aaaa" {
		t.Fatalf("the target's own image and commit: %+v", l)
	}
	if o, _ := out["origin"].(map[string]any); o["chainId"] != buildChain {
		t.Fatalf("%s", body)
	}
	if annotationsOf(store, out["pipelineRun"].(string))[pipelineruns.AnnotationOriginChainID] != buildChain {
		t.Fatal("stamped")
	}
}

func recordWith(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: releases.Prefix + name, Namespace: "app-shop-cicd",
		Labels: map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": "shop"}}, Data: data}
}

func TestReleasesCarryAndFilterByOrigin(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	s.Releases = releases.Reader{CS: fake.NewSimpleClientset(
		recordWith("79fa9817-1b1a-4ea2-95fc-205c46efb116", map[string]string{"state": "proposed", "originChainId": buildChain, "originChainSlug": "ornate-ram", "originTraceId": "98f0963d6bc08cb69e6051852fc528de"}),
		recordWith("7200a685-2b64-49ff-8587-686bb992d878", map[string]string{"state": "healthy"}),
	)}
	st, out := getJSON(t, s, "/v1/apps/shop/releases/79fa9817-1b1a-4ea2-95fc-205c46efb116")
	o, _ := out["origin"].(map[string]any)
	if st != 200 || o["chainId"] != buildChain || o["chainSlug"] != "ornate-ram" || o["traceId"] != "98f0963d6bc08cb69e6051852fc528de" {
		t.Fatalf("%d %v", st, out)
	}
	if _, has := getOne(t, s, "7200a685-2b64-49ff-8587-686bb992d878")["origin"]; has {
		t.Fatal("a release with no origin has no origin field")
	}
	st, out = getJSON(t, s, "/v1/apps/shop/releases?fromChain="+buildChain)
	if recs, _ := out["releases"].([]any); st != 200 || len(recs) != 1 || recs[0].(map[string]any)["name"] != releases.Prefix+"79fa9817-1b1a-4ea2-95fc-205c46efb116" {
		t.Fatalf("only the releases that came from that build: %d %v", st, out)
	}
	if st, out = getJSON(t, s, "/v1/apps/shop/releases?fromChain=none-such-0000"); st != 200 || len(out["releases"].([]any)) != 0 {
		t.Fatalf("a flow nothing came from lists nothing, not an error: %d %v", st, out)
	}
	if st, _ = getJSON(t, s, "/v1/apps/shop/releases?fromChain=BAD%20ID"); st != 400 {
		t.Fatalf("a malformed chain id is a 400: %d", st)
	}
	if st, out = getJSON(t, s, "/v1/apps/shop/releases"); st != 200 || len(out["releases"].([]any)) != 2 {
		t.Fatalf("no filter lists all: %d %v", st, out)
	}
}

func getOne(t *testing.T, s *Server, chain string) map[string]any {
	_, out := getJSON(t, s, "/v1/apps/shop/releases/"+chain)
	return out
}

func TestReleaseRunShowsItsOrigin(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	f := withReleaseRun(s, false)
	prs := f.objs["pipelineruns"]
	meta := prs[len(prs)-1]["metadata"].(map[string]any)
	meta["annotations"] = map[string]any{"glidepath.io/origin-chain-id": buildChain, "glidepath.io/origin-chain-slug": "ornate-ram", "glidepath.io/origin-trace-id": "98f0963d6bc08cb69e6051852fc528de"}
	_, out := getJSON(t, s, "/v1/apps/shop/releases/runs/promote-prod-abc1234-0a1b2c")
	o, _ := out["origin"].(map[string]any)
	if o["chainId"] != buildChain || o["chainSlug"] != "ornate-ram" || out["chainId"] != chain {
		t.Fatalf("the run keeps its own chain and shows the build's: %v", out)
	}
}

// recordingStore refuses every create with refuse (when set) and keeps a copy of each submitted run.
type recordingStore struct {
	*fakeStore
	refuse  func(obj map[string]any) error
	mu      sync.Mutex
	creates []map[string]any
}

func (r *recordingStore) Create(ctx context.Context, ns string, obj map[string]any, dryRun bool) error {
	r.mu.Lock()
	r.creates = append(r.creates, copyObj(obj))
	r.mu.Unlock()
	if r.refuse != nil {
		if err := r.refuse(obj); err != nil {
			return err
		}
	}
	return r.fakeStore.Create(ctx, ns, obj, dryRun)
}

func hasLineage(obj map[string]any) bool {
	for k := range obj["metadata"].(map[string]any)["annotations"].(map[string]any) {
		if strings.HasPrefix(k, "glidepath.io/origin-chain") || k == pipelineruns.AnnotationOriginTraceID || k == pipelineruns.AnnotationOriginTraceparent || k == pipelineruns.AnnotationOriginFinishedAt {
			return true
		}
	}
	return false
}

// The retry fires only on the admission refusal that is about these annotations, and resubmits the same run less them.
func TestTheLineageRetryIsOnlyForTheAdmissionRefusalOfTheseAnnotations(t *testing.T) {
	older := apierrors.NewForbidden(pipelineruns.GVR.GroupResource(), "x", errors.New("A run glidepath-api starts may carry only the annotations glidepath.io/origin-ref, origin-sha and origin-event"))
	cases := []struct {
		name    string
		err     error
		creates int
		status  int
	}{
		{"older policy: allowlist refusal", older, 2, 201},
		{"the new shape rule", apierrors.NewForbidden(pipelineruns.GVR.GroupResource(), "x", errors.New("The promotion-lineage annotations must have the shape glidepath-api writes")), 2, 201},
		{"another forbidden", apierrors.NewForbidden(pipelineruns.GVR.GroupResource(), "x", errors.New("taskRunTemplate.serviceAccountName must be pipeline-runner")), 1, 422},
		{"a bad request", apierrors.NewBadRequest("nope"), 1, 422},
		{"the API server down", apierrors.NewServiceUnavailable("down"), 1, 502},
		{"an internal error", apierrors.NewInternalError(errors.New("boom")), 1, 502},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, store, _, _ := withEnvs(t, releaser)
			rec := &recordingStore{fakeStore: store, refuse: func(obj map[string]any) error {
				if hasLineage(obj) || c.creates == 1 {
					return c.err
				}
				return nil
			}}
			s.Envs.Runs = rec
			s.Envs.Lineage = &fakeLineage{res: anOrigin()}
			st, _, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k1"), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
			if len(rec.creates) != c.creates || st != c.status {
				t.Fatalf("%d creates, status %d, want %d and %d: %s", len(rec.creates), st, c.creates, c.status, body)
			}
			if c.creates == 2 {
				first, second := rec.creates[0], rec.creates[1]
				if !hasLineage(first) || hasLineage(second) {
					t.Fatal("the first try carries the lineage, the second does not")
				}
				ann := first["metadata"].(map[string]any)["annotations"].(map[string]any)
				for k := range ann {
					if strings.HasPrefix(k, "glidepath.io/origin-chain") || k == pipelineruns.AnnotationOriginTraceID || k == pipelineruns.AnnotationOriginTraceparent || k == pipelineruns.AnnotationOriginFinishedAt {
						delete(ann, k)
					}
				}
				a, _ := json.Marshal(first)
				b, _ := json.Marshal(second)
				if string(a) != string(b) {
					t.Fatalf("the retry is the same run less the lineage:\n%s\n%s", a, b)
				}
			}
		})
	}
}

// A lookup that does not answer in time is "origin unknown", and the promotion starts.
type slowLineage struct{ release chan struct{} }

func (l slowLineage) Resolve(context.Context, string, string, string, string) lineage.Resolution {
	<-l.release // ignores its context, like a source that is stuck
	return anOrigin()
}

func TestASlowLineageLookupDoesNotStallThePromotion(t *testing.T) {
	s, store, _, _ := withEnvs(t, releaser)
	release := make(chan struct{})
	defer close(release)
	s.Envs.Lineage = slowLineage{release}
	s.Envs.LineageTimeout = 50 * time.Millisecond
	start := time.Now()
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k1"), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
	if st != 201 || out["originUnknown"] != "origin unknown: lineage lookup timed out" || out["origin"] != nil {
		t.Fatalf("%d %s", st, body)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("stalled %v", time.Since(start))
	}
	if hasLineage(store.objs[out["pipelineRun"].(string)]) {
		t.Fatal("no lineage was resolved, none is stamped")
	}
}

// An origin whose values the policy would refuse is never stamped.
func TestAnOriginOutsideTheAdmissionShapesIsNotStamped(t *testing.T) {
	s, store, _, _ := withEnvs(t, releaser)
	bad := anOrigin()
	bad.Origin.Traceparent = "00-98f0963d6bc08cb69e6051852fc528de-a71828a66b3e8c7d-00"
	s.Envs.Lineage = &fakeLineage{res: bad}
	st, out, body := postJSON(t, s, "/v1/apps/shop/environments/staging/promote", hdr("rel", "k1"), `{"cluster":"hangar-prod","sourceEnv":"dev","sourceCluster":"hangar-dev"}`)
	if st != 201 || !strings.Contains(out["originUnknown"].(string), "outside the shape") || hasLineage(store.objs[out["pipelineRun"].(string)]) {
		t.Fatalf("%d %s", st, body)
	}
}
