package server

// GET /v1/apps/{app}/ref-protection?repo=<owner>/<name>&ref=<full ref>&sha=<commit> (internal/refprotection,
// docs/admin/api.md#ref-protection): whether every commit being released arrived on the protected branch through a pull
// request someone other than its author approved (review evidence), with that evidence: the commits, their pull requests,
// the openers, mergers and approvers. Tier T0: it writes nothing. The privileged Pipelines call it as their first task
// (catalog verify-ref-protection) with the repo, ref and sha Pipelines-as-Code recorded on the run, and refuse to run
// unless the verdict is "protected". The Clearance gate's proposer reads the same evidence.

import (
	"context"
	"net/http"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
)

func (s *Server) getRefProtection(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	if s.RefProtection == nil {
		return &auth.Error{Status: http.StatusServiceUnavailable, Rule: "GP-API-503", Msg: "ref protection is not configured on this glidepath-api"}
	}
	q := r.URL.Query()
	v, err := s.RefProtection.Verify(ctx, app, q.Get("repo"), q.Get("ref"), q.Get("sha"))
	if err != nil {
		return err
	}
	commits, calls := 0, 0
	if v.Evidence != nil {
		commits, calls = len(v.Evidence.Commits), v.Evidence.Calls
	}
	note(w, "verdict", v.Verdict, "repo", v.Repo, "ref", v.Ref, "sha", v.Sha, "commits", commits, "unreviewed", len(v.Missing), "githubReads", calls)
	w.Header().Set("Cache-Control", "no-store")
	writeTowerJSON(w, http.StatusOK, v)
	return nil
}
