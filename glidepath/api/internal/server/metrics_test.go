package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glidepath/api/internal/metrics"
)

func scrape(t *testing.T, m *metrics.Metrics) string {
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestMetrics(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	s.Metrics = metrics.New(7, metrics.Gauges{LiveWatches: func() float64 { return 3 }})
	tower := map[string]string{"Authorization": "Bearer t-tower"}
	call(t, s, "/v1/whoami", tower)
	call(t, s, "/v1/whoami", nil)
	call(t, s, "/v1/apps/nope/runs", tower)

	// A caller that went away before the answer: 499, not a 502 against the availability SLO, and not timed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/apps/shop/runs", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer t-tower")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Body.Len() != 0 {
		t.Fatalf("answered a caller that left: %s", rec.Body.String())
	}

	out := scrape(t, s.Metrics)
	for _, want := range []string{
		`glidepath_api_requests_total{code="200",decision="allow",op="whoami",tier="T0"} 1`,
		`glidepath_api_requests_total{code="401",decision="deny",op="whoami",tier="T0"} 1`,
		`glidepath_api_requests_total{code="404",decision="allow",op="listRuns",tier="T0"} 1`,
		`glidepath_api_requests_total{code="499",decision="allow",op="listRuns",tier="T0"} 1`,
		`glidepath_api_request_duration_seconds_count{op="listRuns",tier="T0"} 1`,
		`glidepath_api_request_duration_seconds_bucket{op="whoami",tier="T0",le="2.5"} 2`,
		`glidepath_api_in_flight_requests{op="whoami"} 0`,
		`glidepath_api_contract_info{contract_version="7"} 1`,
		`glidepath_api_live_watches 3`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "glidepath_api_event_streams") {
		t.Error("a gauge with no source is not registered")
	}
}

func TestNoMetricsRecordsNothing(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	if st, _, _ := call(t, s, "/v1/whoami", map[string]string{"Authorization": "Bearer t-tower"}); st != 200 {
		t.Fatalf("whoami without metrics: %d", st)
	}
}
