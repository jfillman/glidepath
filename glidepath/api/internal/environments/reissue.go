package environments

// Reissue (docs/admin/api.md#reissue-a-release-pr, docs/admin/release-state.md#reissue): a release PR that was closed
// without merging is replaced by a FRESH one, cut by the app's own `release` Pipeline from the current default branch of
// the gitops repo. Nothing is reopened and nothing is pushed to the closed PR: the review-evidence code treats a release
// PR as a baseline only when it has exactly one commit and was never reopened or force-pushed (internal/refprotection).
//
// The release record decides everything. The image, the commit, the environment and the cluster are the closed release's
// own, read from its release-tracking record on the cluster; the PR's state is read from the git host; the caller names
// only which release (the chain id in the path) and supplies an Idempotency-Key. The reissue is a NEW release (new chain id,
// new record, new PR) whose record links back with reissueOf; the old record ends `closed` with reissuedBy. A reissue is
// refused unless every precondition below holds, each with its own reason.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/github"
	"github.com/glidepath/api/internal/pipelineruns"
	"github.com/glidepath/api/internal/releases"
)

// reissueStates are the record states a reissue makes sense from. `proposed` is the usual one: closing a PR makes no push,
// so nothing moves the record. `closed` is a record open-release-pr already closed because a newer release PR opened for the
// environment; it is only ever reissuable while it is still the newest. Every other state is refused: merged and the ran
// states (the release happened or is happening), superseded and rolled-back (it ran and was replaced).
var reissueStates = map[string]bool{"proposed": true, "closed": true}

// PRReader is the two pull-request reads a reissue makes (internal/github's Session); a GitHub without them cannot reissue.
type PRReader interface {
	PullRequest(ctx context.Context, owner, repo string, number int) (github.PullRequest, error)
	OpenPRsFrom(ctx context.Context, owner, repo string, prefixes ...string) ([]github.PullRequest, error)
}

var fullShaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// notClosed is the refusal for a release whose PR is not closed-and-unmerged: the list of reissuable releases leaves it out.
type notClosed struct{ Refused }

// reissueSource is what a passing check yields: the closed release's own image, commit and place.
type reissueSource struct {
	chain, releaseID, env, cluster, image, gitURL, gitRevision, prURL string
	prNumber                                                          int
}

func chainOf(rec *releases.Record) string { return strings.TrimPrefix(rec.Name, releases.Prefix) }

func normGitURL(u string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(u), "/"), ".git")
}

// reissueCheck is every precondition, in the order that gives the clearest reason first. A Refused is an answer; any other
// error is a failed read.
func (c *Client) reissueCheck(ctx context.Context, app *apps.App, gh GitHub, rec *releases.Record, siblings func() ([]releases.Record, error), planOf func() (*Plan, error)) (*reissueSource, error) {
	d := rec.Data
	chain := chainOf(rec)
	repos := ReposOf(app)
	prs, ok := gh.(PRReader)
	if !ok {
		return nil, Refused{"this glidepath-api cannot read pull requests from the git host", ""}
	}

	// 1. The record's state. Only a release that never ran is reissuable.
	st := d["state"]
	if st == "" {
		st = "proposed" // a record written before states existed was proposed
	}
	if !reissueStates[st] {
		return nil, Refused{fmt.Sprintf("release %s is %s: only a release whose PR was closed without merging (state proposed or closed) can be reissued", chain, st),
			stateHint(st)}
	}

	// 1b. Once per release, from the record itself: the first reissue's run may have been pruned, and the record outlives it.
	if by := d["reissuedBy"]; by != "" {
		return nil, Refused{fmt.Sprintf("release %s was already reissued (by release %s)", chain, by), "follow that release, or promote again from the source environment"}
	}

	// 2. Its release PR, from the git host, never the caller: it must be one of this app's gitops PRs, closed and not merged.
	m := regexp.MustCompile(`^https://github\.com/` + regexp.QuoteMeta(c.Owner) + `/` + regexp.QuoteMeta(repos.Gitops) + `/pull/([0-9]+)$`).FindStringSubmatch(d["prUrl"])
	if m == nil {
		return nil, Refused{fmt.Sprintf("release %s records no release PR in %s/%s (prUrl %q)", chain, c.Owner, repos.Gitops, d["prUrl"]), ""}
	}
	var number int
	fmt.Sscanf(m[1], "%d", &number)
	pr, err := prs.PullRequest(ctx, c.Owner, repos.Gitops, number)
	if err != nil {
		return nil, err
	}
	switch {
	case pr.Merged:
		return nil, notClosed{Refused{fmt.Sprintf("release PR #%d was merged: there is nothing to reissue", number), "a merged release is rolled forward or back from the Deployments tab"}}
	case pr.State != "closed":
		return nil, notClosed{Refused{fmt.Sprintf("release PR #%d is still open: close it first, or merge it", number), ""}}
	}

	// 3. The record's own facts, each checked for shape: a record is written by the cluster's pipeline-runner, whose token a
	// repository's own pipeline code can reach, so nothing in it is trusted to be what the API would have resolved itself.
	env, cluster, image, rev := d["env"], d["cluster"], d["image"], d["gitRevision"]
	if k := d["kind"]; k != "" && k != "promote" {
		return nil, Refused{fmt.Sprintf("release %s is a %s: only a promotion's PR can be reissued", chain, k), "start the rollback again from the Deployments tab"}
	}
	if !uuidRE.MatchString(chain) {
		return nil, Refused{fmt.Sprintf("%q is not a release chain id", chain), ""}
	}
	if !NameRE.MatchString(env) || !NameRE.MatchString(cluster) {
		return nil, Refused{fmt.Sprintf("release %s records no valid environment and cluster", chain), ""}
	}
	if image == "" {
		return nil, Refused{fmt.Sprintf("release %s records no image (it was written before ADR-0021 phase 4)", chain), "promote again from the source environment"}
	}
	i := strings.LastIndex(image, ":")
	if i < 1 || !imageRepoRE.MatchString(image[:i]) || !imageTagRE.MatchString(image[i+1:]) {
		return nil, Refused{fmt.Sprintf("release %s records an image (%q) that is not a repository and tag", chain, image), ""}
	}
	if path := image[:i]; path[strings.LastIndex(path, "/")+1:] != app.Name {
		return nil, Refused{fmt.Sprintf("release %s records image %s, which is not an image of %s", chain, image, app.Name), ""}
	}
	if !fullShaRE.MatchString(rev) {
		return nil, Refused{fmt.Sprintf("release %s records no full commit sha", chain), ""}
	}
	sha, err := ExtractShortSha(image[i+1:])
	if err != nil || !strings.HasPrefix(rev, strings.ToLower(sha)) {
		return nil, Refused{fmt.Sprintf("release %s records image %s and commit %s, and the image's own sha is not that commit", chain, image, rev[:12]), ""}
	}
	if want := fmt.Sprintf("https://github.com/%s/%s", c.Owner, repos.Source); normGitURL(d["gitUrl"]) != want {
		return nil, Refused{fmt.Sprintf("release %s records source %q, not %s", chain, d["gitUrl"], want), ""}
	}
	if pr.HeadRef != "release-"+env+"-"+short(rev) {
		return nil, Refused{fmt.Sprintf("release PR #%d was opened from %q, not the promotion branch release-%s-%s", number, pr.HeadRef, env, short(rev)), ""}
	}

	// 4. The environment must still be one of the app's Flight environments, as its own cicd.yaml says now.
	plan, err := planOf()
	if err != nil {
		return nil, err
	}
	if plan.index(env) == -1 {
		return nil, Refused{fmt.Sprintf("%s is no longer in %s's cicd.yaml release order (%s)", env, app.Name, strings.Join(plan.Order, " -> ")), ""}
	}
	if plan.isLower(env) {
		return nil, Refused{fmt.Sprintf("%s is a Ground environment: glidepath-api promotes to Flight environments only", env), ""}
	}

	// 4b. The image is the one the app's source environment carries now, and its repository (registry and path, exactly) is the
	// one that environment's release file names: the same source promote reads, never the record (which pipeline-runner can write).
	if err := c.reissueSourceCheck(ctx, app, gh, plan, env, cluster, chain, image); err != nil {
		return nil, err
	}

	// 5. Never replay an old release over a newer one: refused when any other release of the app to this environment and
	// cluster was opened after this one, or merged after this one was opened (its commit is then what the environment has).
	created := jsTime(d["prCreatedAt"])
	if created == 0 {
		return nil, Refused{fmt.Sprintf("release %s records no PR creation time, so it cannot be ordered against the environment's other releases", chain), ""}
	}
	others, err := siblings()
	if err != nil {
		return nil, err
	}
	for _, o := range others {
		if o.Name == rec.Name {
			continue
		}
		od := o.Data
		if jsTime(od["prCreatedAt"]) > created {
			return nil, Refused{fmt.Sprintf("a newer release of %s to %s on %s was opened since (%s, %s): reissuing this one would replay it over that one",
				app.Name, env, cluster, od["releaseId"], orDefault(od["state"], "proposed")),
				"reissue the newer release instead, or promote again"}
		}
		if ranStates[od["state"]] || od["state"] == "merged" || od["state"] == "superseded" || od["state"] == "rolled-back" {
			at := od["mergedAt"]
			if at == "" {
				at = od["stateAt"]
			}
			if jsTime(at) > created {
				return nil, Refused{fmt.Sprintf("release %s to %s on %s was merged after this one was opened: reissuing this one would replay it over that one", od["releaseId"], env, cluster),
					"promote again from the source environment"}
			}
		}
	}

	// 6. No other release PR is open for the environment (the branch is per commit, so a newer commit's PR is a different
	// branch). The git host does not say which cluster a PR is for, so the check is per environment name: stricter, never looser.
	open, err := prs.OpenPRsFrom(ctx, c.Owner, repos.Gitops, "release-"+env+"-", "release-rollback-"+env+"-")
	if err != nil {
		return nil, err
	}
	branchRE := regexp.MustCompile(`^release-(rollback-)?` + regexp.QuoteMeta(env) + `-[0-9a-zA-Z]{1,40}$`)
	for _, p := range open {
		if branchRE.MatchString(p.HeadRef) {
			return nil, Refused{fmt.Sprintf("a release PR for %s is already open (%s)", env, p.URL), "merge or close it first"}
		}
	}

	// 7. The commit to release is a real commit of the app repo (the same read promote makes of an image tag's sha).
	if full, err := gh.CommitSHA(ctx, c.Owner, repos.Source, rev); err != nil {
		// The git host saying the commit is not there (404, or 422 for a sha it cannot resolve) is an answer about this release.
		var he github.HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 422) {
			return nil, Refused{fmt.Sprintf("commit %s of release %s does not resolve in %s/%s", rev[:12], chain, c.Owner, repos.Source), ""}
		}
		return nil, err
	} else if full != rev {
		return nil, Refused{fmt.Sprintf("commit %s of release %s does not resolve in %s/%s", rev[:12], chain, c.Owner, repos.Source), ""}
	}

	return &reissueSource{chain: chain, releaseID: d["releaseId"], env: env, cluster: cluster, image: image, gitURL: fmt.Sprintf("https://github.com/%s/%s", c.Owner, repos.Source),
		gitRevision: rev, prURL: d["prUrl"], prNumber: number}, nil
}

// reissueSourceCheck re-reads what promote reads. For an environment with a predecessor in the app's release order, that
// environment's release file must still carry this image's tag (staging rolled back since: refused, naming what it carries now)
// and name exactly this image's repository. The first environment has no source environment: its own gitops release file is the anchor.
func (c *Client) reissueSourceCheck(ctx context.Context, app *apps.App, gh GitHub, plan *Plan, env, cluster, chain, image string) error {
	i := strings.LastIndex(image, ":")
	repo, tag := image[:i], image[i+1:]
	if idx := plan.index(env); idx > 0 {
		srcEnv := plan.Order[idx-1]
		tier, srcCluster := "upper", cluster
		if plan.isLower(srcEnv) {
			tier = "lower"
		} else {
			for _, u := range plan.Upper {
				if strings.EqualFold(u.Name, srcEnv) && u.Cluster != "" {
					srcCluster = u.Cluster
				}
			}
		}
		src, err := c.resolveSource(ctx, gh, ReposOf(app), PromoteRequest{Cluster: cluster, SourceEnv: srcEnv, SourceCluster: srcCluster}, tier)
		if err != nil {
			var he github.HTTPError
			if errors.As(err, &he) && (he.Status == 404 || he.Status == 422) {
				return Refused{fmt.Sprintf("the commit of the image %s carries does not resolve in %s/%s", srcEnv, c.Owner, ReposOf(app).Source), ""}
			}
			return err
		}
		if src.imageTag != tag {
			return Refused{fmt.Sprintf("%s no longer carries release %s's image: it carries %s:%s, not %s", srcEnv, chain, src.imageRepo, src.imageTag, image),
				"promote again from " + srcEnv}
		}
		if src.imageRepo != repo {
			return Refused{fmt.Sprintf("release %s records image repository %s, but %s's release file names %s", chain, repo, srcEnv, src.imageRepo), ""}
		}
		return nil
	}
	// No source environment: the anchor is the environment's own release file in the gitops repository (default branch), which
	// pipeline-runner cannot write (a release PR changes it only through review and merge), never another release record, which it
	// can. No file yet, or one naming another repository, is a refusal.
	repos := ReposOf(app)
	var fileRepo, path string
	for _, cand := range []string{cluster + "/" + env + "/release.yaml", cluster + "/" + env + "/values.yaml"} {
		content, gerr := gh.Contents(ctx, c.Owner, repos.Gitops, cand)
		if gerr != nil {
			continue
		}
		doc, perr := parse(content, cand)
		if perr != nil {
			return perr
		}
		path = cand
		if fileRepo = promotableRepo(doc); fileRepo != "" {
			break
		}
	}
	if fileRepo == "" {
		return Refused{fmt.Sprintf("%s is the app's first environment and has no release file naming an image repository yet (%s/%s/release.yaml in %s), so the image repository of release %s cannot be verified", env, cluster, env, repos.Gitops, chain),
			"promote the first image from Tower, which a person reviews"}
	}
	if fileRepo != repo {
		return Refused{fmt.Sprintf("release %s records image repository %s, but %s names %s", chain, repo, path, fileRepo), ""}
	}
	return nil
}

// oncePlan reads the app's release plan at most once per request (a list checks several candidates against the same plan).
func (c *Client) oncePlan(ctx context.Context, gh GitHub, app *apps.App) func() (*Plan, error) {
	var (
		once sync.Once
		plan *Plan
		err  error
	)
	return func() (*Plan, error) {
		once.Do(func() { plan, err = fetchPlan(ctx, gh, c.Owner, ReposOf(app)) })
		return plan, err
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func stateHint(st string) string {
	switch st {
	case "merged", "progressing", "healthy", "sync-failed", "aborted", "degraded":
		return "the release's PR merged; follow it on the Deployments tab"
	case "superseded", "rolled-back":
		return "this release ran and was replaced; promote again from the source environment"
	}
	return ""
}

// ReissueResult answers a reissue: the release run that will open the fresh PR, and which closed release it replaces.
type ReissueResult struct {
	Mode        string `json:"mode"` // pipeline
	PipelineRun string `json:"pipelineRun"`
	Reissues    string `json:"reissues"` // the chain id of the closed release
	ClosedPR    string `json:"closedPrUrl"`
	Env         string `json:"env"`
	Cluster     string `json:"cluster"`
	Image       string `json:"image"`
	// Linked: the run carries the reissue-of annotation, so the new record names the one it replaces. False only when the
	// cluster's admission policy for glidepath-api runs does not yet allow it (the reissue itself is unaffected).
	Linked        bool           `json:"linked"`
	Replayed      bool           `json:"replayed,omitempty"`
	DryRun        bool           `json:"dryRun,omitempty"`
	Object        map[string]any `json:"object,omitempty"`
	Origin        *Origin        `json:"origin,omitempty"`
	OriginUnknown string         `json:"originUnknown,omitempty"`
}

// Reissue starts the app's `promote-release` run for the closed release's own image and commit. The run name derives from the
// closed release's chain id, so a release is reissued at most once (a second call with another Idempotency-Key is refused,
// naming the first run, which can be re-run if it failed); the same key replays.
func (c *Client) Reissue(ctx context.Context, app *apps.App, chain, initiator, key string, dryRun bool) (*ReissueResult, error) {
	gh := c.GitHub(app.Name)
	prefetch(ctx, gh, c.Owner, ReposOf(app).Gitops)
	rec, err := c.Records.Get(ctx, app.Namespace, app.Name, chain)
	if err != nil {
		return nil, err
	}
	src, err := c.reissueCheck(ctx, app, gh, rec, func() ([]releases.Record, error) {
		return c.Records.ListEnv(ctx, app.Namespace, app.Name, rec.Data["env"], rec.Data["cluster"])
	}, c.oncePlan(ctx, gh, app))
	if err != nil {
		var nc notClosed
		if errors.As(err, &nc) {
			return nil, nc.Refused
		}
		return nil, err
	}
	s, err := c.startRun(ctx, app, releaseRun{env: src.env, cluster: src.cluster, gitURL: src.gitURL, gitRevision: src.gitRevision, imageRef: src.image,
		initiator: initiator, reissueOf: src.chain, nameKey: KeyHash(app.Name, "reissue", src.env, src.cluster, src.chain)},
		KeyHash(app.Name, "reissue/"+src.chain, src.env, src.cluster, key), dryRun)
	if err != nil {
		var conf pipelineruns.Conflict
		if errors.As(err, &conf) && strings.Contains(conf.What, "exists and is not this") {
			name := RunName("promote", src.env, src.gitRevision, KeyHash(app.Name, "reissue", src.env, src.cluster, src.chain))
			return nil, Refused{fmt.Sprintf("release %s was already reissued (run %s)", src.chain, name),
				"follow it with GET /v1/apps/" + app.Name + "/releases/runs/" + name + "; if it failed, re-run it"}
		}
		return nil, err
	}
	return &ReissueResult{Mode: "pipeline", PipelineRun: s.Name, Reissues: src.chain, ClosedPR: src.prURL, Env: src.env, Cluster: src.cluster, Image: src.image,
		Linked: s.ReissueLinked, Replayed: s.Replayed, DryRun: dryRun, Object: s.Object, Origin: s.Origin, OriginUnknown: unknownText(s.OriginUnknown)}, nil
}

// Reissuable is one release whose PR was closed without merging and which is still the newest for its environment and
// cluster: the candidates a person may reissue. Eligible says whether the API would accept the reissue now, Reason why not.
type Reissuable struct {
	ChainID   string `json:"chainId"`
	ReleaseID string `json:"releaseId"`
	Env       string `json:"env"`
	Cluster   string `json:"cluster"`
	Image     string `json:"image"`
	PRURL     string `json:"prUrl"`
	State     string `json:"state"`
	Eligible  bool   `json:"eligible"`
	Reason    string `json:"reason,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

// Reissuable lists them: for each environment and cluster, the newest release record; when it is proposed or closed, its PR
// is read from the git host and it is listed when that PR is closed without merging.
func (c *Client) Reissuable(ctx context.Context, app *apps.App) ([]Reissuable, error) {
	all, err := c.Records.List(ctx, app.Namespace, app.Name)
	if err != nil {
		return nil, err
	}
	type key struct{ env, cluster string }
	newest := map[key]releases.Record{}
	group := map[key][]releases.Record{}
	for _, r := range all {
		k := key{r.Data["env"], r.Data["cluster"]}
		group[k] = append(group[k], r)
		if cur, ok := newest[k]; !ok || jsTime(r.Data["prCreatedAt"]) > jsTime(cur.Data["prCreatedAt"]) {
			newest[k] = r
		}
	}
	keys := make([]key, 0, len(newest))
	for k := range newest {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].env+"/"+keys[i].cluster < keys[j].env+"/"+keys[j].cluster })
	gh := c.GitHub(app.Name)
	prefetch(ctx, gh, c.Owner, ReposOf(app).Gitops)
	planOf := c.oncePlan(ctx, gh, app)
	out := []Reissuable{}
	for _, k := range keys {
		rec := newest[k]
		st := orDefault(rec.Data["state"], "proposed")
		if !reissueStates[st] || rec.Data["prUrl"] == "" {
			continue
		}
		siblings := group[k]
		_, err := c.reissueCheck(ctx, app, gh, &rec, func() ([]releases.Record, error) { return siblings, nil }, planOf)
		var nc notClosed
		if errors.As(err, &nc) {
			continue // its PR is open (or merged): not a candidate
		}
		e := Reissuable{ChainID: chainOf(&rec), ReleaseID: rec.Data["releaseId"], Env: k.env, Cluster: k.cluster, Image: rec.Data["image"], PRURL: rec.Data["prUrl"], State: st, Eligible: err == nil}
		var ref Refused
		switch {
		case err == nil:
		case errors.As(err, &ref):
			e.Reason, e.Hint = ref.What, ref.Hint
		default:
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
