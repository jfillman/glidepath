package server

// POST /v1/apps/{app}/cicd/validations (internal/cicd, docs/admin/api.md#validate-a-cicdyaml-change): what a change
// to the app's cicd.yaml would mean, as the cicd-validate PR check will judge it. Tier T0: it writes nothing, so a
// delegate needs no on-behalf-of, though Clearance sends one and the audit line keeps it.

import (
	"context"
	"net/http"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/cicd"
)

func (s *Server) validateCicd(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	var body cicd.Request
	if err := readBody(r, &body); err != nil {
		return err
	}
	out, err := s.Cicd.Validate(ctx, app, body)
	if err != nil {
		return err
	}
	errs := 0
	for _, p := range out.Problems {
		if p.Severity == "error" {
			errs++
		}
	}
	sha := out.Base.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	note(w, "valid", out.Valid, "errors", errs, "changes", len(out.Changes), "agent", body.Agent, "base", sha)
	writeTowerJSON(w, http.StatusOK, out)
	return nil
}
