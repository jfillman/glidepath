package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glidepath/api/internal/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func withTracer(t *testing.T) *tracetest.InMemoryExporter {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	if _, err := tracing.Setup(context.Background(), "test"); err != nil { // the propagator; no endpoint, no exporter
		t.Fatal(err)
	}
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return exp
}

func TestTracing(t *testing.T) {
	exp := withTracer(t)
	s, done := newServer(t, reader)
	defer done()
	h := tracing.Handler(s.Handler())

	// Joined to the caller's trace: Backstage's traceparent through the service proxy.
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer t-tower")
	req.Header.Set("traceparent", parent)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Glidepath-Trace-Id"); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id header: %q", got)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "whoami" || spans[0].Parent.SpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("spans: %+v", spans)
	}
	attrs := map[string]string{}
	for _, a := range spans[0].Attributes {
		attrs[string(a.Key)] = a.Value.Emit()
	}
	if attrs["glidepath.op"] != "whoami" || attrs["glidepath.caller"] != "tower" || attrs["glidepath.decision"] != "allow" {
		t.Fatalf("attributes: %v", attrs)
	}

	// A new trace when the caller sends none; probes are not traced.
	exp.Reset()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/whoami", nil))
	if rec.Header().Get("X-Glidepath-Trace-Id") == "" || len(exp.GetSpans()) != 1 {
		t.Fatalf("untraced caller: %q %d", rec.Header().Get("X-Glidepath-Trace-Id"), len(exp.GetSpans()))
	}
	if exp.GetSpans()[0].Status.Code == codes.Error {
		t.Fatal("a 401 is not the API failing")
	}
	exp.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("probe traced: %d spans", n)
	}
}

// An event stream still flushes through the tracing handler's ResponseWriter.
func TestEventStreamTraced(t *testing.T) {
	withTracer(t)
	s, done := newEventServer(t, map[string]bool{"tower *": true})
	defer done()
	srv := httptest.NewServer(tracing.Handler(s.Handler()))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, comments := open(t, ctx, srv.URL+"/v1/events?app=shop", "tower", "")
	select {
	case <-comments:
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat through the tracing handler")
	}
}

// An outbound call is a child span only inside a traced call.
func TestTransportOnlyInsideATrace(t *testing.T) {
	exp := withTracer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer up.Close()
	c := tracing.Client(&http.Client{})
	req, _ := http.NewRequest(http.MethodGet, up.URL, nil)
	if resp, err := c.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close() // the client span ends with the body
	}
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("a background call started a trace: %d spans", n)
	}
	ctx, span := otel.Tracer("test").Start(context.Background(), "call")
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, up.URL, nil)
	if resp, err := c.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close() // the client span ends with the body
	}
	span.End()
	if n := len(exp.GetSpans()); n != 2 {
		t.Fatalf("want the call and its child, got %d spans", n)
	}
}
