package server

// POST /v1/apps/{app}/pipelineruns/{name}/gate-verdict (internal/gateverdict, docs/admin/api.md#gate-verdicts): records the
// verdict of the gate run {name} for its commit in the release record's gate ledger, and answers with this commit's ledger.
// Called by the promotion-ready step of comment-pr-check-result (a finally task) with a projected token for audience
// glidepath-gate-verdict; the body is not read. Tier T1: it writes, as glidepath-api's own account.

import (
	"context"
	"io"
	"net/http"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
)

func (s *Server) recordGateVerdict(ctx context.Context, w http.ResponseWriter, r *http.Request, c *auth.Caller, app *apps.App) error {
	if s.GateVerdict == nil {
		return &auth.Error{Status: http.StatusServiceUnavailable, Rule: "GP-API-503", Msg: "gate verdicts are not configured on this glidepath-api"}
	}
	dry, err := dryRun(r)
	if err != nil {
		return err
	}
	// The body is a trigger only: drained (bounded) and never read for anything.
	io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
	a, err := s.GateVerdict.Record(ctx, c, app.Namespace, r.PathValue("name"), dry)
	if err != nil {
		return err
	}
	note(w, "outcome", a.Outcome, "gate", a.Gate, "key", a.Key, "verdict", a.Verdict, "record", a.Record, "pod", c.PodName)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a)
	return nil
}
