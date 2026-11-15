// Package lineage resolves the ORIGIN of a promotion or rollback: the flow (chain) that built the image being released.
// glidepath-api stamps it on the release run (annotations glidepath.io/origin-chain-id and friends) and the catalog's
// trusted tasks copy it onto the release record and into the trace (docs/admin/api.md#promotion-lineage).
//
// The origin is read from the BUILD's own run, never from the caller: the build PipelineRun still on the cluster (Tekton
// deletes a finished run about an hour after it ends) or its copy in Tekton Results (the 30-day archive). That run names
// the image repository it pushed (IMAGE_URL), the digest (IMAGE_DIGEST), the commit it built (CHAINS-GIT_COMMIT) and,
// through its preflight task, the chain id, slug and trace the flow minted. A promotion is bound to a build by the image
// repository AND the commit being released, so a caller cannot make a promotion claim another flow's lineage: the
// resolver is given the repository and commit the API itself resolved (from the source environment's release file),
// never a chain id.
//
// Lineage is information, not a gate. Every case this cannot answer (no build found, two builds that produced different
// digests for the commit, a source that could not be read) resolves to Unknown with the reason, and the promotion goes
// ahead without an origin.
package lineage

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/glidepath/api/internal/live"
	"github.com/glidepath/api/internal/results"
)

// Obj is a decoded Kubernetes object.
type Obj = map[string]any

// Run is one finished-or-running build PipelineRun with its TaskRuns.
type Run struct {
	PipelineRun Obj
	TaskRuns    []Obj
}

// Source lists the build runs of a commit in an app's -cicd namespace. A source that cannot answer returns an error:
// that is never read as "no build".
type Source interface {
	Runs(ctx context.Context, namespace, commit string) ([]Run, error)
}

// Origin is the build a release came from.
type Origin struct {
	ChainID       string
	ChainSlug     string
	TraceID       string
	Traceparent   string // the build flow's root traceparent (what a promotion joins)
	FlowStartTime string // the flow root span's start
	FinishedAt    string // when the build run finished (RFC 3339)
	Run           string // the build PipelineRun's name
	Digest        string // the image digest that build pushed
}

// Resolution is an answer: an Origin, or Unknown with why there is none.
type Resolution struct {
	Origin  *Origin
	Unknown string
}

// Resolver asks every Source and decides.
type Resolver struct{ Sources []Source }

var (
	uuidRE        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	traceparentRE = regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-0[01]$`)
	slugRE        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	digestRE      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitRE      = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ImageRepo is imageRef without its tag or digest: ghcr.io/o/app:1.0.0-abc1234 is ghcr.io/o/app.
func ImageRepo(imageRef string) string {
	if i := strings.Index(imageRef, "@"); i >= 0 {
		imageRef = imageRef[:i]
	}
	if i := strings.LastIndex(imageRef, ":"); i > strings.LastIndex(imageRef, "/") {
		imageRef = imageRef[:i]
	}
	return imageRef
}

func str(m Obj, k string) string { s, _ := m[k].(string); return s }
func mp(v any) Obj               { m, _ := v.(Obj); return m }

func resultsOf(o Obj) map[string]string {
	out := map[string]string{}
	rs, _ := mp(o["status"])["results"].([]any)
	for _, r := range rs {
		rm := mp(r)
		if v, ok := rm["value"].(string); ok {
			out[str(rm, "name")] = strings.TrimSpace(v)
		}
	}
	return out
}

func succeeded(o Obj) bool {
	cs, _ := mp(o["status"])["conditions"].([]any)
	for _, c := range cs {
		if cm := mp(c); str(cm, "type") == "Succeeded" {
			return str(cm, "status") == "True"
		}
	}
	return false
}

// IsBuildOf is the cheap pre-filter a Source applies: a succeeded build PipelineRun whose Chains type hint names commit.
func IsBuildOf(pr Obj, commit string) bool {
	lbl := mp(mp(pr["metadata"])["labels"])
	return str(lbl, "tekton.dev/pipeline") == "build" && succeeded(pr) && resultsOf(pr)["CHAINS-GIT_COMMIT"] == commit
}

type build struct {
	Origin
	finished time.Time
}

// buildOf reads one run as a build of repo at commit; ok is false when it is not one, why says so when it matters.
func buildOf(r Run, repo, commit string) (b build, ok bool, why string) {
	pr := r.PipelineRun
	if !IsBuildOf(pr, commit) {
		return b, false, ""
	}
	res := resultsOf(pr)
	if res["IMAGE_URL"] != repo {
		return b, false, "" // another image (a pull request build pushes to <app>-pr)
	}
	name := str(mp(pr["metadata"]), "name")
	if !digestRE.MatchString(res["IMAGE_DIGEST"]) {
		return b, false, fmt.Sprintf("build %s recorded no image digest", name)
	}
	var pre Obj
	for _, tr := range r.TaskRuns {
		if str(mp(mp(tr["metadata"])["labels"]), "tekton.dev/pipelineTask") == "preflight" {
			pre = tr
		}
	}
	pres := resultsOf(pre)
	tp := traceparentRE.FindStringSubmatch(pres["traceparent"])
	if pre == nil || !uuidRE.MatchString(pres["chain-id"]) || tp == nil {
		return b, false, fmt.Sprintf("build %s has no readable chain id and trace (its preflight record is missing or malformed)", name)
	}
	slug := pres["chain-slug"]
	if !slugRE.MatchString(slug) {
		slug = ""
	}
	fin := str(mp(pr["status"]), "completionTime")
	t, err := time.Parse(time.RFC3339Nano, fin)
	if err != nil {
		return b, false, fmt.Sprintf("build %s has no completion time", name)
	}
	return build{Origin: Origin{ChainID: pres["chain-id"], ChainSlug: slug, TraceID: tp[1], Traceparent: pres["traceparent"],
		FlowStartTime: pres["start-time"], FinishedAt: t.UTC().Format(time.RFC3339Nano), Run: name, Digest: res["IMAGE_DIGEST"]}, finished: t}, true, ""
}

// Resolve finds the build of imageRef at commit (a full 40-hex sha) in namespace.
//
//   - No build found: Unknown (the build is older than the archive, or ran elsewhere).
//   - Builds that pushed the SAME digest (a rerun of the same commit): the earliest finished one, the build that produced
//     the content; the later ones only pushed it again.
//   - Builds that pushed DIFFERENT digests for the commit: Unknown, because the tag could point at either and the API
//     does not read the registry here. Nothing is guessed.
//   - A source that fails is Unknown too, with the error, even when another found a build: the failed one might hold a
//     second build that makes the answer ambiguous.
func (r *Resolver) Resolve(ctx context.Context, namespace, imageRef, commit string) Resolution {
	repo := ImageRepo(imageRef)
	if repo == "" || !commitRE.MatchString(commit) {
		return Resolution{Unknown: "the release names no image repository or no full commit sha to look a build up by"}
	}
	seen := map[string]bool{}
	var builds []build
	var whys []string
	for _, src := range r.Sources {
		runs, err := src.Runs(ctx, namespace, commit)
		if err != nil {
			return Resolution{Unknown: "could not read the build runs: " + err.Error()}
		}
		for _, run := range runs {
			name := str(mp(run.PipelineRun["metadata"]), "name")
			if seen[name] {
				continue
			}
			seen[name] = true
			if b, ok, why := buildOf(run, repo, commit); ok {
				builds = append(builds, b)
			} else if why != "" {
				whys = append(whys, why)
			}
		}
	}
	if len(builds) == 0 {
		if len(whys) > 0 {
			return Resolution{Unknown: strings.Join(whys, "; ")}
		}
		return Resolution{Unknown: fmt.Sprintf("no succeeded build of %s at %s on the cluster or in the run archive (older than the archive, or built elsewhere)", repo, commit[:7])}
	}
	digests := map[string]bool{}
	for _, b := range builds {
		digests[b.Digest] = true
	}
	if len(digests) > 1 {
		return Resolution{Unknown: fmt.Sprintf("%d builds of %s at %s pushed %d different digests: the tag could be any of them", len(builds), repo, commit[:7], len(digests))}
	}
	sort.Slice(builds, func(i, j int) bool {
		if !builds[i].finished.Equal(builds[j].finished) {
			return builds[i].finished.Before(builds[j].finished)
		}
		return builds[i].Run < builds[j].Run
	})
	o := builds[0].Origin
	return Resolution{Origin: &o}
}

// ClusterSource reads the build runs still on the cluster.
type ClusterSource struct{ Live *live.Client }

func (s ClusterSource) Runs(ctx context.Context, namespace, commit string) ([]Run, error) {
	prs, err := s.Live.PipelineRuns(ctx, namespace)
	if err != nil {
		return nil, err
	}
	var names []string
	byName := map[string]Obj{}
	for _, pr := range prs {
		if IsBuildOf(pr, commit) {
			n := str(mp(pr["metadata"]), "name")
			names = append(names, n)
			byName[n] = pr
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	trs, err := s.Live.TaskRunsOf(ctx, namespace, names)
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(names))
	grouped := map[string][]Obj{}
	for _, tr := range trs {
		n := str(mp(mp(tr["metadata"])["labels"]), live.LabelPipelineRun)
		grouped[n] = append(grouped[n], tr)
	}
	for _, n := range names {
		out = append(out, Run{PipelineRun: byName[n], TaskRuns: grouped[n]})
	}
	return out, nil
}

// ArchiveSource reads Tekton Results (30 days). At most maxCandidates candidate builds are read in full.
type ArchiveSource struct {
	Results *results.Client
	Window  time.Duration // how far back to look; 30 days when zero
}

const maxCandidates = 5

func (s ArchiveSource) Runs(ctx context.Context, namespace, commit string) ([]Run, error) {
	w := s.Window
	if w <= 0 {
		w = 30 * 24 * time.Hour
	}
	rows, err := s.Results.List(ctx, namespace, time.Now().Add(-w), time.Time{}, 500)
	if err != nil {
		return nil, err
	}
	var out []Run
	for _, row := range rows {
		if row.Pipeline == nil || *row.Pipeline != "build" || row.Commit == nil || *row.Commit != commit || row.Status != "succeeded" {
			continue
		}
		if len(out) == maxCandidates {
			return nil, fmt.Errorf("more than %d archived builds of the commit", maxCandidates)
		}
		run, err := s.Results.Get(ctx, namespace, row.Result)
		if err != nil {
			return nil, err
		}
		out = append(out, Run{PipelineRun: run.PipelineRun, TaskRuns: run.TaskRuns})
	}
	return out, nil
}
