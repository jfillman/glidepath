// Package testkube reads an app's test runs and their JUnit reports from Testkube's standalone
// agent API (testkube-api-server.testkube:8088), in the cluster.
//
// Ported from Tower's backend (backstage packages/backend/src/testEvidence.ts and the /tests and
// /tests/report routes in glidepathProvenance.ts) with the same reads and the same row shape, so
// Tower can shadow its own answer against this one field for field (docs/admin/api.md). Glidepath
// tags every run it starts with app=<name>, which is the whole filter.
package testkube

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/glidepath/api/internal/jsval"
)

// NotFound: the execution, or its report, doesn't exist (or isn't this app's).
type NotFound struct{ What string }

func (e NotFound) Error() string { return e.What }

// Invalid: a malformed execution id.
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

// Client talks to Testkube's agent API.
type Client struct {
	Base string // http://testkube-api-server.testkube.svc.cluster.local:8088
	// ProxyDecodes: Base is the API server's services/proxy, which decodes the path once before
	// forwarding, so the report's artifact name is encoded twice (Tower's path). Direct, once.
	ProxyDecodes bool
	HTTP         *http.Client
	Log          *slog.Logger
}

const concurrency = 4

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("testkube: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		b := string(body)
		if len(b) > 200 {
			b = b[:200]
		}
		return nil, fmt.Errorf("request to %s failed with %d: %s", path, res.StatusCode, b)
	}
	return body, nil
}

// Limit is Tower's Math.min(Math.max(Number(limit ?? 50) || 50, 1), 200), as the page size it sends.
func Limit(q string, present bool) string {
	n := 50.0
	if present {
		n = jsval.Number(q)
	}
	if !jsval.Truthy(n) {
		n = 50
	}
	return jsval.NumberString(min(max(n, 1), 200))
}

// ListPath is testkubeListPath: every execution tagged app=<name>.
func ListPath(app, pageSize string) string {
	return "/v1/test-workflow-executions?tagSelector=" + url.QueryEscape("app="+app) + "&pageSize=" + url.QueryEscape(pageSize)
}

// DetailPath is one execution in full. The list returns summaries only (no signature, no
// result.steps; Testkube 2.13.1), so every run read from the list alone looks aborted with no
// steps and no report: each listed run is read through this.
func DetailPath(id string) string { return "/v1/test-workflow-executions/" + encodeURIComponent(id) }

// ReportPath is the JUnit report the check-ride templates leave, data/artifacts/junit.xml. Testkube
// wants the artifact's slashes as %2F in one path segment.
func (c *Client) ReportPath(id string) string {
	name := encodeURIComponent("data/artifacts/junit.xml")
	if c.ProxyDecodes {
		name = encodeURIComponent(name)
	}
	return DetailPath(id) + "/artifacts/" + name
}

// encodeURIComponent is JavaScript's.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ExecutionIDs is testkubeExecutionIds: the ids the list returned, in its order.
func ExecutionIDs(list any) []string {
	var ids []string
	for _, r := range arr(jsval.Get(list, "results")) {
		if id, ok := jsval.Get(r, "id").(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

// mapWithConcurrency runs fn over items with at most n in flight, keeping the order.
func mapWithConcurrency[T, R any](items []T, n int, fn func(T) R) []R {
	out := make([]R, len(items))
	next := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < min(n, len(items)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= len(items) {
					return
				}
				out[i] = fn(items[i])
			}
		}()
	}
	wg.Wait()
	return out
}

// Runs is Tower's /tests: the app's executions, each read in full a few at a time, falling back to
// the list's summary for one whose detail read fails so the list still renders.
func (c *Client) Runs(ctx context.Context, app, pageSize string) ([]*jsval.Obj, error) {
	body, err := c.get(ctx, ListPath(app, pageSize))
	if err != nil {
		return nil, err
	}
	list, err := jsval.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("testkube list: %w", err)
	}
	details := mapWithConcurrency(ExecutionIDs(list), concurrency, func(id string) any {
		b, err := c.get(ctx, DetailPath(id))
		var v any
		if err == nil {
			v, err = jsval.Parse(b)
		}
		if err != nil {
			c.Log.Warn("test run detail read failed", "id", id, "error", err.Error())
			for _, r := range arr(jsval.Get(list, "results")) {
				if jsval.Get(r, "id") == any(id) {
					return r
				}
			}
			return jsval.Undefined
		}
		return v
	})
	results := make([]any, len(details))
	copy(results, details)
	return MapExecutions(jsval.NewObj().Set("results", results)), nil
}

// Report is Tower's /tests/report: the execution's JUnit report as Testkube serves it. The
// execution must be this app's (tagged app=<name>); Tower's route takes no app and doesn't check.
// Any failure is NotFound, as Tower answers 404 for any failure.
func (c *Client) Report(ctx context.Context, app, id string) ([]byte, error) {
	if !reportID.MatchString(id) {
		return nil, Invalid{`missing or malformed "id"`}
	}
	b, err := c.get(ctx, DetailPath(id))
	if err != nil {
		return nil, NotFound{fmt.Sprintf("no JUnit report for execution %s: %v", id, err)}
	}
	detail, err := jsval.Parse(b)
	if err != nil || jsval.Path(detail, "tags", "app") != any(app) {
		return nil, NotFound{fmt.Sprintf("no JUnit report for execution %s: not a run of %s", id, app)}
	}
	xml, err := c.get(ctx, c.ReportPath(id))
	if err != nil {
		return nil, NotFound{fmt.Sprintf("no JUnit report for execution %s: %v", id, err)}
	}
	return xml, nil
}

var reportID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var reportStep = regexp.MustCompile(`(?i)report`)

// MapExecutions is mapTestkubeExecutions: Testkube's executions reduced to what the Tests view
// shows. The agent (2.13.1) reports a short run as aborted while every step passed (a race with
// the Job controller that Glidepath's run-testworkflow already works around); the same rule
// applies here and is flagged on the row.
func MapExecutions(body any) []*jsval.Obj {
	rows := []*jsval.Obj{}
	for _, e := range arr(jsval.Get(body, "results")) {
		if !jsval.Truthy(e) || !jsval.Truthy(jsval.Get(e, "id")) {
			continue
		}
		names := map[string]any{}
		flattenSignature(jsval.Get(e, "signature"), names)
		result := jsval.Get(e, "result")
		steps := []any{}
		allPassed := true
		hasReportStep := false
		if so, ok := jsval.Or(jsval.Get(result, "steps"), jsval.NewObj()).(*jsval.Obj); ok {
			for _, ref := range so.Keys() {
				name := any(ref)
				if n, ok := names[ref]; ok && !jsval.Nullish(n) {
					name = n
				}
				status := jsval.Or(jsval.Get(so.Get(ref), "status"), "")
				steps = append(steps, jsval.NewObj().Set("name", name).Set("status", status))
				if status != any("passed") {
					allPassed = false
				}
				if reportStep.MatchString(jsval.String(name)) {
					hasReportStep = true
				}
			}
		}
		status := jsval.Or(jsval.Get(result, "status"), "queued")
		var note any = jsval.Undefined
		if status == any("aborted") && len(steps) > 0 && allPassed {
			status = "passed"
			note = "The agent reported aborted while every step passed (a known race on short runs); counted as passed, as Glidepath does."
		}
		tags := jsval.Or(jsval.Get(e, "tags"), jsval.NewObj())
		id := jsval.String(jsval.Get(e, "id"))
		rows = append(rows, jsval.NewObj().
			Set("id", id).
			Set("name", jsval.Or(jsval.Get(e, "name"), id)).
			Set("workflow", jsval.Or(jsval.Path(e, "workflow", "name"), "")).
			Set("status", status).
			Set("statusNote", note).
			Set("role", jsval.Get(tags, "role")).
			Set("env", jsval.Get(tags, "env")).
			Set("test", jsval.Get(tags, "test")).
			Set("image", jsval.Get(tags, "image")).
			Set("revision", jsval.Get(tags, "revision")).
			Set("chain", jsval.Get(tags, "chain")).
			Set("startedAt", jsval.Or(jsval.Get(result, "startedAt"), jsval.Get(e, "scheduledAt"))).
			Set("finishedAt", jsval.Get(result, "finishedAt")).
			Set("durationMs", jsval.Get(result, "durationMs")).
			Set("steps", steps).
			Set("hasReport", hasReportStep || status == any("passed") || status == any("failed")))
	}
	return rows
}

// flattenSignature maps each step ref in the workflow's signature tree to its name.
func flattenSignature(sig any, out map[string]any) {
	for _, s := range arr(sig) {
		if ref := jsval.Get(s, "ref"); jsval.Truthy(ref) {
			if r, ok := ref.(string); ok {
				out[r] = jsval.Or(jsval.Get(s, "name"), ref) // a later duplicate wins, as new Map(entries) does
			}
		}
		if ch := jsval.Get(s, "children"); jsval.Truthy(ch) {
			flattenSignature(ch, out)
		}
	}
}
