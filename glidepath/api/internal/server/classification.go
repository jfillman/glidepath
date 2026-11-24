package server

// POST and GET /v1/apps/{app}/pulls/{n}/classification?head=<sha> (internal/changekind, docs/admin/api.md#change-classification):
// the change kind of gitops pull request {n} at commit head (image-release, rollback, config or no-op), worked out from the pull
// request's own diff and stored write-once per head, so every release gate and every later reader (promote, detect-bypass-merge,
// release-log-emit, Tower) reads one answer. POST computes it on the first ask and stores it (change-kind-pr-<n> in the app's
// namespace, and changeKind.<sha12> on the pull request's release record); GET only reads what is stored. Tier T0: it changes
// nothing in git or in what runs (spec section 5.3); `dryRun` computes without storing.

import (
	"context"
	"net/http"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/changekind"
)

func (s *Server) classifyPullRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	if s.ChangeKind == nil {
		return &auth.Error{Status: http.StatusServiceUnavailable, Rule: "GP-API-503", Msg: "change classification is not configured on this glidepath-api"}
	}
	dry, err := dryRun(r)
	if err != nil {
		return err
	}
	n, err := changekind.ParseNumber(r.PathValue("n"))
	if err != nil {
		return err
	}
	a, err := s.ChangeKind.Classify(ctx, app, n, r.URL.Query().Get("head"), dry)
	if err != nil {
		return err
	}
	note(w, "pr", n, "head", a.Head, "kind", a.Kind, "outcome", a.Outcome, "failClosed", a.FailClosed, "record", a.Record, "recordCopy", a.RecordCopy, "dryRun", dry)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a)
	return nil
}

func (s *Server) getPullRequestClassification(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	if s.ChangeKind == nil {
		return &auth.Error{Status: http.StatusServiceUnavailable, Rule: "GP-API-503", Msg: "change classification is not configured on this glidepath-api"}
	}
	n, err := changekind.ParseNumber(r.PathValue("n"))
	if err != nil {
		return err
	}
	a, err := s.ChangeKind.Get(ctx, app, n, r.URL.Query().Get("head"))
	if err != nil {
		return err
	}
	note(w, "pr", n, "head", a.Head, "kind", a.Kind)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a)
	return nil
}
