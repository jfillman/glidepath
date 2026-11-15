package server

import (
	"context"
	"strings"
	"testing"

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
}

func (f *fakeLineage) Resolve(_ context.Context, ns, image, commit string) lineage.Resolution {
	f.namespace, f.image, f.commit = ns, image, commit
	return f.res
}

func anOrigin() lineage.Resolution {
	return lineage.Resolution{Origin: &lineage.Origin{ChainID: buildChain, ChainSlug: "ornate-ram", TraceID: "98f0963d6bc08cb69e6051852fc528de",
		Traceparent: buildTP, FlowStartTime: "2026-11-15T20:53:01.775206426Z", FinishedAt: "2026-11-15T20:56:00Z", Run: "ci-0-build-nm682"}}
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
		pipelineruns.AnnotationOriginFlowStart: "2026-11-15T20:53:01.775206426Z", pipelineruns.AnnotationOriginFinishedAt: "2026-11-15T20:56:00Z",
	} {
		if ann[k] != v {
			t.Errorf("%s = %v, want %s", k, ann[k], v)
		}
	}
	if param(store.objs[name], "chain-id") != nil || param(store.objs[name], "flow-traceparent") != nil {
		t.Fatalf("the promotion keeps its own chain: nothing is passed as a chain id or trace param: %v", store.objs[name]["spec"])
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
			return apierrors.NewForbidden(pipelineruns.GVR.GroupResource(), "x", context.Canceled)
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
