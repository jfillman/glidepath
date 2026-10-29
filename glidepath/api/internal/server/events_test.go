package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glidepath/api/internal/events"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// evReviewer: "sa-<namespace>" is a pipeline ServiceAccount in that namespace, for the broker's audience.
type evReviewer struct{}

func (evReviewer) Review(_ context.Context, token string, auds []string) (*authenticationv1.TokenReviewStatus, error) {
	if !strings.HasPrefix(token, "sa-") || len(auds) != 1 || auds[0] != "cdevents-broker" {
		return &authenticationv1.TokenReviewStatus{}, nil
	}
	return &authenticationv1.TokenReviewStatus{Authenticated: true,
		User: authenticationv1.UserInfo{Username: "system:serviceaccount:" + token[3:] + ":pipeline-runner"}}, nil
}

func newEventServer(t *testing.T, allow map[string]bool) (*Server, func()) {
	s, done := newServer(t, allow)
	s.Apps.CS.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-cart-cicd"}}, metav1.CreateOptions{})
	s.Events = &events.Store{Max: 100, MaxAge: time.Hour, MaxStreams: 2}
	s.Tap = &events.Tap{Verifier: &events.Verifier{Reviewer: evReviewer{}, Audience: "cdevents-broker"},
		Store: s.Events, Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	s.Heartbeat = 50 * time.Millisecond
	s.ResumeGrace = 0
	return s, done
}

// send posts a pipeline's event through the tap, as the broker's interceptor call would.
func send(t *testing.T, h http.Handler, ns, id, typ string) {
	ev, _ := json.Marshal(map[string]any{"context": map[string]any{"id": id, "source": "/glidepath/" + ns + "/run-" + id, "type": typ},
		"subject": map[string]any{"id": "run-" + id, "type": "pipelineRun", "content": map[string]any{}}})
	raw, _ := json.Marshal(map[string]any{"body": string(ev), "header": map[string][]string{"Authorization": {"Bearer sa-" + ns}}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/internal/events/tap", bytes.NewReader(raw)))
	if !strings.Contains(w.Body.String(), `"continue":false`) {
		t.Fatal("tap:", w.Body.String())
	}
}

func TestEventsAuthz(t *testing.T) {
	s, done := newEventServer(t, map[string]bool{
		"tower *":                           true, // cluster-wide reader
		"shopper list events app-shop-cicd": true, // one app's reader
	})
	defer done()
	h := s.Handler()
	send(t, h, "app-shop-cicd", "1", "dev.cdevents.pipelinerun.finished.0.3.0")
	send(t, h, "app-cart-cicd", "2", "dev.cdevents.pipelinerun.started.0.3.0")
	cases := []struct {
		name, who, query string
		status           int
		rule             string
		n                int
	}{
		{"cluster-wide reader, every app", "tower", "", 200, "", 2},
		{"cluster-wide reader, one app", "tower", "?app=cart", 200, "", 1},
		{"app reader without app=", "shopper", "", 403, "GP-AUTHZ-001", 0},
		{"app reader, its app", "shopper", "?app=shop", 200, "", 1},
		{"app reader, another app", "shopper", "?app=cart", 403, "GP-AUTHZ-001", 0},
		{"app reader, no such app", "shopper", "?app=nope", 403, "GP-AUTHZ-001", 0},
		{"reader, no such app", "tower", "?app=nope", 404, "GP-API-404", 0},
		{"bad app", "tower", "?app=Bad_Name", 400, "GP-API-400", 0},
		{"bad since", "tower", "?since=yesterday", 400, "GP-API-400", 0},
		{"type prefix", "tower", "?type=dev.cdevents.pipelinerun.fin", 200, "", 1},
		{"no token", "", "", 401, "GP-AUTH-001", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hdr := map[string]string{}
			if c.who != "" {
				hdr[`X-Glidepath-Authorization`] = "Bearer t-" + c.who
			}
			status, out, body := call(t, s, "/v1/events/recent"+c.query, hdr)
			if status != c.status || (c.rule != "" && out["rule"] != c.rule) {
				t.Fatalf("got %d %s", status, body)
			}
			if status == 200 && len(out["events"].([]any)) != c.n {
				t.Fatalf("events: %s", body)
			}
		})
	}
	_, out, _ := call(t, s, "/v1/events/recent", map[string]string{"Authorization": "Bearer t-shopper"})
	if !strings.Contains(out["hint"].(string), "app=") {
		t.Fatal("cluster-wide denial should say to pass app=:", out)
	}
	_, out, _ = call(t, s, "/v1/events/recent?app=shop", map[string]string{"Authorization": "Bearer t-tower"})
	e := out["events"].([]any)[0].(map[string]any)
	if e["app"] != "shop" || e["routingKey"] != "shop.pipelinerun.finished" || e["id"] == "" || e["receivedAt"] == "" ||
		e["event"].(map[string]any)["context"].(map[string]any)["id"] != "1" {
		t.Fatal("envelope:", e)
	}
}

type sseEvent struct{ id, data string }

// readSSE reads the stream into events and comments until ctx ends.
func readSSE(body io.Reader, evs chan<- sseEvent, comments chan<- string) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur sseEvent
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, ":"):
			select {
			case comments <- l:
			default:
			}
		case strings.HasPrefix(l, "id: "):
			cur.id = l[4:]
		case strings.HasPrefix(l, "data: "):
			cur.data = l[6:]
		case l == "" && cur.data != "":
			evs <- cur
			cur = sseEvent{}
		}
	}
	close(evs)
}

func open(t *testing.T, ctx context.Context, url, who, lastID string) (*http.Response, chan sseEvent, chan string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer t-"+who)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	evs, comments := make(chan sseEvent, 16), make(chan string, 64)
	go readSSE(resp.Body, evs, comments)
	return resp, evs, comments
}

func next(t *testing.T, evs chan sseEvent) map[string]any {
	select {
	case e, ok := <-evs:
		if !ok {
			t.Fatal("stream ended")
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(e.data), &env); err != nil || env["id"] != e.id {
			t.Fatalf("bad event %+v", e)
		}
		return env
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
	}
	return nil
}

func TestEventStream(t *testing.T) {
	s, done := newEventServer(t, map[string]bool{"tower *": true})
	defer done()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	h := s.Handler()

	ctx, cancel := context.WithCancel(context.Background())
	_, evs, comments := open(t, ctx, srv.URL+"/v1/events?app=shop&type=dev.cdevents.pipelinerun.", "tower", "")
	// Heartbeats while idle.
	deadline := time.After(2 * time.Second)
	for got := 0; got < 2; {
		select {
		case c := <-comments:
			if c == ": ping" {
				got++
			}
		case <-deadline:
			t.Fatal("no heartbeat")
		}
	}
	send(t, h, "app-cart-cicd", "c1", "dev.cdevents.pipelinerun.started.0.3.0") // another app
	send(t, h, "app-shop-cicd", "s0", "dev.cdevents.artifact.published.0.3.0")  // another type
	send(t, h, "app-shop-cicd", "s1", "dev.cdevents.pipelinerun.started.0.3.0")
	first := next(t, evs)
	if first["routingKey"] != "shop.pipelinerun.started" {
		t.Fatal(first)
	}
	send(t, h, "app-shop-cicd", "s2", "dev.cdevents.pipelinerun.finished.0.3.0")
	second := next(t, evs)
	if second["routingKey"] != "shop.pipelinerun.finished" {
		t.Fatal(second)
	}
	// Disconnect: the server lets the stream go.
	cancel()
	for i := 0; i < 100 && s.Events.Streams() != 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Events.Streams() != 0 {
		t.Fatal("stream not released after disconnect")
	}

	// Resume after the first event: the second is replayed, then live ones follow.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	_, evs2, _ := open(t, ctx2, srv.URL+"/v1/events?app=shop&type=dev.cdevents.pipelinerun.", "tower", first["id"].(string))
	if got := next(t, evs2); got["id"] != second["id"] {
		t.Fatal("resume replayed", got)
	}
	send(t, h, "app-shop-cicd", "s3", "dev.cdevents.pipelinerun.finished.0.3.0")
	if got := next(t, evs2); got["event"].(map[string]any)["context"].(map[string]any)["id"] != "s3" {
		t.Fatal(got)
	}

	// ?since= does the same as Last-Event-ID.
	_, out, _ := call(t, s, "/v1/events/recent?app=shop&since="+first["id"].(string), map[string]string{"Authorization": "Bearer t-tower"})
	if n := len(out["events"].([]any)); n != 2 {
		t.Fatal("since:", out)
	}

	// The stream limit (2): one open, one more allowed, then 503.
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	open(t, ctx3, srv.URL+"/v1/events", "tower", "")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer t-tower")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 || !strings.Contains(string(b), "GP-API-503") {
		t.Fatal(resp.StatusCode, string(b))
	}

	// Shutdown ends open streams.
	s.Events.Close()
	for range evs2 {
	}
}
