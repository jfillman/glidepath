// Package results reads an app's runs from Tekton Results, the platform's run archive (30 days,
// step logs in object storage). Tekton deletes a finished PipelineRun about an hour after it
// completes, so anything older exists only here.
//
// Ported from Tower's backend (backstage packages/backend/src/pipelineHistory.ts) with the same
// queries, the same trimming and the same response shapes, so Tower can shadow its own answer
// against this one field for field (docs/admin/api.md). Keep the two in step until Tower's copy is
// retired (spec phase 6).
package results

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	prefix       = "/apis/results.tekton.dev/v1alpha2"
	pipelineRun  = "tekton.dev/v1.PipelineRun"
	taskRun      = "tekton.dev/v1.TaskRun"
	logType      = "results.tekton.dev/v1alpha3.Log"
	summaryPage  = 200 // Results rejects a page size under 5
	maxSummary   = 15
	runTTL       = 6 * time.Hour
	listTTL      = time.Minute
	logTTL       = time.Hour
	concurrency  = 4
	bulkAbove    = 16 // above this many uncached runs, one paged query beats a query per run
	bulkPage     = 200
	maxBulkPages = 40
)

var uid = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// NotFound: the result or its log doesn't exist.
type NotFound struct{ What string }

func (e NotFound) Error() string { return e.What }

// NoLog: the run exists but Results archived no log for that TaskRun. Distinct from NotFound so a
// client can tell "no log" (Tower shows a placeholder) from "no such run" (an error).
type NoLog struct{ What string }

func (e NoLog) Error() string { return e.What }

// Invalid: a malformed id.
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

type Obj = map[string]any

// Row is one finished run from its Result summary (~1 KB, no TaskRuns). Optional fields are
// pointers so an absent value is omitted while an empty string is kept, as Tower's undefined vs "".
type Row struct {
	Result    string  `json:"result"`
	Name      string  `json:"name"`
	Pipeline  *string `json:"pipeline,omitempty"`
	Status    string  `json:"status"`
	StartTime *string `json:"startTime,omitempty"`
	EndTime   *string `json:"endTime,omitempty"`
	Commit    *string `json:"commit,omitempty"`
	EventType *string `json:"eventType,omitempty"`
	PRNumber  *string `json:"prNumber,omitempty"`
}

// Run is one archived run in full, trimmed.
type Run struct {
	Result      string `json:"result"`
	PipelineRun Obj    `json:"pipelineRun"`
	TaskRuns    []Obj  `json:"taskRuns"`
	logs        map[string]string
}

// Client talks to the Results API: through tekton-results-relay, which adds its own identity, or
// directly with a token file.
type Client struct {
	Base      string // http://tekton-results-relay.platform-system.svc.cluster.local:8080
	TokenPath string // optional bearer token file, re-read per request so kubelet rotation needs no restart
	HTTP      *http.Client
	cache     ttlCache
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+prefix+path, nil)
	if err != nil {
		return nil, err
	}
	if c.TokenPath != "" {
		tok, err := readFile(c.TokenPath)
		if err != nil {
			return nil, fmt.Errorf("reading the results token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tok))
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tekton results: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNotFound {
		return nil, NotFound{"tekton results: not found"}
	}
	if res.StatusCode != http.StatusOK {
		b := string(body)
		if len(b) > 200 {
			b = b[:200]
		}
		return nil, fmt.Errorf("tekton results %d: %s", res.StatusCode, b)
	}
	return body, nil
}

// --- trimming: exactly what Tower's toPipelineRunSummary/toTaskRunSummary read ---

var keepAnnotation = []string{"pipelinesascode.tekton.dev/", "tekton.dev/", "triggers.tekton.dev/", "hangar.io/", "platform.io/"}

func m(v any) Obj {
	o, _ := v.(Obj)
	return o
}

// pick keeps the keys that are present, null included: Tower's TypeScript pick drops only
// undefined (absent), so a JSON null survives there and must survive here.
func pick(o Obj, keys ...string) Obj {
	out := Obj{}
	for _, k := range keys {
		if v, ok := o[k]; ok {
			out[k] = v
		}
	}
	return out
}

func trimMeta(meta Obj) Obj {
	ann := Obj{}
	for k, v := range m(meta["annotations"]) {
		for _, p := range keepAnnotation {
			if strings.HasPrefix(k, p) {
				ann[k] = v
				break
			}
		}
	}
	out := pick(meta, "name", "namespace", "uid", "creationTimestamp")
	labels := m(meta["labels"])
	if labels == nil {
		labels = Obj{}
	}
	out["labels"] = labels
	out["annotations"] = ann
	return out
}

func tasks(v any) []Obj {
	out := []Obj{}
	list, _ := v.([]any)
	for _, t := range list {
		out = append(out, pick(m(t), "name", "runAfter", "taskRef", "params", "when"))
	}
	return out
}

func TrimPipelineRun(pr Obj) Obj {
	st := m(pr["status"])
	status := pick(st, "conditions", "childReferences", "startTime", "completionTime", "results")
	ps := m(st["pipelineSpec"])
	status["pipelineSpec"] = Obj{"tasks": tasks(ps["tasks"]), "finally": tasks(ps["finally"])}
	spec := m(pr["spec"])
	out := Obj{"kind": "PipelineRun", "metadata": trimMeta(m(pr["metadata"])), "spec": pick(spec, "params", "pipelineRef"), "status": status}
	if v, ok := pr["apiVersion"]; ok {
		out["apiVersion"] = v
	}
	return out
}

func TrimTaskRun(tr Obj) Obj {
	st := m(tr["status"])
	status := pick(st, "conditions", "podName", "startTime", "completionTime", "results")
	steps := []Obj{}
	list, _ := st["steps"].([]any)
	for _, s := range list {
		steps = append(steps, pick(m(s), "name", "container", "terminated", "running", "waiting"))
	}
	status["steps"] = steps
	out := Obj{"kind": "TaskRun", "metadata": trimMeta(m(tr["metadata"])), "spec": pick(m(tr["spec"]), "params"), "status": status}
	if v, ok := tr["apiVersion"]; ok {
		out["apiVersion"] = v
	}
	return out
}

// --- Results shapes ---

type record struct {
	Name string `json:"name"`
	Data struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"data"`
}

type recordList struct {
	Records       []record `json:"records"`
	NextPageToken string   `json:"next_page_token"`
	NextPageAlt   string   `json:"nextPageToken"`
}

type summary struct {
	Name        string            `json:"name"`
	CreateTime  string            `json:"create_time"`
	Annotations map[string]string `json:"annotations"`
	Summary     *struct {
		Type        string            `json:"type"`
		StartTime   *string           `json:"start_time"`
		EndTime     *string           `json:"end_time"`
		Status      string            `json:"status"`
		Annotations map[string]string `json:"annotations"`
	} `json:"summary"`
}

type summaryList struct {
	Results       []summary `json:"results"`
	NextPageToken string    `json:"next_page_token"`
	NextPageAlt   string    `json:"nextPageToken"`
}

var statusMap = map[string]string{"SUCCESS": "succeeded", "FAILURE": "failed", "CANCELLED": "cancelled", "TIMEOUT": "failed"}

func toRow(s summary) (Row, bool) {
	if s.Summary == nil || s.Summary.Type != pipelineRun {
		return Row{}, false
	}
	parts := strings.Split(s.Name, "/")
	result := parts[len(parts)-1]
	st, ok := statusMap[s.Summary.Status]
	if !ok {
		st = "unknown"
	}
	at := func(m map[string]string, k string) *string {
		if v, ok := m[k]; ok {
			return &v
		}
		return nil
	}
	name := result // Tower: annotations['object.metadata.name'] ?? result - only an absent name falls back
	if v, ok := s.Annotations["object.metadata.name"]; ok {
		name = v
	}
	sa := s.Summary.Annotations
	return Row{Result: result, Name: name, Pipeline: at(s.Annotations, "tekton.dev/pipeline"), Status: st,
		StartTime: s.Summary.StartTime, EndTime: s.Summary.EndTime, Commit: at(sa, "commit"), EventType: at(sa, "eventType"), PRNumber: at(sa, "pull_request-id")}, true
}

func decode(r record) Obj {
	if r.Data.Value == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(r.Data.Value)
	if err != nil {
		return nil
	}
	var o Obj
	if json.Unmarshal(raw, &o) != nil {
		return nil
	}
	return o
}

func next(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// List returns finished runs of the app in namespace parent created since `since` (never before
// appStart), newest first, at most limit.
func (c *Client) List(ctx context.Context, parent string, since, appStart time.Time, limit int) ([]Row, error) {
	if appStart.After(since) {
		since = appStart
	}
	key := fmt.Sprintf("list:%s:%d:%d", parent, since.UnixMilli()/listTTL.Milliseconds(), limit)
	if v, ok := c.cache.get(key); ok {
		return v.([]Row), nil
	}
	rows := []Row{}
	token := ""
	for page := 0; page < maxSummary && len(rows) < limit; page++ {
		q := url.Values{"page_size": {fmt.Sprint(summaryPage)}, "order_by": {"create_time desc"}, "filter": {fmt.Sprintf(`summary.type=="%s"`, pipelineRun)}}
		if token != "" {
			q.Set("page_token", token)
		}
		body, err := c.get(ctx, "/parents/"+parent+"/results?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var res summaryList
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, err
		}
		older := false
		for _, s := range res.Results {
			if t, err := time.Parse(time.RFC3339Nano, s.CreateTime); err == nil && t.Before(since) {
				older = true
				break
			}
			if row, ok := toRow(s); ok {
				rows = append(rows, row)
			}
			if len(rows) >= limit {
				break
			}
		}
		token = next(res.NextPageToken, res.NextPageAlt)
		if older || token == "" {
			break
		}
	}
	c.cache.set(key, rows, listTTL)
	return rows, nil
}

func runKey(parent, result string) string { return "run:" + parent + ":" + result }

func (c *Client) cacheRun(parent string, r *Run) {
	ttl := listTTL
	if m(r.PipelineRun["status"])["completionTime"] != nil {
		ttl = runTTL // a finished run never changes
	}
	c.cache.set(runKey(parent, r.Result), r, ttl)
}

func (c *Client) cachedRun(ctx context.Context, parent, result string, needLogs bool) (*Run, error) {
	if !uid.MatchString(result) {
		return nil, Invalid{fmt.Sprintf("invalid result id '%s'", result)}
	}
	if v, ok := c.cache.get(runKey(parent, result)); ok {
		if r := v.(*Run); !needLogs || r.logs != nil {
			return r, nil
		}
	}
	var records []record
	token := ""
	for {
		q := url.Values{"page_size": {"100"}}
		if token != "" {
			q.Set("page_token", token)
		}
		body, err := c.get(ctx, "/parents/"+parent+"/results/"+result+"/records?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var res recordList
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, err
		}
		records = append(records, res.Records...)
		if token = next(res.NextPageToken, res.NextPageAlt); token == "" {
			break
		}
	}
	run := &Run{Result: result, TaskRuns: []Obj{}, logs: map[string]string{}}
	for _, r := range records {
		obj := decode(r)
		if obj == nil {
			continue
		}
		switch r.Data.Type {
		case pipelineRun:
			run.PipelineRun = TrimPipelineRun(obj)
		case taskRun:
			run.TaskRuns = append(run.TaskRuns, TrimTaskRun(obj))
		case logType:
			if owner, _ := m(m(obj["spec"])["resource"])["name"].(string); owner != "" {
				parts := strings.Split(r.Name, "/")
				run.logs[owner] = parts[len(parts)-1]
			}
		}
	}
	if run.PipelineRun == nil {
		return nil, NotFound{fmt.Sprintf("no PipelineRun record in result %s", result)}
	}
	c.cacheRun(parent, run)
	return run, nil
}

// Get returns one run, PipelineRun and TaskRuns.
func (c *Client) Get(ctx context.Context, parent, result string) (*Run, error) {
	return c.cachedRun(ctx, parent, result, false)
}

func (c *Client) bulk(ctx context.Context, parent string, since time.Time, limit int) ([]*Run, error) {
	filter := fmt.Sprintf(`data_type in ["%s","%s"] && data.metadata.creationTimestamp > timestamp("%s")`, pipelineRun, taskRun, since.UTC().Format("2006-01-02T15:04:05.000Z"))
	type entry struct {
		pr  Obj
		trs []Obj
	}
	order := []string{}
	runs := map[string]*entry{}
	prs := 0
	token := ""
	for page := 0; page < maxBulkPages && prs < limit; page++ {
		q := url.Values{"page_size": {fmt.Sprint(bulkPage)}, "order_by": {"create_time desc"}, "filter": {filter}}
		if token != "" {
			q.Set("page_token", token)
		}
		body, err := c.get(ctx, "/parents/"+parent+"/results/-/records?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var res recordList
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Records {
			obj := decode(r)
			if obj == nil {
				continue
			}
			id := strings.Split(r.Name, "/")[2]
			e, ok := runs[id]
			if !ok {
				e = &entry{trs: []Obj{}}
				runs[id] = e
				order = append(order, id)
			}
			switch r.Data.Type {
			case pipelineRun:
				e.pr = TrimPipelineRun(obj)
				prs++
			case taskRun:
				e.trs = append(e.trs, TrimTaskRun(obj))
			}
		}
		if token = next(res.NextPageToken, res.NextPageAlt); token == "" {
			break
		}
	}
	out := []*Run{}
	for _, id := range order {
		if e := runs[id]; e.pr != nil {
			r := &Run{Result: id, PipelineRun: e.pr, TaskRuns: e.trs}
			c.cacheRun(parent, r)
			out = append(out, r)
		}
	}
	return out, nil
}

// Runs returns every run since `since`, in full. Cached runs cost nothing; a few new ones are read
// one by one; a cold cache reads the window in one paged query.
func (c *Client) Runs(ctx context.Context, parent string, since, appStart time.Time, limit int) ([]*Run, error) {
	rows, err := c.List(ctx, parent, since, appStart, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*Run, len(rows))
	missing := []int{}
	for i, r := range rows {
		if v, ok := c.cache.get(runKey(parent, r.Result)); ok {
			out[i] = v.(*Run)
		} else {
			missing = append(missing, i)
		}
	}
	if len(missing) > bulkAbove {
		// Tower: Math.min over each missing run's start time, a missing or unparsable one counting as
		// the window start, so the bulk query never starts later than a run it has to cover.
		var oldest time.Time
		for n, i := range missing {
			t := since
			if rows[i].StartTime != nil {
				if p, err := time.Parse(time.RFC3339Nano, *rows[i].StartTime); err == nil {
					t = p
				}
			}
			if n == 0 || t.Before(oldest) {
				oldest = t
			}
		}
		from := oldest.Add(-time.Minute) // a PipelineRun's creationTimestamp can precede its summary's start time
		if from.Before(since) {
			from = since
		}
		got, err := c.bulk(ctx, parent, from, limit)
		if err != nil {
			return nil, err
		}
		by := map[string]*Run{}
		for _, r := range got {
			by[r.Result] = r
		}
		for _, i := range missing {
			out[i] = by[rows[i].Result]
		}
	} else {
		var wg sync.WaitGroup
		work := make(chan int)
		for w := 0; w < concurrency && w < len(missing); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					if r, err := c.cachedRun(ctx, parent, rows[i].Result, false); err == nil {
						out[i] = r // one unreadable run does not fail the list
					}
				}
			}()
		}
		for _, i := range missing {
			work <- i
		}
		close(work)
		wg.Wait()
	}
	res := []*Run{}
	for _, r := range out {
		if r != nil {
			res = append(res, r)
		}
	}
	return res, nil
}

// Log returns one TaskRun's archived step logs as plain text.
func (c *Client) Log(ctx context.Context, parent, result, taskRun string) (string, error) {
	run, err := c.cachedRun(ctx, parent, result, true)
	if err != nil {
		return "", err
	}
	rec := run.logs[taskRun]
	if rec == "" {
		return "", NoLog{"Tekton Results archived no logs for this task"}
	}
	if !uid.MatchString(rec) {
		return "", Invalid{"invalid log record id"}
	}
	key := "log:" + parent + ":" + result + ":" + rec
	if v, ok := c.cache.get(key); ok {
		return v.(string), nil
	}
	body, err := c.get(ctx, "/parents/"+parent+"/results/"+result+"/logs/"+rec)
	var nf NotFound
	if errors.As(err, &nf) {
		// The Log record exists but its blob was never stored (status.isStored false): the watcher
		// gave up on the upload (docs/admin/tekton-results.md). For the caller that is "no log".
		return "", NoLog{"Tekton Results has a log record for this task but never stored its log"}
	}
	if err != nil {
		return "", err
	}
	c.cache.set(key, string(body), logTTL)
	return string(body), nil
}
