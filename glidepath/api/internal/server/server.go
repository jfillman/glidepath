// Package server is glidepath-api's HTTP surface: api/openapi.yaml, served.
//
// Every /v1 call is authenticated (auth.Authenticate), resolved to the app's -cicd namespace, and
// authorized there (auth.Authorize) before any data is read or written. Every call writes one audit
// line to stdout: who, on whose behalf, which operation and tier, the decision and the status; a
// write adds what it wrote.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/evidence"
	"github.com/glidepath/api/internal/explain"
	"github.com/glidepath/api/internal/pipelineruns"
	"github.com/glidepath/api/internal/releases"
	"github.com/glidepath/api/internal/results"
	"github.com/glidepath/api/internal/testkube"
)

// Server holds the API's dependencies.
type Server struct {
	Auth     *auth.Auth
	Apps     *apps.Resolver
	Results  *results.Client
	Releases releases.Reader
	Evidence *evidence.Reader
	Tests    *testkube.Client
	Runs     *pipelineruns.Client
	Contract []byte
	Known    []explain.KnownFailure // the contract's knownFailures, compiled (explain.Load)
	Version  int                    // the contract's contractVersion
	Log      *slog.Logger
	Now      func() time.Time
}

// op describes one operation, for authorization and the audit line.
type op struct {
	id          string
	tier        string
	resource    string // "" = any authenticated caller
	subresource string
	verb        string
}

type handler func(ctx context.Context, w http.ResponseWriter, r *http.Request, c *auth.Caller, app *apps.App) error

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /v1/whoami", s.wrap(op{"whoami", "T0", "", "", ""}, s.whoami))
	mux.Handle("GET /v1/contract", s.wrap(op{"getContract", "T0", "", "", ""}, s.contract))
	mux.Handle("GET /v1/apps/{app}/runs", s.wrap(op{"listRuns", "T0", "runs", "", "list"}, s.listRuns))
	mux.Handle("GET /v1/apps/{app}/runs/{result}", s.wrap(op{"getRun", "T0", "runs", "", "get"}, s.getRun))
	mux.Handle("GET /v1/apps/{app}/runs/{result}/logs/{taskRun}", s.wrap(op{"getRunLog", "T0", "runs", "logs", "get"}, s.getLog))
	mux.Handle("GET /v1/apps/{app}/runs/{result}/explain", s.wrap(op{"explainRun", "T0", "runs", "logs", "get"}, s.explainRun))
	mux.Handle("GET /v1/apps/{app}/releases", s.wrap(op{"listReleases", "T0", "releases", "", "list"}, s.listReleases))
	mux.Handle("GET /v1/apps/{app}/releases/{chainId}", s.wrap(op{"getRelease", "T0", "releases", "", "get"}, s.getRelease))
	mux.Handle("GET /v1/apps/{app}/evidence", s.wrap(op{"getEvidence", "T0", "evidence", "", "get"}, s.getEvidence))
	mux.Handle("GET /v1/apps/{app}/tests", s.wrap(op{"listTests", "T0", "tests", "", "list"}, s.listTests))
	mux.Handle("GET /v1/apps/{app}/tests/{id}/report", s.wrap(op{"getTestReport", "T0", "tests", "", "get"}, s.getTestReport))
	mux.Handle("POST /v1/apps/{app}/pipelineruns/{name}/rerun", s.wrap(op{"rerunRun", "T1", "runs", "", "rerun"}, s.rerunRun))
	mux.Handle("POST /v1/apps/{app}/pipelineruns/{name}/cancel", s.wrap(op{"cancelRun", "T1", "runs", "", "cancel"}, s.cancelRun))
	return mux
}

type apiError struct {
	Error string `json:"error"`
	Rule  string `json:"rule,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// classify maps an error to a status and body. Unknown errors are 502: the API's own sources
// (the API server, Tekton Results, GHCR, Rekor, Testkube) failed, which is what Tower's routes
// answer too.
func classify(err error) (int, apiError) {
	if e, ok := auth.AsError(err); ok {
		return e.Status, apiError{e.Msg, e.Rule, e.Hint}
	}
	var (
		appNF  apps.NotFound
		appInv apps.Invalid
		resNF  results.NotFound
		noLog  results.NoLog
		resInv results.Invalid
		relNF  releases.NotFound
		relInv releases.Invalid
		evInv  evidence.Invalid
		tkNF   testkube.NotFound
		tkInv  testkube.Invalid
		prNF   pipelineruns.NotFound
		prInv  pipelineruns.Invalid
		prConf pipelineruns.Conflict
		prRef  pipelineruns.Refused
	)
	switch {
	case errors.As(err, &noLog):
		return http.StatusNotFound, apiError{err.Error(), "GP-LOG-404", "the run exists; Results stored no log for this TaskRun (docs/admin/tekton-results.md)"}
	case errors.As(err, &appNF):
		return http.StatusNotFound, apiError{err.Error(), "GP-API-404", ""}
	case errors.As(err, &resNF), errors.As(err, &relNF), errors.As(err, &tkNF), errors.As(err, &prNF):
		return http.StatusNotFound, apiError{err.Error(), "GP-API-404", ""}
	case errors.As(err, &appInv), errors.As(err, &resInv), errors.As(err, &relInv), errors.As(err, &evInv), errors.As(err, &tkInv), errors.As(err, &prInv):
		return http.StatusBadRequest, apiError{err.Error(), "GP-API-400", ""}
	case errors.As(err, &prConf):
		return http.StatusConflict, apiError{err.Error(), "GP-API-409", "send a new Idempotency-Key"}
	case errors.As(err, &prRef):
		return http.StatusUnprocessableEntity, apiError{err.Error(), "GP-API-422",
			"an admission policy (tower-pipelinerun-shape) or glidepath-api's own RBAC refused it; retrying won't help"}
	}
	return http.StatusBadGateway, apiError{err.Error(), "GP-API-502", "a source Glidepath reads (the API server, Tekton Results, GHCR, Rekor or Testkube) failed; retry"}
}

type recorder struct {
	http.ResponseWriter
	status int
	audit  []any // what a write adds to its audit line
}

// note adds key/value pairs to the call's audit line.
func note(w http.ResponseWriter, kv ...any) {
	if r, ok := w.(*recorder); ok {
		r.audit = append(r.audit, kv...)
	}
}

func (r *recorder) WriteHeader(s int) { r.status = s; r.ResponseWriter.WriteHeader(s) }

func (s *Server) wrap(o op, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		write := o.tier != "T0"
		var caller *auth.Caller
		decision := "allow"
		appName := r.PathValue("app")
		err := func() error {
			c, err := s.Auth.Authenticate(r)
			if err != nil {
				return err
			}
			caller = c
			// The delegate is trusted to have checked the person (Tower's owner permission), so a write
			// it makes must say who that was.
			if write && c.Delegate && c.OnBehalfOf == "" {
				return &auth.Error{Status: http.StatusBadRequest, Rule: "GP-AUTH-004",
					Msg: "a delegate's write must name the person", Hint: "send " + auth.HeaderOnBehalf}
			}
			var app *apps.App
			if appName != "" {
				if app, err = s.Apps.Resolve(r.Context(), appName); err != nil {
					// Say "no such app" only to a caller who could read it if it existed.
					var nf apps.NotFound
					if errors.As(err, &nf) && o.resource != "" {
						if aerr := s.Auth.Authorize(r.Context(), c, o.verb, o.resource, o.subresource, "app-"+appName+"-cicd", ""); aerr != nil {
							return aerr
						}
					}
					return err
				}
			}
			if o.resource != "" {
				if err := s.Auth.Authorize(r.Context(), c, o.verb, o.resource, o.subresource, app.Namespace, ""); err != nil {
					return err
				}
			}
			return h(r.Context(), rec, r, c, app)
		}()
		if err != nil {
			status, body := classify(err)
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				decision = "deny"
			}
			writeJSON(rec, status, body)
		}
		attrs := []any{"op", o.id, "tier", o.tier, "app", appName, "decision", decision, "status", rec.status,
			"ms", time.Since(start).Milliseconds()}
		if caller != nil {
			attrs = append(attrs, "caller", caller.User, "onBehalfOf", caller.OnBehalfOf)
		}
		if write {
			dry, _ := dryRun(r)
			attrs = append(attrs, "name", r.PathValue("name"), "dryRun", dry)
			attrs = append(attrs, rec.audit...)
		}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		s.Log.Info("audit", attrs...)
	})
}

// window reads sinceHours (1..744, default 24) and limit (1..1000, default 100): the same bounds
// Tower's pipeline-history routes use, so a shadowed call asks the same question.
func (s *Server) window(r *http.Request) (time.Time, int) {
	clamp := func(v, lo, hi, def int) int {
		if v == 0 {
			return def
		}
		return max(lo, min(hi, v))
	}
	h, _ := strconv.Atoi(r.URL.Query().Get("sinceHours"))
	l, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return s.Now().Add(-time.Duration(clamp(h, 1, 24*31, 24)) * time.Hour), clamp(l, 1, 1000, 100)
}

func (s *Server) whoami(_ context.Context, w http.ResponseWriter, _ *http.Request, c *auth.Caller, _ *apps.App) error {
	writeJSON(w, http.StatusOK, c)
	return nil
}

func (s *Server) contract(_ context.Context, w http.ResponseWriter, _ *http.Request, _ *auth.Caller, _ *apps.App) error {
	w.Header().Set("Content-Type", "application/json")
	w.Write(s.Contract)
	return nil
}

func (s *Server) listRuns(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	since, limit := s.window(r)
	if r.URL.Query().Get("view") == "full" {
		runs, err := s.Results.Runs(ctx, app.Namespace, since, app.Created, limit)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
		return nil
	}
	rows, err := s.Results.List(ctx, app.Namespace, since, app.Created, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": rows})
	return nil
}

func (s *Server) getRun(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	run, err := s.Results.Get(ctx, app.Namespace, r.PathValue("result"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, run)
	return nil
}

func (s *Server) getLog(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	text, err := s.Results.Log(ctx, app.Namespace, r.PathValue("result"), r.PathValue("taskRun"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(text))
	return nil
}

// explainRun reads the run and its failed TaskRuns' logs, so it needs what getRunLog needs.
func (s *Server) explainRun(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	result := r.PathValue("result")
	run, err := s.Results.Get(ctx, app.Namespace, result)
	if err != nil {
		return err
	}
	ex, err := explain.Run(run, s.Known, s.Version, func(taskRun string) (string, error) {
		return s.Results.Log(ctx, app.Namespace, result, taskRun)
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ex)
	return nil
}

func (s *Server) listReleases(ctx context.Context, w http.ResponseWriter, _ *http.Request, _ *auth.Caller, app *apps.App) error {
	recs, err := s.Releases.List(ctx, app.Namespace, app.Name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": recs})
	return nil
}

func (s *Server) getRelease(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	rec, err := s.Releases.Get(ctx, app.Namespace, app.Name, r.PathValue("chainId"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rec)
	return nil
}

// getEvidence is Tower's /provenance for one image, read with the app's own registry credential.
func (s *Server) getEvidence(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	image := r.URL.Query().Get("image")
	if image == "" {
		return evidence.Invalid{What: `missing required "image" query param`}
	}
	p, err := s.Evidence.Read(ctx, app.Namespace, image)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

func (s *Server) listTests(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	q, present := r.URL.Query()["limit"]
	limit := ""
	if present {
		limit = q[0]
	}
	runs, err := s.Tests.Runs(ctx, app.Name, testkube.Limit(limit, present))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
	return nil
}

func (s *Server) getTestReport(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	xml, err := s.Tests.Report(ctx, app.Name, r.PathValue("id"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write(xml)
	return nil
}

// HeaderDryRun asks for a dry run like ?dryRun=true does. A caller coming through the API server's service proxy
// (Tower) must use it: the API server refuses a proxied request whose query has dryRun ("dryRun is not supported").
const HeaderDryRun = "X-Glidepath-Dry-Run"

// dryRun reads ?dryRun=true|false and the X-Glidepath-Dry-Run header; either may ask, and they must not disagree.
func dryRun(r *http.Request) (bool, error) {
	parse := func(v string) (bool, bool) {
		switch v {
		case "", "false":
			return false, true
		case "true":
			return true, true
		}
		return false, false
	}
	q, okQ := parse(r.URL.Query().Get("dryRun"))
	h, okH := parse(strings.TrimSpace(r.Header.Get(HeaderDryRun)))
	if !okQ || !okH {
		return false, pipelineruns.Invalid{What: "dryRun must be true or false"}
	}
	if r.URL.Query().Get("dryRun") != "" && r.Header.Get(HeaderDryRun) != "" && q != h {
		return false, pipelineruns.Invalid{What: "dryRun and " + HeaderDryRun + " disagree"}
	}
	return q || h, nil
}

// rerunRun starts a copy of a PipelineRun, once per Idempotency-Key: 201 when this call created it,
// 200 when it is a replay or a dry run.
func (s *Server) rerunRun(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	dry, err := dryRun(r)
	if err != nil {
		return err
	}
	key := r.Header.Get("Idempotency-Key")
	if err := pipelineruns.ValidKey(key); err != nil {
		return err
	}
	name := r.PathValue("name")
	note(w, "idempotencyKeySha256", pipelineruns.KeyHash(app.Name, name, key)[:12])
	out, created, err := s.Runs.Rerun(ctx, app.Namespace, app.Name, name, key, dry)
	if err != nil {
		return err
	}
	note(w, "created", out.Name, "replayed", out.Replayed)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
	return nil
}

// cancelRun cancels a PipelineRun (CancelledRunFinally); one already cancelled or finished is a 200
// that says so.
func (s *Server) cancelRun(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	dry, err := dryRun(r)
	if err != nil {
		return err
	}
	out, err := s.Runs.Cancel(ctx, app.Namespace, r.PathValue("name"), dry)
	if err != nil {
		return err
	}
	note(w, "result", out.Status)
	writeJSON(w, http.StatusOK, out)
	return nil
}

// NewLogger writes JSON lines to stdout.
func NewLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stdout, nil)) }
