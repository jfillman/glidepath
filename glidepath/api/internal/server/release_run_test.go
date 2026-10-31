package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glidepath/api/internal/live"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/glidepath/api/internal/releases"
)

const chain = "7200a685-2b64-49ff-8587-686bb992d878"

// withReleaseRun is withLive plus a promote run of shop to prod: its preflight minted the chain id, open-release-pr
// has (or hasn't yet) opened the PR.
func withReleaseRun(s *Server, prOpened bool) *fakeLive {
	f := withLive(s)
	pr := liveObj("PipelineRun", "app-shop-cicd", "promote-prod-abc1234-0a1b2c",
		map[string]any{"hangar.io/app": "shop", "hangar.io/stage": "release", "tekton.dev/pipeline": "release"},
		map[string]any{"startTime": "2026-10-31T10:00:00Z", "conditions": []any{
			map[string]any{"type": "Succeeded", "status": "Unknown", "reason": "Running"}}})
	pr["spec"] = map[string]any{"params": []any{
		map[string]any{"name": "env", "value": "prod"}, map[string]any{"name": "cluster", "value": "hangar-prod"},
		map[string]any{"name": "initiator", "value": "user:default/jfillman"}}}
	task := func(name, start string, status map[string]any, res ...string) map[string]any {
		st := map[string]any{"startTime": start}
		for k, v := range status {
			st[k] = v
		}
		var rs []any
		for i := 0; i+1 < len(res); i += 2 {
			rs = append(rs, map[string]any{"name": res[i], "value": res[i+1]})
		}
		if rs != nil {
			st["results"] = rs
		}
		return liveObj("TaskRun", "app-shop-cicd", "promote-prod-abc1234-0a1b2c-"+name,
			map[string]any{live.LabelPipelineRun: "promote-prod-abc1234-0a1b2c", "tekton.dev/pipelineTask": name}, st)
	}
	done := map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": "True", "reason": "Succeeded"}}}
	running := map[string]any{"conditions": []any{map[string]any{"type": "Succeeded", "status": "Unknown", "reason": "Running"}}}
	f.objs["pipelineruns"] = append(f.objs["pipelineruns"], pr)
	f.objs["taskruns"] = append(f.objs["taskruns"],
		task("preflight", "2026-10-31T10:00:05Z", done, "chain-id", chain),
		task("gates", "2026-10-31T10:01:00Z", done))
	if prOpened {
		f.objs["taskruns"] = append(f.objs["taskruns"], task("open-release-pr", "2026-10-31T10:05:00Z", done,
			"pr-url", "https://github.com/jfillman/gitops-shop/pull/9", "change-request-number", "CHG0010099"))
	} else {
		f.objs["taskruns"] = append(f.objs["taskruns"], task("open-release-pr", "2026-10-31T10:05:00Z", running))
	}
	return f
}

func getJSON(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	code, out, _ := call(t, s, path, map[string]string{"Authorization": "Bearer t-tower"})
	return code, out
}

func TestReleaseRunBeforeThePR(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	withReleaseRun(s, false)
	code, out := getJSON(t, s, "/v1/apps/shop/releases/runs/promote-prod-abc1234-0a1b2c")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	if out["phase"] != "running" || out["status"] != "running" || out["chainId"] != chain || out["prUrl"] != nil || out["record"] != nil {
		t.Fatalf("%v", out)
	}
	if out["env"] != "prod" || out["cluster"] != "hangar-prod" || out["kind"] != "release" || out["initiator"] != "user:default/jfillman" {
		t.Fatalf("%v", out)
	}
	tasks := out["tasks"].([]any)
	got := []string{}
	for _, tk := range tasks {
		m := tk.(map[string]any)
		got = append(got, m["name"].(string)+":"+m["status"].(string))
	}
	if strings.Join(got, ",") != "preflight:succeeded,gates:succeeded,open-release-pr:running" {
		t.Fatalf("tasks %v", got)
	}
}

func TestReleaseRunWithItsRecord(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	withReleaseRun(s, true)
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: releases.Prefix + chain, Namespace: "app-shop-cicd",
			Labels: map[string]string{"hangar.io/subcomponent": "release-tracking", "hangar.io/app": "shop"}},
		Data: map[string]string{"state": "proposed", "prUrl": "https://github.com/jfillman/gitops-shop/pull/9"}})
	s.Releases = releases.Reader{CS: cs}
	code, out := getJSON(t, s, "/v1/apps/shop/releases/runs/promote-prod-abc1234-0a1b2c")
	if code != http.StatusOK || out["phase"] != "proposed" || out["prUrl"] != "https://github.com/jfillman/gitops-shop/pull/9" || out["changeRequest"] != "CHG0010099" {
		t.Fatalf("%d %v", code, out)
	}
	if rec := out["record"].(map[string]any); rec["name"] != releases.Prefix+chain {
		t.Fatalf("record %v", rec)
	}
}

func TestReleaseRunRefusals(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	withReleaseRun(s, false)
	for path, want := range map[string]string{
		"/v1/apps/shop/releases/runs/gone-run":    "GP-API-404",
		"/v1/apps/shop/releases/runs/shop-push-1": "GP-API-400", // not a release run
		"/v1/apps/shop/releases/runs/Bad_Name":    "GP-API-400",
	} {
		code, out := getJSON(t, s, path)
		if out["rule"] != want {
			t.Errorf("%s: %d %v", path, code, out)
		}
	}
	code, out, _ := call(t, s, "/v1/apps/shop/releases/runs/promote-prod-abc1234-0a1b2c", map[string]string{"Authorization": "Bearer t-nobody"})
	if code != http.StatusForbidden {
		t.Fatalf("%d %v", code, out)
	}
}

// sse reads server-sent events: each event's name and data.
type sse struct {
	sc *bufio.Scanner
}

func (r *sse) next(t *testing.T) (string, map[string]any) {
	t.Helper()
	event := ""
	for r.sc.Scan() {
		line := r.sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var v map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Fatal(err)
			}
			return event, v
		}
	}
	t.Fatalf("the stream ended: %v", r.sc.Err())
	return "", nil
}

func TestLiveStreamSnapshotThenChanges(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	f := withLive(s)
	s.Live.TTL = time.Millisecond
	s.LiveHub = &live.Hub{Client: s.Live, Interval: 20 * time.Millisecond}
	s.Heartbeat = time.Hour
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/apps/shop/live/stream", nil)
	req.Header.Set("Authorization", "Bearer t-tower")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	r := &sse{bufio.NewScanner(res.Body)}
	ev, snap := r.next(t)
	if ev != "snapshot" || len(snap["pipelineRuns"].([]any)) != 1 || len(snap["taskRuns"].([]any)) != 3 {
		t.Fatalf("%s %v", ev, snap)
	}
	if meta := snap["pipelineRuns"].([]any)[0].(map[string]any)["metadata"].(map[string]any); meta["managedFields"] != nil {
		t.Fatal("managedFields reached the stream")
	}
	if n, streams := s.LiveHub.Watching(); n != 1 || streams != 1 {
		t.Fatalf("watching %d namespaces, %d streams", n, streams)
	}

	// A TaskRun moves, a new one appears, one goes.
	f.mu.Lock()
	trs := f.objs["taskruns"]
	moved := liveObj("TaskRun", "app-shop-cicd", "shop-push-1-build", map[string]any{live.LabelPipelineRun: "shop-push-1"},
		map[string]any{"podName": "shop-push-1-build-pod", "completionTime": "2026-10-31T10:00:00Z"})
	moved["metadata"].(map[string]any)["resourceVersion"] = "8"
	keep := []map[string]any{moved}
	for _, o := range trs {
		n := o["metadata"].(map[string]any)["name"]
		if n != "shop-push-1-build" && n != "shop-push-1-scan" {
			keep = append(keep, o)
		}
	}
	keep = append(keep, liveObj("TaskRun", "app-shop-cicd", "shop-push-1-deploy", map[string]any{live.LabelPipelineRun: "shop-push-1"}, nil))
	f.objs["taskruns"] = keep
	f.mu.Unlock()

	seen := map[string]bool{}
	for len(seen) < 3 {
		ev, c := r.next(t)
		if ev != "change" {
			t.Fatalf("%s %v", ev, c)
		}
		seen[c["type"].(string)+" "+c["name"].(string)] = true
		if c["type"] == "delete" && c["object"] != nil {
			t.Fatalf("a delete carries no object: %v", c)
		}
	}
	for _, want := range []string{"upsert shop-push-1-build", "upsert shop-push-1-deploy", "delete shop-push-1-scan"} {
		if !seen[want] {
			t.Fatalf("missing %s in %v", want, seen)
		}
	}

	cancel()
	for i := 0; i < 100; i++ {
		if n, streams := s.LiveHub.Watching(); n == 0 && streams == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the watch did not stop with its last viewer")
}

func TestLiveStreamAuthorizesAndLimits(t *testing.T) {
	s, done := newServer(t, reader)
	defer done()
	withLive(s)
	s.LiveHub = &live.Hub{Client: s.Live, MaxStreams: 1, Interval: time.Hour}
	code, _, _ := call(t, s, "/v1/apps/shop/live/stream", map[string]string{"Authorization": "Bearer t-nobody"})
	if code != http.StatusForbidden {
		t.Fatalf("%d", code)
	}
	sub, _, err := s.LiveHub.Subscribe(context.Background(), "app-shop-cicd")
	if err != nil {
		t.Fatal(err)
	}
	defer s.LiveHub.Unsubscribe(sub)
	code, out, _ := call(t, s, "/v1/apps/shop/live/stream", map[string]string{"Authorization": "Bearer t-tower"})
	if code != http.StatusServiceUnavailable || out["rule"] != "GP-API-503" {
		t.Fatalf("%d %v", code, out)
	}
}
