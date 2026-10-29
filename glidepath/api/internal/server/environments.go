package server

// Phase 4 (tier T2, propose): promote and roll back, and the release plan and rollback plan they rest on
// (internal/environments, docs/admin/api.md#promote-and-roll-back). Both writes only start the app's own release
// Pipeline; its open-release-pr opens the gitops PR with the platform's GitHub App and a human merges it.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/environments"
	"github.com/glidepath/api/internal/pipelineruns"
)

// writeTowerJSON writes v as Tower's res.json would: no HTML escaping (JSON.stringify leaves <, > and & alone), so a
// shadow comparison of the bytes holds.
func writeTowerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// readBody decodes a write's JSON body: one object, at most 64 KiB.
func readBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return environments.Invalid{What: "a JSON body is required"}
		}
		return environments.Invalid{What: "the body is not the JSON object this call takes: " + err.Error()}
	}
	return nil
}

// initiator is who the release run records as having asked: the person when a delegate (Tower) calls, which the
// wrapper has already required, else the caller itself.
func initiator(c *auth.Caller) string {
	if c.Delegate && c.OnBehalfOf != "" {
		return c.OnBehalfOf
	}
	return c.User
}

// listEnvironments is Tower's fetchReleasePlan: the app's environments, their tiers and their promotion order.
func (s *Server) listEnvironments(ctx context.Context, w http.ResponseWriter, _ *http.Request, _ *auth.Caller, app *apps.App) error {
	p, err := s.Envs.Plan(ctx, app)
	if err != nil {
		return err
	}
	writeTowerJSON(w, http.StatusOK, p)
	return nil
}

// getRollbackPlan is Tower's GET /release/rollback-plan: the environment's current release and its eligible targets.
func (s *Server) getRollbackPlan(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	cluster := r.URL.Query().Get("cluster")
	if cluster == "" {
		return environments.Invalid{What: `missing required "cluster" query param`}
	}
	p, err := s.Envs.RollbackPlan(ctx, app, r.PathValue("env"), cluster)
	if err != nil {
		return err
	}
	writeTowerJSON(w, http.StatusOK, p)
	return nil
}

// writeStart reads what both T2 writes need first: dry run and the Idempotency-Key, and audits the key's hash.
func writeStart(w http.ResponseWriter, r *http.Request, app *apps.App, kind, cluster string) (dry bool, keyHash string, err error) {
	if dry, err = dryRun(r); err != nil {
		return false, "", err
	}
	key := r.Header.Get("Idempotency-Key")
	if err = pipelineruns.ValidKey(key); err != nil {
		return false, "", err
	}
	keyHash = environments.KeyHash(app.Name, kind, r.PathValue("env"), cluster, key)
	note(w, "env", r.PathValue("env"), "cluster", cluster, "idempotencyKeySha256", keyHash[:12])
	return dry, keyHash, nil
}

// promoteEnvironment starts the release run that promotes to a Flight environment, once per Idempotency-Key: 201 when
// this call created it; 200 when it is a replay, a dry run, or the release PR was already open.
func (s *Server) promoteEnvironment(ctx context.Context, w http.ResponseWriter, r *http.Request, c *auth.Caller, app *apps.App) error {
	var body environments.PromoteRequest
	if err := readBody(r, &body); err != nil {
		return err
	}
	dry, keyHash, err := writeStart(w, r, app, "promote", body.Cluster)
	if err != nil {
		return err
	}
	out, err := s.Envs.Promote(ctx, app, r.PathValue("env"), body, initiator(c), keyHash, dry)
	if err != nil {
		return err
	}
	note(w, "mode", out.Mode, "created", out.PipelineRun, "replayed", out.Replayed, "prUrl", out.PRURL)
	status := http.StatusOK
	if out.Mode == "pipeline" && !out.Replayed && !dry {
		status = http.StatusCreated
	}
	writeTowerJSON(w, status, out)
	return nil
}

// rollbackEnvironment starts the rollback release run to one of the plan's targets, once per Idempotency-Key.
func (s *Server) rollbackEnvironment(ctx context.Context, w http.ResponseWriter, r *http.Request, c *auth.Caller, app *apps.App) error {
	var body environments.RollbackRequest
	if err := readBody(r, &body); err != nil {
		return err
	}
	dry, keyHash, err := writeStart(w, r, app, "rollback", body.Cluster)
	if err != nil {
		return err
	}
	out, err := s.Envs.Rollback(ctx, app, r.PathValue("env"), body, initiator(c), keyHash, dry)
	if err != nil {
		return err
	}
	replaces := ""
	if out.Replaces != nil {
		replaces = *out.Replaces
	}
	note(w, "target", out.Target.ReleaseID, "replaces", replaces, "created", out.PipelineRun, "replayed", out.Replayed, "prUrl", out.PRURL)
	status := http.StatusOK
	if out.PipelineRun != "" && !out.Replayed && !dry {
		status = http.StatusCreated
	}
	writeTowerJSON(w, status, out)
	return nil
}
