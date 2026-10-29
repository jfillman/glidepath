package environments

import (
	"context"
	"fmt"
	"strings"

	"github.com/glidepath/api/internal/apps"

	"gopkg.in/yaml.v3"
)

// PromoteRequest is Tower's PromoteRequest less what the API knows itself (owner, app, target env from the path,
// initiator from the caller). Exactly one source pair: sourceEnv+sourceCluster (a promotion from a deployed
// environment) or sourceImageRepo+sourceImageTag (a first deploy, when the target is the app's first environment).
type PromoteRequest struct {
	Cluster         string `json:"cluster"`
	SourceEnv       string `json:"sourceEnv,omitempty"`
	SourceCluster   string `json:"sourceCluster,omitempty"`
	SourceImageRepo string `json:"sourceImageRepo,omitempty"`
	SourceImageTag  string `json:"sourceImageTag,omitempty"`
}

// PromoteResult is Tower's PromoteResult for a Flight target: mode pipeline (the release run started) or pr (its PR
// is already open, from an earlier promotion of the same commit). Replayed and DryRun are the API's own, absent
// unless true, so Tower's fields compare one for one.
type PromoteResult struct {
	Mode        string         `json:"mode"`
	PipelineRun string         `json:"pipelineRun,omitempty"`
	PRURL       string         `json:"prUrl,omitempty"`
	AlreadyOpen bool           `json:"alreadyOpen,omitempty"`
	Replayed    bool           `json:"replayed,omitempty"`
	DryRun      bool           `json:"dryRun,omitempty"`
	Object      map[string]any `json:"object,omitempty"`
}

// source is Tower's ResolvedSource.
type source struct{ imageRepo, imageTag, gitRevision, gitURL string }

// Validate is Tower's /promote route check: the target cluster, and exactly one of the two source pairs.
func (r PromoteRequest) Validate() error {
	hasEnv := r.SourceCluster != "" && r.SourceEnv != ""
	hasImage := r.SourceImageRepo != "" && r.SourceImageTag != ""
	if r.Cluster == "" || hasEnv == hasImage {
		return Invalid{"missing required field(s): cluster, and exactly one of {sourceCluster+sourceEnv, sourceImageRepo+sourceImageTag}"}
	}
	if !NameRE.MatchString(r.Cluster) || (hasEnv && (!NameRE.MatchString(r.SourceEnv) || !NameRE.MatchString(r.SourceCluster))) {
		return Invalid{"cluster, sourceEnv and sourceCluster must be plain names"}
	}
	if hasImage && (!imageRepoRE.MatchString(r.SourceImageRepo) || !imageTagRE.MatchString(r.SourceImageTag)) {
		return Invalid{"sourceImageRepo/sourceImageTag must be an image repository and tag"}
	}
	return nil
}

// Promote is Tower's promoteEnvironment for a Flight target. The order check, the source and the open-PR dedupe are
// Tower's; a Ground target is refused (Tower's direct commit needs a GitHub write identity). The release run's name
// comes from keyHash, so the same key again replays.
func (c *Client) Promote(ctx context.Context, app *apps.App, targetEnv string, req PromoteRequest, initiator, keyHash string, dryRun bool) (*PromoteResult, error) {
	if !NameRE.MatchString(targetEnv) {
		return nil, Invalid{fmt.Sprintf("invalid environment name '%s'", targetEnv)}
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	gh := c.GitHub(app.Name)
	repos := ReposOf(app)
	// The gitops token is needed only after the plan and the source are read; minting it alongside saves a broker
	// round trip on every promotion that gets that far.
	prefetch(ctx, gh, c.Owner, repos.Gitops)
	plan, err := fetchPlan(ctx, gh, c.Owner, repos)
	if err != nil {
		return nil, err
	}

	// Never trust the click alone: the target must be exactly the stage after the source in the app's own order, or,
	// for a first deploy, the app's first environment.
	orderText := strings.Join(plan.Order, " -> ")
	if orderText == "" {
		orderText = "none found"
	}
	var sourceTier string
	if req.SourceEnv != "" {
		si, ti := plan.index(req.SourceEnv), plan.index(targetEnv)
		if si == -1 || ti != si+1 {
			return nil, Refused{fmt.Sprintf("%s is not the environment immediately after %s in %s's own cicd.yaml release order (%s)",
				targetEnv, req.SourceEnv, app.Name, orderText), "promote one stage at a time, in deploy.environments order"}
		}
		sourceTier = "upper"
		if plan.isLower(req.SourceEnv) {
			sourceTier = "lower"
		}
	} else if len(plan.Order) == 0 || !strings.EqualFold(plan.Order[0], targetEnv) {
		return nil, Refused{fmt.Sprintf("%s is not %s's first environment in its own cicd.yaml release order (%s) - a first deploy can only go there",
			targetEnv, app.Name, orderText), ""}
	}
	if plan.isLower(targetEnv) {
		return nil, Refused{fmt.Sprintf("%s is a Ground environment: glidepath-api promotes to Flight environments only", targetEnv),
			"a Ground promotion is a direct commit to the app repo, which needs a GitHub write identity; promote it from Tower"}
	}
	if sourceTier == "" {
		sourceTier = "upper" // a first deploy takes the target's tier, which is upper here
	}

	src, err := c.resolveSource(ctx, gh, repos, req, sourceTier)
	if err != nil {
		return nil, err
	}

	// The branch open-release-pr uses: a repeated promotion finds the open PR instead of starting a second run.
	if url, err := gh.OpenPR(ctx, c.Owner, repos.Gitops, "release-"+targetEnv+"-"+short(src.gitRevision)); err != nil {
		return nil, err
	} else if url != "" {
		return &PromoteResult{Mode: "pr", PRURL: url, AlreadyOpen: true, DryRun: dryRun}, nil
	}

	s, err := c.startRun(ctx, app, releaseRun{env: targetEnv, cluster: req.Cluster, gitURL: src.gitURL, gitRevision: src.gitRevision,
		imageRef: src.imageRepo + ":" + src.imageTag, initiator: initiator}, keyHash, dryRun)
	if err != nil {
		return nil, err
	}
	return &PromoteResult{Mode: "pipeline", PipelineRun: s.Name, Replayed: s.Replayed, DryRun: dryRun, Object: s.Object}, nil
}

// resolveSource is Tower's: what to promote comes from the file the source environment's tier commits to, never from
// what the cluster or the client says is running. A first deploy's image is the client's, but its tag's sha must be a
// real commit of the app repo.
func (c *Client) resolveSource(ctx context.Context, gh GitHub, repos Repos, req PromoteRequest, tier string) (*source, error) {
	gitURL := fmt.Sprintf("https://github.com/%s/%s", c.Owner, repos.Source)
	if req.SourceEnv == "" {
		sha, err := ExtractShortSha(req.SourceImageTag)
		if err != nil {
			return nil, err
		}
		rev, err := gh.CommitSHA(ctx, c.Owner, repos.Source, sha)
		if err != nil {
			return nil, err
		}
		return &source{req.SourceImageRepo, req.SourceImageTag, rev, gitURL}, nil
	}

	if tier == "lower" {
		repo, tag, path, err := readGroundImage(ctx, gh, c.Owner, repos.Source, req.SourceEnv)
		if err != nil {
			return nil, err
		}
		if repo == "" || tag == "" {
			return nil, Refused{fmt.Sprintf("%s (%s) has no release.image recorded yet - nothing to promote", req.SourceEnv, path), ""}
		}
		sha, err := ExtractShortSha(tag)
		if err != nil {
			return nil, err
		}
		rev, err := gh.CommitSHA(ctx, c.Owner, repos.Source, sha)
		if err != nil {
			return nil, err
		}
		return &source{repo, tag, rev, gitURL}, nil
	}

	// A Flight source: release.image in <cluster>/<env>/release.yaml (the release-file split), falling back to
	// values.yaml and the deprecated rollout.image. The commit is releaseTracking.gitRevision when a file still has
	// one, else the image tag's own sha.
	var imageRepo, imageTag, recorded string
	path := req.SourceCluster + "/" + req.SourceEnv + "/release.yaml"
	for _, candidate := range []string{req.SourceCluster + "/" + req.SourceEnv + "/release.yaml", req.SourceCluster + "/" + req.SourceEnv + "/values.yaml"} {
		content, err := gh.Contents(ctx, c.Owner, repos.Gitops, candidate)
		if err != nil {
			continue // Tower's catch: a missing (or unreadable) file is the next candidate
		}
		doc, err := parse(content, candidate)
		if err != nil {
			return nil, err
		}
		imageRepo, imageTag = orElse(doc, "repository"), orElse(doc, "tag")
		recorded, _ = scalar(getIn(doc, "releaseTracking", "gitRevision"))
		path = candidate
		if imageRepo != "" && imageTag != "" {
			break
		}
	}
	if imageRepo == "" || imageTag == "" {
		return nil, Refused{fmt.Sprintf("%s (%s) has no release.image recorded yet - nothing to promote", req.SourceEnv, path), ""}
	}
	rev := recorded
	if rev == "" {
		sha, err := ExtractShortSha(imageTag)
		if err != nil {
			return nil, err
		}
		if rev, err = gh.CommitSHA(ctx, c.Owner, repos.Source, sha); err != nil {
			return nil, err
		}
	}
	return &source{imageRepo, imageTag, rev, gitURL}, nil
}

// orElse is `doc.getIn(['release','image',k]) ?? doc.getIn(['rollout','image',k])`.
func orElse(doc *yaml.Node, k string) string {
	if v, ok := scalar(getIn(doc, "release", "image", k)); ok {
		return v
	}
	v, _ := scalar(getIn(doc, "rollout", "image", k))
	return v
}

// readGroundImage is Tower's: a Ground environment's deployed image, the machine-owned release file first
// (glidepath/envs/<env>.release.yaml), then the env file's release.image, then its deprecated rollout.image.
func readGroundImage(ctx context.Context, gh GitHub, owner, repo, env string) (imageRepo, imageTag, path string, err error) {
	candidates := []struct {
		path string
		keys [][]string
	}{
		{envsRoot + "/envs/" + env + ".release.yaml", [][]string{{"release", "image"}}},
		{envsRoot + "/envs/" + env + ".yaml", [][]string{{"release", "image"}, {"rollout", "image"}}},
	}
	for _, c := range candidates {
		content, gerr := gh.Contents(ctx, owner, repo, c.path)
		if gerr != nil {
			continue
		}
		doc, perr := parse(content, c.path)
		if perr != nil {
			return "", "", "", perr
		}
		for _, k := range c.keys {
			r, _ := scalar(getIn(doc, append(k, "repository")...))
			t, _ := scalar(getIn(doc, append(k, "tag")...))
			if r != "" && t != "" {
				return r, t, c.path, nil
			}
		}
	}
	return "", "", candidates[0].path, nil
}
