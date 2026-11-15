package lineage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/glidepath/api/internal/results"
)

const (
	repo   = "ghcr.io/jfillman/gate-api"
	commit = "e65cefaea07087f37192869f86f55a99036cc87a"
	chain  = "94e8228a-5cdc-4350-914a-d73802e5b296"
	trace  = "98f0963d6bc08cb69e6051852fc528de"
	tp     = "00-" + trace + "-a71828a66b3e8c7d-01"
	dig1   = "sha256:e8da3d9762920f58cdf2e048f6496b4533075a181f035c005fe4ebc0707a8d39"
	dig2   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func res(kv ...string) []any {
	out := []any{}
	for i := 0; i < len(kv); i += 2 {
		out = append(out, Obj{"name": kv[i], "value": kv[i+1]})
	}
	return out
}

// buildRun is a succeeded build PipelineRun as the cluster (or Tekton Results) holds it, with its preflight TaskRun.
func buildRun(name, digest, finished, chainID string) Run {
	return Run{
		PipelineRun: Obj{
			"metadata": Obj{"name": name, "labels": Obj{"tekton.dev/pipeline": "build"}},
			"status": Obj{
				"conditions":     []any{Obj{"type": "Succeeded", "status": "True"}},
				"completionTime": finished,
				"results":        res("IMAGE_URL", repo, "IMAGE_DIGEST", digest, "CHAINS-GIT_COMMIT", commit),
			},
		},
		TaskRuns: []Obj{{
			"metadata": Obj{"labels": Obj{"tekton.dev/pipelineTask": "preflight"}},
			"status":   Obj{"results": res("chain-id", chainID, "chain-slug", "ornate-ram", "traceparent", tp, "start-time", "2026-11-15T20:53:01.775206426Z")},
		}},
	}
}

type fake struct {
	runs []Run
	err  error
}

func (f fake) Runs(context.Context, string, string) ([]Run, error) { return f.runs, f.err }

func resolve(srcs ...Source) Resolution {
	return (&Resolver{Sources: srcs}).Resolve(context.Background(), "app-gate-api-cicd", repo+":2.0.14-e65cefa", commit, "")
}

func resolveDigest(digest string, srcs ...Source) Resolution {
	return (&Resolver{Sources: srcs}).Resolve(context.Background(), "app-gate-api-cicd", repo+":2.0.14-e65cefa", commit, digest)
}

// The release file pins a digest: the build that pushed THAT digest is the origin, whatever else was built from the commit.
func TestAPinnedDigestSelectsItsBuildAndAnotherDigestIsNotTheOrigin(t *testing.T) {
	b1 := buildRun("b1", dig1, "2026-11-15T20:56:00Z", chain)
	b2 := buildRun("b2", dig2, "2026-11-15T22:00:00Z", "22222222-2222-4222-8222-222222222222")
	if r := resolveDigest(dig2, fake{runs: []Run{b1, b2}}); r.Origin == nil || r.Origin.Run != "b2" {
		t.Fatalf("two builds, different digests, one pinned: the pinned one: %+v %q", r.Origin, r.Unknown)
	}
	if r := resolveDigest(dig1, fake{runs: []Run{b1, b2}}); r.Origin == nil || r.Origin.ChainID != chain {
		t.Fatalf("%+v", r)
	}
	if r := resolveDigest(dig2, fake{runs: []Run{b1}}); r.Origin != nil || !strings.Contains(r.Unknown, "none pushed the pinned digest") {
		t.Fatalf("a build with a different digest is not the origin: %+v", r)
	}
	if r := resolveDigest("sha256:short", fake{runs: []Run{b1}}); r.Origin != nil {
		t.Fatalf("%+v", r)
	}
}

func TestImageRepo(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/o/a:1.0-abc": "ghcr.io/o/a", "ghcr.io/o/a": "ghcr.io/o/a", "localhost:5000/a:1": "localhost:5000/a",
		"ghcr.io/o/a:1@sha256:" + strings.Repeat("a", 64): "ghcr.io/o/a",
	} {
		if got := ImageRepo(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestTheBuildOfTheImageIsTheOrigin(t *testing.T) {
	r := resolve(fake{runs: []Run{buildRun("ci-0-build-nm682", dig1, "2026-11-15T20:56:00Z", chain)}})
	o := r.Origin
	if o == nil || o.ChainID != chain || o.ChainSlug != "ornate-ram" || o.TraceID != trace || o.Traceparent != tp ||
		o.Run != "ci-0-build-nm682" || o.Digest != dig1 || o.FinishedAt != "2026-11-15T20:56:00Z" {
		t.Fatalf("%+v %q", o, r.Unknown)
	}
}

func TestNoBuildIsUnknownWithAReason(t *testing.T) {
	r := resolve(fake{})
	if r.Origin != nil || !strings.Contains(r.Unknown, "no succeeded build") {
		t.Fatalf("%+v", r)
	}
}

func TestAnotherImageOrCommitOrAFailedBuildIsNotThisReleases(t *testing.T) {
	other := buildRun("pr-build", dig1, "2026-11-15T20:56:00Z", chain)
	other.PipelineRun["status"].(Obj)["results"] = res("IMAGE_URL", repo+"-pr", "IMAGE_DIGEST", dig1, "CHAINS-GIT_COMMIT", commit)
	failed := buildRun("failed", dig1, "2026-11-15T20:56:00Z", chain)
	failed.PipelineRun["status"].(Obj)["conditions"] = []any{Obj{"type": "Succeeded", "status": "False"}}
	wrongCommit := buildRun("old", dig1, "2026-11-15T20:56:00Z", chain)
	wrongCommit.PipelineRun["status"].(Obj)["results"] = res("IMAGE_URL", repo, "IMAGE_DIGEST", dig1, "CHAINS-GIT_COMMIT", strings.Repeat("a", 40))
	r := resolve(fake{runs: []Run{other, failed, wrongCommit}})
	if r.Origin != nil {
		t.Fatalf("a build of another image, commit or a failed one is no origin: %+v", r.Origin)
	}
}

func TestTwoBuildsOfOneDigestResolveToTheEarliest(t *testing.T) {
	// The live copy and the archive hold the same run: one build, not two.
	first := buildRun("b1", dig1, "2026-11-15T20:56:00Z", chain)
	rerun := buildRun("b2", dig1, "2026-11-15T22:00:00Z", "22222222-2222-4222-8222-222222222222")
	r := resolve(fake{runs: []Run{rerun, first}}, fake{runs: []Run{first}})
	if r.Origin == nil || r.Origin.ChainID != chain {
		t.Fatalf("the earliest build produced the content: %+v %q", r.Origin, r.Unknown)
	}
}

func TestTwoBuildsOfDifferentDigestsAreAmbiguousAndGuessNothing(t *testing.T) {
	r := resolve(fake{runs: []Run{
		buildRun("b1", dig1, "2026-11-15T20:56:00Z", chain),
		buildRun("b2", dig2, "2026-11-15T22:00:00Z", "22222222-2222-4222-8222-222222222222"),
	}})
	if r.Origin != nil || !strings.Contains(r.Unknown, "different digests") {
		t.Fatalf("%+v", r)
	}
}

func TestAFailedReadIsNeverNoBuild(t *testing.T) {
	// One source finds a build, the other cannot answer: the failed one might hold a second build, so nothing is claimed.
	r := resolve(fake{runs: []Run{buildRun("b1", dig1, "2026-11-15T20:56:00Z", chain)}}, fake{err: errors.New("results down")})
	if r.Origin != nil || !strings.Contains(r.Unknown, "could not read") || !strings.Contains(r.Unknown, "results down") {
		t.Fatalf("%+v", r)
	}
}

func TestABuildWithoutAReadableChainIsUnknown(t *testing.T) {
	b := buildRun("b1", dig1, "2026-11-15T20:56:00Z", "not-a-uuid")
	if r := resolve(fake{runs: []Run{b}}); r.Origin != nil || !strings.Contains(r.Unknown, "chain id") {
		t.Fatalf("%+v", r)
	}
	b = buildRun("b1", "latest", "2026-11-15T20:56:00Z", chain)
	if r := resolve(fake{runs: []Run{b}}); r.Origin != nil {
		t.Fatalf("a digest that is not sha256: %+v", r)
	}
}

func TestNoFullCommitNoLookup(t *testing.T) {
	r := (&Resolver{Sources: []Source{fake{runs: []Run{buildRun("b", dig1, "2026-11-15T20:56:00Z", chain)}}}}).Resolve(context.Background(), "ns", repo+":1", "e65cefa", "")
	if r.Origin != nil {
		t.Fatalf("%+v", r)
	}
}

// ---- shapes: the resolver and the API stamp only what the cluster's admission policy accepts ----

// policyPatterns are the literals of the promotion-lineage validation in jfillman/apron's glidepath-api-pipelinerun-shape
// (templates/30-policy/kyverno-policies/tower-identity-writes.yaml). Keep them identical to Shapes; apron's own test holds the
// policy to the same strings.
var policyPatterns = map[string]string{
	"glidepath.io/origin-chain-id":    `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
	"glidepath.io/origin-chain-slug":  `^[a-z]{2,20}-[a-z]{2,20}$`,
	"glidepath.io/origin-trace-id":    `^[0-9a-f]{32}$`,
	"glidepath.io/origin-traceparent": `^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`,
	"glidepath.io/origin-finished-at": `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]{1,9})?Z$`,
}

func TestShapesAreTheAdmissionPoliciesPatterns(t *testing.T) {
	if len(Shapes) != len(policyPatterns) {
		t.Fatalf("one table: %v vs %v", Shapes, policyPatterns)
	}
	for k, p := range policyPatterns {
		if Shapes[k] != p {
			t.Errorf("%s: resolver pattern %q is not the policy's %q", k, Shapes[k], p)
		}
	}
}

// Every origin the resolver returns, for every edge value of a build's context, stamps only values the policy's patterns accept.
func TestAValueTheResolverReturnsIsNeverOneAdmissionRefuses(t *testing.T) {
	re := map[string]*regexp.Regexp{}
	for k, p := range policyPatterns {
		re[k] = regexp.MustCompile(p)
	}
	tps := []string{tp, "00-" + trace + "-a71828a66b3e8c7d-00", "00-" + trace + "-a71828a66b3e8c7d-02", "01-" + trace + "-a71828a66b3e8c7d-01", "00-" + trace + "-A71828a66b3e8c7d-01"}
	slugs := []string{"ornate-ram", "Ornate-Ram", "ornate", "ornate-ram-x", "a-b", "ornate-" + strings.Repeat("a", 21), "ornate_ram", ""}
	times := []string{"2026-11-15T20:56:00Z", "2026-11-15T20:56:00.123456789Z", "2026-11-15T20:56:00.1Z", "2026-11-15T20:56:00+02:00", "2026-11-15T20:56:00.000000000Z", "2026-11-15 20:56:00Z", ""}
	resolved := 0
	for _, tpv := range tps {
		for _, sl := range slugs {
			for _, ts := range times {
				b := buildRun("b", dig1, ts, chain)
				pre := b.TaskRuns[0]["status"].(Obj)
				pre["results"] = res("chain-id", chain, "chain-slug", sl, "traceparent", tpv)
				r := resolve(fake{runs: []Run{b}})
				if r.Origin == nil {
					continue
				}
				resolved++
				a, ok := r.Origin.Annotations()
				if !ok {
					t.Fatalf("tp=%q slug=%q time=%q: resolved but not stampable", tpv, sl, ts)
				}
				for k, v := range a {
					if !re[k].MatchString(v) {
						t.Errorf("tp=%q slug=%q time=%q: %s=%q would be refused by admission", tpv, sl, ts, k, v)
					}
				}
			}
		}
	}
	if resolved == 0 {
		t.Fatal("vacuous: nothing resolved")
	}
	// -00 flags and a malformed trace context are no origin at all
	b := buildRun("b", dig1, "2026-11-15T20:56:00Z", chain)
	b.TaskRuns[0]["status"].(Obj)["results"] = res("chain-id", chain, "chain-slug", "ornate-ram", "traceparent", "00-"+trace+"-a71828a66b3e8c7d-00")
	if r := resolve(fake{runs: []Run{b}}); r.Origin != nil {
		t.Fatalf("a traceparent ending -00 is refused by admission: %+v", r.Origin)
	}
	// a bad slug is left out, the rest stands
	b = buildRun("b", dig1, "2026-11-15T20:56:00Z", chain)
	b.TaskRuns[0]["status"].(Obj)["results"] = res("chain-id", chain, "chain-slug", "Bad Slug", "traceparent", tp)
	if r := resolve(fake{runs: []Run{b}}); r.Origin == nil || r.Origin.ChainSlug != "" {
		t.Fatalf("%+v", r)
	}
}

// ---- the commit-shape check and the 20-candidate cap ----

type countingSource struct{ calls int }

func (c *countingSource) Runs(context.Context, string, string) ([]Run, error) {
	c.calls++
	return nil, nil
}

func TestOnlyAFullLowercaseCommitIsLookedUp(t *testing.T) {
	for _, bad := range []string{"", "e65cefa", strings.ToUpper(commit), commit + "a", commit[:39], "g" + commit[1:], " " + commit} {
		src := &countingSource{}
		r := (&Resolver{Sources: []Source{src}}).Resolve(context.Background(), "ns", repo+":1", bad, "")
		if r.Origin != nil || src.calls != 0 || r.Unknown == "" {
			t.Errorf("commit %q: %d source calls, %+v", bad, src.calls, r)
		}
	}
	src := &countingSource{}
	(&Resolver{Sources: []Source{src}}).Resolve(context.Background(), "ns", repo+":1", commit, "")
	if src.calls != 1 {
		t.Fatalf("a full commit is looked up once per source: %d", src.calls)
	}
}

type fakeArchive struct {
	rows []results.Row
	gets int
}

func (f *fakeArchive) List(_ context.Context, _ string, _, _ time.Time, limit int) ([]results.Row, error) {
	if limit != ListLimit {
		return nil, fmt.Errorf("limit %d", limit)
	}
	return f.rows, nil
}

func (f *fakeArchive) Get(_ context.Context, _, result string) (*results.Run, error) {
	f.gets++
	r := buildRun(result, dig1, "2026-11-15T20:56:00Z", chain)
	return &results.Run{Result: result, PipelineRun: r.PipelineRun, TaskRuns: r.TaskRuns}, nil
}

func row(result, pipeline, c, status string) results.Row {
	return results.Row{Result: result, Pipeline: &pipeline, Commit: &c, Status: status}
}

func TestTheArchiveReadsTwentyCandidatesAndFailsClosedPastThat(t *testing.T) {
	rows := []results.Row{row("other-commit", "build", strings.Repeat("a", 40), "succeeded"), row("not-a-build", "deploy", commit, "succeeded"),
		row("failed", "build", commit, "failed")}
	for i := 0; i < maxCandidates; i++ {
		rows = append(rows, row(fmt.Sprintf("b%02d", i), "build", commit, "succeeded"))
	}
	a := &fakeArchive{rows: rows}
	runs, err := ArchiveSource{Results: a}.Runs(context.Background(), "ns", commit)
	if err != nil || len(runs) != maxCandidates || a.gets != maxCandidates {
		t.Fatalf("exactly the cap, only builds of this commit that succeeded: %d runs, %d gets, %v", len(runs), a.gets, err)
	}
	a = &fakeArchive{rows: append(rows, row("b-extra", "build", commit, "succeeded"))}
	if _, err := (ArchiveSource{Results: a}).Runs(context.Background(), "ns", commit); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("one past the cap is a failed read, never a truncation: %v", err)
	}
	// ...which the resolver answers as unknown, not as a build
	r := (&Resolver{Sources: []Source{ArchiveSource{Results: a}}}).Resolve(context.Background(), "ns", repo+":1", commit, "")
	if r.Origin != nil || !strings.Contains(r.Unknown, "could not read") {
		t.Fatalf("%+v", r)
	}
}
