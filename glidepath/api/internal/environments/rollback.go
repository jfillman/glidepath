package environments

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/releases"
)

// Tower's glidepathRollback.ts constants: the eligible window, and the states a release that ran is in.
const eligibleWindow = 5

var ranStates = map[string]bool{"progressing": true, "healthy": true, "aborted": true, "degraded": true, "sync-failed": true}

// Current is the release running in the environment now. A field the record lacks is absent, as in Tower's JSON.
type Current struct {
	ReleaseID *string `json:"releaseId,omitempty"`
	Image     *string `json:"image,omitempty"`
	State     string  `json:"state"`
	StateAt   *string `json:"stateAt,omitempty"`
}

// Target is one release a rollback may go back to (Tower's RollbackTarget).
type Target struct {
	ReleaseID   string  `json:"releaseId"`
	Image       string  `json:"image"`
	GitRevision string  `json:"gitRevision"`
	GitURL      string  `json:"gitUrl"`
	HealthyAt   string  `json:"healthyAt"`
	State       *string `json:"state,omitempty"`
}

// RollbackPlan is Tower's RollbackPlan: current is absent when nothing has run there; targets newest first, [] when
// no earlier image qualifies.
type RollbackPlan struct {
	Current *Current `json:"current,omitempty"`
	Targets []Target `json:"targets"`
}

// jsTime is Tower's time(): Date.parse in milliseconds, 0 for empty or unparsable.
func jsTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func field(d map[string]string, k string) *string {
	if v, ok := d[k]; ok {
		return &v
	}
	return nil
}

// PlanRollback is Tower's planRollback, ported line for line: the newest release that ran (by prCreatedAt) is
// current; the targets are the newest five that reached healthy (by healthyAt), less any written before phase 4, the
// current release, its image, and repeats of an image. Both sorts are stable, like V8's, so ties keep list order.
func PlanRollback(records []releases.Record) RollbackPlan {
	var ran []releases.Record
	for _, r := range records {
		if ranStates[r.Data["state"]] {
			ran = append(ran, r)
		}
	}
	sort.SliceStable(ran, func(i, j int) bool { return jsTime(ran[i].Data["prCreatedAt"]) > jsTime(ran[j].Data["prCreatedAt"]) })
	var current *Current
	if len(ran) > 0 {
		d := ran[0].Data
		current = &Current{ReleaseID: field(d, "releaseId"), Image: field(d, "image"), State: d["state"], StateAt: field(d, "stateAt")}
	}

	var window []releases.Record
	for _, r := range records {
		if r.Data["healthyAt"] != "" {
			window = append(window, r)
		}
	}
	sort.SliceStable(window, func(i, j int) bool { return jsTime(window[i].Data["healthyAt"]) > jsTime(window[j].Data["healthyAt"]) })
	if len(window) > eligibleWindow {
		window = window[:eligibleWindow]
	}
	seen := map[string]bool{}
	targets := []Target{}
	for _, r := range window {
		d := r.Data
		if d["image"] == "" || d["gitRevision"] == "" || d["gitUrl"] == "" || d["releaseId"] == "" {
			continue // written before phase 4
		}
		if current != nil && ((current.ReleaseID != nil && d["releaseId"] == *current.ReleaseID) ||
			(current.Image != nil && d["image"] == *current.Image)) || seen[d["image"]] {
			continue
		}
		seen[d["image"]] = true
		targets = append(targets, Target{ReleaseID: d["releaseId"], Image: d["image"], GitRevision: d["gitRevision"],
			GitURL: d["gitUrl"], HealthyAt: d["healthyAt"], State: field(d, "state")})
	}
	return RollbackPlan{Current: current, Targets: targets}
}

// RollbackRequest is Tower's /release/rollback body less what the API knows itself.
type RollbackRequest struct {
	Cluster         string `json:"cluster"`
	TargetReleaseID string `json:"targetReleaseId"`
	Reason          string `json:"reason"`
}

// RollbackResult is Tower's RollbackResult without the PR wait: Tower held the request up to 90 s for the run's
// open-release-pr to report its PR; the API answers as soon as the run exists. alreadyOpen is false and pipelineRun
// set when a run started; prUrl and alreadyOpen true when the rollback's PR was already open. Replayed and DryRun
// are the API's own, absent unless true.
type RollbackResult struct {
	PRURL       string         `json:"prUrl,omitempty"`
	AlreadyOpen bool           `json:"alreadyOpen"`
	PipelineRun string         `json:"pipelineRun,omitempty"`
	Target      Target         `json:"target"`
	Replaces    *string        `json:"replaces,omitempty"`
	Replayed    bool           `json:"replayed,omitempty"`
	DryRun      bool           `json:"dryRun,omitempty"`
	Object      map[string]any `json:"object,omitempty"`
}

// Rollback is Tower's startRollback: the target must be one of the environment's eligible targets, computed here
// from the release records, never taken from the request; a rollback whose PR is already open is answered with it.
func (c *Client) Rollback(ctx context.Context, app *apps.App, env string, req RollbackRequest, initiator, keyHash string, dryRun bool) (*RollbackResult, error) {
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Cluster == "" || req.TargetReleaseID == "" || req.Reason == "" {
		return nil, Invalid{"missing required field(s): cluster, targetReleaseId, reason"}
	}
	if len(req.Reason) > 1024 || len(req.TargetReleaseID) > 256 {
		return nil, Invalid{"reason is at most 1024 characters, targetReleaseId 256"}
	}
	plan, err := c.RollbackPlan(ctx, app, env, req.Cluster)
	if err != nil {
		return nil, err
	}
	if plan.Current == nil {
		return nil, Refused{fmt.Sprintf("nothing has been released to %s on %s yet: there is nothing to roll back", env, req.Cluster), ""}
	}
	var target *Target
	for i := range plan.Targets {
		if plan.Targets[i].ReleaseID == req.TargetReleaseID {
			target = &plan.Targets[i]
			break
		}
	}
	if target == nil {
		return nil, Refused{fmt.Sprintf("%s is not an eligible rollback target for %s (one of the last %d healthy releases, with a different image from the current one)",
			req.TargetReleaseID, env, eligibleWindow), "GET /v1/apps/{app}/environments/" + env + "/rollback-plan lists the eligible targets"}
	}
	replaces := plan.Current.ReleaseID

	// The branch open-release-pr uses for a rollback: a repeated rollback finds the open PR instead of a second run.
	url, err := c.GitHub(app.Name).OpenPR(ctx, c.Owner, ReposOf(app).Gitops, "release-rollback-"+env+"-"+short(target.GitRevision))
	if err != nil {
		return nil, err
	}
	if url != "" {
		return &RollbackResult{PRURL: url, AlreadyOpen: true, Target: *target, Replaces: replaces, DryRun: dryRun}, nil
	}
	of := ""
	if replaces != nil {
		of = *replaces
	}
	s, err := c.startRun(ctx, app, releaseRun{env: env, cluster: req.Cluster, gitURL: target.GitURL, gitRevision: target.GitRevision,
		imageRef: target.Image, initiator: initiator,
		// cicd.yaml at main: the target's own commit may carry keys the platform has since removed.
		rollback: &rollbackParams{of: of, reason: req.Reason, configRevision: "main"}}, keyHash, dryRun)
	if err != nil {
		return nil, err
	}
	return &RollbackResult{PipelineRun: s.Name, Target: *target, Replaces: replaces, Replayed: s.Replayed, DryRun: dryRun, Object: s.Object}, nil
}
