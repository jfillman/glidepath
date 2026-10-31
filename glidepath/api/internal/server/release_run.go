package server

// Follow-ons 4 and 5 (2026-10-31): following a release run after promote or rollback answers with its name, and an
// app's live runs pushed over server-sent events instead of polled (internal/live run.go and stream.go).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/live"
	"github.com/glidepath/api/internal/releases"
)

// releaseRunAnswer is a release run with the release record its chain id names, and where it is in one word.
type releaseRunAnswer struct {
	*live.ReleaseRun
	// Phase: the record's state once open-release-pr has written it (proposed, merged, progressing, healthy, ...),
	// else the run's: running, failed, cancelled, or finished (it succeeded without a record, e.g. nothing to release).
	Phase  string           `json:"phase"`
	Record *releases.Record `json:"record"`
}

// getReleaseRun is one release run and its release record: what Tower follows from the moment promote or rollback
// answers with the run's name until the PR shows.
func (s *Server) getReleaseRun(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	run, err := s.Live.ReleaseRun(ctx, app.Namespace, r.PathValue("name"))
	if err != nil {
		return err
	}
	out := releaseRunAnswer{ReleaseRun: run}
	if run.ChainID != "" {
		rec, err := s.Releases.Get(ctx, app.Namespace, app.Name, run.ChainID)
		var nf releases.NotFound
		var inv releases.Invalid
		switch {
		case err == nil:
			out.Record = rec
		case errors.As(err, &nf), errors.As(err, &inv):
		default:
			return err
		}
	}
	switch {
	case out.Record != nil && out.Record.Data["state"] != "":
		out.Phase = out.Record.Data["state"]
		if out.PRURL == "" {
			out.PRURL = out.Record.Data["prUrl"]
		}
		if out.ChangeRequest == "" {
			out.ChangeRequest = out.Record.Data["changeRequestNumber"]
		}
	case run.Status == "succeeded":
		out.Phase = "finished"
	default:
		out.Phase = run.Status
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// streamLiveRuns is GET /v1/apps/{app}/live as server-sent events: `event: snapshot` with every PipelineRun and
// TaskRun when it opens, then `event: change` per object that appears, moves or goes. No resume: a viewer that
// reconnects gets a fresh snapshot, which is the whole state. A viewer that falls behind is closed with a comment
// line and reconnects.
func (s *Server) streamLiveRuns(ctx context.Context, w http.ResponseWriter, _ *http.Request, _ *auth.Caller, app *apps.App) error {
	sub, snap, err := s.LiveHub.Subscribe(ctx, app.Namespace)
	if err != nil {
		return err
	}
	defer s.LiveHub.Unsubscribe(sub)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, v any) error {
		data, _ := json.Marshal(v)
		_, err := w.Write([]byte("event: " + event + "\ndata: " + string(data) + "\n\n"))
		return err
	}
	if _, err := w.Write([]byte("retry: 5000\n: glidepath-api live runs\n\n")); err != nil {
		return nil
	}
	if send("snapshot", snap) != nil || rc.Flush() != nil {
		return nil
	}
	beat := s.Heartbeat
	if beat <= 0 {
		beat = 15 * time.Second
	}
	tick := time.NewTicker(beat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return nil
			}
		case c, ok := <-sub.C:
			if !ok {
				if sub.Lagged {
					w.Write([]byte(": lagged; reconnect for a fresh snapshot\n\n"))
				}
				rc.Flush()
				return nil
			}
			if send("change", c) != nil {
				return nil
			}
		}
		if rc.Flush() != nil {
			return nil
		}
	}
}
