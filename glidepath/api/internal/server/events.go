package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/events"
)

// eventQuery reads app (already authorized by wrap), type and the resume point.
func (s *Server) eventQuery(r *http.Request, app *apps.App) (events.Filter, events.Resume, error) {
	f := events.Filter{TypePrefix: r.URL.Query().Get("type")}
	if len(f.TypePrefix) > 256 {
		return f, events.Resume{}, events.Invalid{What: "type is too long"}
	}
	if app != nil {
		f.App = app.Name
	}
	res, err := events.ParseResume(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("since"), s.ResumeGrace)
	return f, res, err
}

// listRecentEvents is the buffer as JSON, oldest first: for tests, agents, and a client that would
// rather poll than hold a stream.
func (s *Server) listRecentEvents(_ context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	f, res, err := s.eventQuery(r, app)
	if err != nil {
		return err
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			return events.Invalid{What: "limit must be 1..10000"}
		}
		limit = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": s.Events.Recent(f, res, limit)})
	return nil
}

// streamEvents serves server-sent events: each matching event as `id: <id>` and `data: <envelope>`,
// a comment line every Heartbeat so an idle stream survives proxies, until the client goes away.
// Last-Event-ID or ?since= replays what the buffer still holds after that point first.
func (s *Server) streamEvents(ctx context.Context, w http.ResponseWriter, r *http.Request, _ *auth.Caller, app *apps.App) error {
	f, res, err := s.eventQuery(r, app)
	if err != nil {
		return err
	}
	sub, replay, err := s.Events.Subscribe(f, res)
	if err != nil {
		return err
	}
	defer s.Events.Unsubscribe(sub)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(e events.Envelope) error {
		data, _ := json.Marshal(e)
		if _, err := w.Write([]byte("id: " + e.ID + "\ndata: " + string(data) + "\n\n")); err != nil {
			return err
		}
		return nil
	}
	if _, err := w.Write([]byte("retry: 5000\n: glidepath-api events\n\n")); err != nil {
		return nil
	}
	for _, e := range replay {
		if write(e) != nil {
			return nil
		}
	}
	if rc.Flush() != nil {
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
		case e, ok := <-sub.C:
			if !ok {
				if sub.Lagged {
					w.Write([]byte(": lagged; reconnect with Last-Event-ID\n\n"))
				}
				rc.Flush()
				return nil
			}
			if write(e) != nil {
				return nil
			}
		}
		if rc.Flush() != nil {
			return nil
		}
	}
}
