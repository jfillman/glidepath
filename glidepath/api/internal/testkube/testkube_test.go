package testkube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glidepath/api/internal/jsval"
)

// The mapping cases are Tower's testEvidence.test.ts (mapTestkubeExecutions, mapWithConcurrency,
// the paths); checkRideEvidence and changeRequestOf are the release-record sealer's, not ported.

func parse(t *testing.T, s string) any {
	t.Helper()
	v, err := jsval.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func rowsJSON(rows []*jsval.Obj) string {
	b, _ := json.Marshal(rows)
	return string(b)
}

func TestMapExecutions(t *testing.T) {
	// Steps named by the signature, and the aborted-but-passed race corrected.
	rows := MapExecutions(parse(t, `{"results":[
		{"id":"6ad6","name":"boarding-api-check-ride-3","workflow":{"name":"boarding-api-check-ride"},
		 "tags":{"app":"boarding-api","role":"check-ride","env":"dev","image":"ghcr.io/o/boarding-api:2.0.0-926d566"},
		 "signature":[{"name":"reach the service","ref":"r1"},{"name":"report","ref":"r2"}],
		 "result":{"status":"aborted","startedAt":"2026-10-19T17:30:00Z","finishedAt":"2026-10-19T17:31:12Z","durationMs":72000,"steps":{"r1":{"status":"passed"},"r2":{"status":"passed"}}}},
		{"id":"x","workflow":{"name":"w"},"result":{"status":"failed","steps":{"a":{"status":"failed"}}}}]}`))
	got := rowsJSON(rows)
	want := `[{"id":"6ad6","name":"boarding-api-check-ride-3","workflow":"boarding-api-check-ride","status":"passed",` +
		`"statusNote":"The agent reported aborted while every step passed (a known race on short runs); counted as passed, as Glidepath does.",` +
		`"role":"check-ride","env":"dev","image":"ghcr.io/o/boarding-api:2.0.0-926d566","startedAt":"2026-10-19T17:30:00Z","finishedAt":"2026-10-19T17:31:12Z",` +
		`"durationMs":72000,"steps":[{"name":"reach the service","status":"passed"},{"name":"report","status":"passed"}],"hasReport":true},` +
		`{"id":"x","name":"x","workflow":"w","status":"failed","steps":[{"name":"a","status":"failed"}],"hasReport":true}]`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestMapListSummary(t *testing.T) {
	// The list endpoint's summary (Testkube 2.13.1, live 2026-10-20): status only, so it reads as
	// aborted with no report, which is why the route reads details.
	rows := MapExecutions(parse(t, `{"results":[{"id":"6ad6","name":"boarding-api-check-ride-3","workflow":{"name":"boarding-api-check-ride"},
		"tags":{"app":"boarding-api","role":"check-ride"},"result":{"status":"aborted","durationMs":72000}}]}`))
	if got := rowsJSON(rows); got != `[{"id":"6ad6","name":"boarding-api-check-ride-3","workflow":"boarding-api-check-ride","status":"aborted","role":"check-ride","durationMs":72000,"steps":[],"hasReport":false}]` {
		t.Fatal(got)
	}
	if got := ExecutionIDs(parse(t, `{"results":[{"id":"6ad6"},{"id":""},{},{"id":"x1"}]}`)); fmt.Sprint(got) != "[6ad6 x1]" {
		t.Fatal(got)
	}
	if DetailPath("6ad6") != "/v1/test-workflow-executions/6ad6" {
		t.Fatal(DetailPath("6ad6"))
	}
}

// Null is not absent: a null tag is kept, a null status falls back, steps keep their order.
func TestMapNullsAndOrder(t *testing.T) {
	rows := MapExecutions(parse(t, `{"results":[null,{"id":"a","name":null,"tags":{"role":null},"scheduledAt":"s",
		"result":{"status":null,"startedAt":null,"finishedAt":null,"steps":{"z":{"status":"passed"},"10":{},"2":{"status":"x"},"a":{"status":"y"}}}}]}`))
	want := `[{"id":"a","name":"a","workflow":"","status":"queued","role":null,"startedAt":"s","finishedAt":null,` +
		`"steps":[{"name":"2","status":"x"},{"name":"10","status":""},{"name":"z","status":"passed"},{"name":"a","status":"y"}],"hasReport":false}]`
	if got := rowsJSON(rows); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestMapWithConcurrency(t *testing.T) {
	// A few at a time, keeping the list order.
	var mu sync.Mutex
	inFlight, peak := 0, 0
	out := mapWithConcurrency([]int{1, 2, 3, 4, 5, 6}, 2, func(n int) int {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(time.Duration(5*(7-n)) * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return n * 10
	})
	if fmt.Sprint(out) != "[10 20 30 40 50 60]" || peak != 2 {
		t.Fatal(out, peak)
	}
}

func TestPaths(t *testing.T) {
	if got := ListPath("boarding-api", "20"); got != "/v1/test-workflow-executions?tagSelector=app%3Dboarding-api&pageSize=20" {
		t.Fatal(got)
	}
	// Through the API server's proxy (Tower's path) the name is encoded twice; direct, once.
	if got := (&Client{ProxyDecodes: true}).ReportPath("6ad6"); got != "/v1/test-workflow-executions/6ad6/artifacts/data%252Fartifacts%252Fjunit.xml" {
		t.Fatal(got)
	}
	if got := (&Client{}).ReportPath("6ad6"); got != "/v1/test-workflow-executions/6ad6/artifacts/data%2Fartifacts%2Fjunit.xml" {
		t.Fatal(got)
	}
	for q, want := range map[string]string{"": "50", "3": "3", "0": "50", "abc": "50", "500": "200", "-4": "1", "2.5": "2.5"} {
		if got := Limit(q, q != ""); got != want {
			t.Errorf("limit %q: %s, want %s", q, got, want)
		}
	}
}

func stub(t *testing.T, failDetail string) (*Client, *[]string) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.EscapedPath() {
		case "/v1/test-workflow-executions":
			io.WriteString(w, `{"results":[{"id":"e1","result":{"status":"aborted"}},{"id":"e2","name":"two","result":{"status":"aborted"}}]}`)
		case "/v1/test-workflow-executions/e1", "/v1/test-workflow-executions/e2":
			id := strings.TrimPrefix(r.URL.Path, "/v1/test-workflow-executions/")
			if id == failDetail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, `{"id":%q,"tags":{"app":"shop"},"signature":[{"name":"report","ref":"r"}],"result":{"status":"passed","steps":{"r":{"status":"passed"}}}}`, id)
		case "/v1/test-workflow-executions/e1/artifacts/data%2Fartifacts%2Fjunit.xml":
			io.WriteString(w, `<testsuites/>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, HTTP: srv.Client(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &paths
}

func TestRuns(t *testing.T) {
	c, paths := stub(t, "e2")
	rows, err := c.Runs(context.Background(), "shop", "50")
	if err != nil {
		t.Fatal(err)
	}
	// e1 from its detail; e2's detail failed, so its summary.
	got := rowsJSON(rows)
	want := `[{"id":"e1","name":"e1","workflow":"","status":"passed","steps":[{"name":"report","status":"passed"}],"hasReport":true},` +
		`{"id":"e2","name":"two","workflow":"","status":"aborted","steps":[],"hasReport":false}]`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if (*paths)[0] != "/v1/test-workflow-executions?tagSelector=app%3Dshop&pageSize=50" {
		t.Fatal((*paths)[0])
	}
}

func TestReport(t *testing.T) {
	c, _ := stub(t, "")
	if b, err := c.Report(context.Background(), "shop", "e1"); err != nil || string(b) != "<testsuites/>" {
		t.Fatal(string(b), err)
	}
	// Another app's run, a run with no report, a malformed id.
	if _, err := c.Report(context.Background(), "other", "e1"); err == nil || !strings.Contains(err.Error(), "not a run of other") {
		t.Fatal(err)
	} else if _, ok := err.(NotFound); !ok {
		t.Fatalf("%T", err)
	}
	if _, err := c.Report(context.Background(), "shop", "e2"); err == nil {
		t.Fatal("e2 has no report")
	} else if _, ok := err.(NotFound); !ok {
		t.Fatalf("%T", err)
	}
	if _, err := c.Report(context.Background(), "shop", "../x"); err == nil {
		t.Fatal("malformed id")
	} else if _, ok := err.(Invalid); !ok {
		t.Fatalf("%T", err)
	}
}
