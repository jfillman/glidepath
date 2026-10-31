// Package tracing is glidepath-api's OpenTelemetry setup: a span per call (named for its operation, joined to the
// caller's trace when it sends a W3C traceparent), and a child span per call it makes on that call's behalf (the API
// server, Tekton Results, Testkube, Rekor, the broker, GitHub, the validator sidecar). Spans go over OTLP/HTTP to
// the endpoint the standard OTEL_EXPORTER_OTLP_* variables name; with neither set, nothing is exported.
//
// Traces are read in Grafana (the cluster's Tempo datasource); the audit line and the X-Glidepath-Trace-Id response
// header carry the trace id to look up.
package tracing

import (
	"context"
	"net/http"
	"os"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Enabled: an OTLP endpoint is configured.
func Enabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Setup installs the W3C trace-context propagator always (so a traceparent passes through even untraced), and, when
// Enabled, a batching OTLP/HTTP exporter. The sampler is the SDK's, from OTEL_TRACES_SAMPLER/_ARG (default
// parentbased_always_on). The func it returns flushes what is buffered.
func Setup(ctx context.Context, version string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if !Enabled() {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("glidepath-api"), semconv.ServiceVersion(version)),
		resource.WithFromEnv(), // OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES win
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Handler traces every call but the probes.
func Handler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "glidepath-api", otelhttp.WithFilter(func(r *http.Request) bool {
		return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
	}))
}

// Transport traces an outbound call only inside a traced one: the live hub's polls and the cache warmers run on
// their own and would otherwise start a root trace every few seconds.
func Transport(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return otelhttp.NewTransport(rt, otelhttp.WithFilter(func(r *http.Request) bool {
		return trace.SpanContextFromContext(r.Context()).IsValid()
	}))
}

// Client is an http.Client whose calls are traced (Transport).
func Client(c *http.Client) *http.Client {
	c.Transport = Transport(c.Transport)
	return c
}
