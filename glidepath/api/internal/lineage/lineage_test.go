package lineage

import (
	"context"
	"errors"
	"strings"
	"testing"
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
	return (&Resolver{Sources: srcs}).Resolve(context.Background(), "app-gate-api-cicd", repo+":2.0.14-e65cefa", commit)
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
		o.Run != "ci-0-build-nm682" || o.Digest != dig1 || o.FinishedAt != "2026-11-15T20:56:00Z" || o.FlowStartTime != "2026-11-15T20:53:01.775206426Z" {
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
	r := (&Resolver{Sources: []Source{fake{runs: []Run{buildRun("b", dig1, "2026-11-15T20:56:00Z", chain)}}}}).Resolve(context.Background(), "ns", repo+":1", "e65cefa")
	if r.Origin != nil {
		t.Fatalf("%+v", r)
	}
}
