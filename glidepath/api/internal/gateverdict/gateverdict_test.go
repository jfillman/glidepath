package gateverdict

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glidepath/api/internal/auth"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	ns    = "app-shop-cicd"
	run   = "sast-abcde"
	sha   = "0123456789abcdef0123456789abcdef01234567"
	key   = "gate.0123456789ab.sast"
	rkey  = "gateRun.0123456789ab.sast"
	prURL = "https://github.com/jfillman/gitops-shop/pull/12"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// fakeStore is the cluster: one gate run with its TaskRuns and the comment-pr pod, and the release records, with the API
// server's resourceVersion check on a patch.
type fakeStore struct {
	mu        sync.Mutex
	runs      map[string]map[string]any
	taskRuns  []map[string]any
	pods      map[string]*corev1.Pod
	records   map[string]*corev1.ConfigMap
	patches   int
	conflicts int // the next N patches lose the race: someone else writes the record first
	racer     map[string]string
}

func (f *fakeStore) PipelineRun(_ context.Context, n, name string) (map[string]any, error) {
	if r, ok := f.runs[name]; ok && n == ns {
		return r, nil
	}
	return nil, NotFound{"PipelineRun " + name}
}

func (f *fakeStore) TaskRun(_ context.Context, n, name string) (map[string]any, error) {
	for _, tr := range f.taskRuns {
		if str(tr, "metadata", "name") == name && n == ns {
			return tr, nil
		}
	}
	return nil, NotFound{"TaskRun " + name}
}

func (f *fakeStore) TaskRuns(_ context.Context, _ string, pr string) ([]map[string]any, error) {
	var out []map[string]any
	for _, tr := range f.taskRuns {
		if mapOf(tr, "metadata", "labels")[labelPipelineRun] == pr {
			out = append(out, tr)
		}
	}
	return out, nil
}

func (f *fakeStore) Pod(_ context.Context, n, name string) (*corev1.Pod, error) {
	if p, ok := f.pods[name]; ok && n == ns {
		return p, nil
	}
	return nil, NotFound{"pod " + name}
}

func (f *fakeStore) Records(_ context.Context, _ string) ([]corev1.ConfigMap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []corev1.ConfigMap
	for _, r := range f.records {
		c := r.DeepCopy()
		out = append(out, *c)
	}
	return out, nil
}

func (f *fakeStore) PatchRecord(_ context.Context, _ string, name, rv string, data map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.records[name]
	if f.conflicts > 0 {
		f.conflicts--
		for k, v := range f.racer {
			r.Data[k] = v
		}
		r.ResourceVersion += "x"
	}
	if r.ResourceVersion != rv {
		return ErrConflict
	}
	f.patches++
	for k, v := range data {
		r.Data[k] = v
	}
	r.ResourceVersion += "1"
	return nil
}

func taskRun(name, task, status string) map[string]any {
	conds := []any{}
	if status != "" {
		conds = append(conds, map[string]any{"type": "Succeeded", "status": status})
	}
	return map[string]any{
		"metadata": map[string]any{"name": run + "-" + name, "labels": map[string]any{labelPipelineRun: run, labelPipelineTask: task}},
		"status":   map[string]any{"conditions": conds},
	}
}

// world is a sast-check run of PR 12 whose scan succeeded, its comment-pr TaskRun and pod, and one release record for the PR.
func world() *fakeStore {
	pr := map[string]any{
		"metadata": map[string]any{
			"name": run, "uid": "run-uid", "creationTimestamp": "2026-11-20T11:50:00Z",
			"labels":      map[string]any{pacPullRequest: "12", pacSha: sha},
			"annotations": map[string]any{pacSha: sha, pacRepoURL: "https://github.com/jfillman/gitops-shop"},
		},
		"spec": map[string]any{"timeouts": map[string]any{"tasks": "14m0s", "finally": "4m0s", "pipeline": "18m0s"}},
		"status": map[string]any{
			"startTime":    "2026-11-20T11:50:00Z",
			"pipelineSpec": map[string]any{"tasks": []any{map[string]any{"name": "fetch"}, map[string]any{"name": "scan"}}, "finally": []any{map[string]any{"name": "comment-pr"}}},
		},
	}
	comment := taskRun("comment-pr", "comment-pr", "")
	comment["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"kind": "PipelineRun", "name": run, "uid": "run-uid"}}
	comment["spec"] = map[string]any{"params": []any{
		map[string]any{"name": "gate-name", "value": "sast"},
		map[string]any{"name": "status", "value": "Failed"}, // never read
	}}
	comment["status"].(map[string]any)["podName"] = run + "-comment-pr-pod"
	return &fakeStore{
		runs:     map[string]map[string]any{run: pr},
		taskRuns: []map[string]any{taskRun("fetch", "fetch", "True"), taskRun("scan", "scan", "True"), comment},
		pods: map[string]*corev1.Pod{run + "-comment-pr-pod": {ObjectMeta: metav1.ObjectMeta{Name: run + "-comment-pr-pod", UID: types.UID("pod-uid"),
			Labels: map[string]string{labelPipelineRun: run, labelPipelineTask: "comment-pr", labelTask: "comment-pr-check-result",
				labelMemberOf: "finally", labelTaskRun: run + "-comment-pr"}}}},
		records: map[string]*corev1.ConfigMap{
			"release-tracking-a": {ObjectMeta: metav1.ObjectMeta{Name: "release-tracking-a", ResourceVersion: "1"},
				Data: map[string]string{"prUrl": prURL, "image": "ghcr.io/x/shop@sha256:1", "gate.0123456789ab.sbom": "success", "gate.ffffffffffff.sast": "failure"}},
			"release-tracking-b": {ObjectMeta: metav1.ObjectMeta{Name: "release-tracking-b", ResourceVersion: "1"},
				Data: map[string]string{"prUrl": "https://github.com/jfillman/gitops-shop/pull/13"}},
		},
	}
}

var runner = &auth.Caller{User: "system:serviceaccount:" + ns + ":pipeline-runner", PodName: run + "-comment-pr-pod", PodUID: "pod-uid"}

func svc(f *fakeStore) *Service {
	return &Service{Store: f, ServiceAccounts: []string{"pipeline-runner"}, Now: func() time.Time { return now }}
}

func TestRecordsTheDerivedVerdict(t *testing.T) {
	f := world()
	a, err := svc(f).Record(context.Background(), runner, ns, run, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Outcome != Recorded || a.Verdict != "success" || a.Gate != "sast" || a.PullRequest != 12 || a.PrURL != prURL || a.Record != "release-tracking-a" || a.Key != key {
		t.Fatalf("%+v", a)
	}
	d := f.records["release-tracking-a"].Data
	if d[key] != "success" || d[rkey] != "2026-11-20T11:50:00Z "+run || f.patches != 1 {
		t.Fatalf("the verdict and the run that wrote it, in one patch: %v (%d patches)", d, f.patches)
	}
	if len(a.Ledger) != 2 || a.Ledger["gate.0123456789ab.sbom"] != "success" || a.Ledger[key] != "success" {
		t.Fatalf("the ledger answer is this commit's gate.<sha12>.* entries only: %v", a.Ledger)
	}
	if len(f.records["release-tracking-b"].Data) != 1 {
		t.Fatalf("another PR's record is untouched")
	}
}

func TestVerdictRule(t *testing.T) {
	cases := []struct {
		name  string
		tasks func(f *fakeStore)
		want  string
	}{
		{"all succeeded", func(*fakeStore) {}, "success"},
		{"a task failed", func(f *fakeStore) { f.taskRuns[1] = taskRun("scan", "scan", "False") }, "failure"},
		{"only an -advisory task failed", func(f *fakeStore) {
			f.taskRuns[1] = taskRun("scan-advisory", "scan-advisory", "False")
			f.runs[run]["status"].(map[string]any)["pipelineSpec"].(map[string]any)["tasks"] = []any{map[string]any{"name": "fetch"}, map[string]any{"name": "scan-advisory"}}
		}, "success"},
		{"a task still running (cancelled or timed out)", func(f *fakeStore) { f.taskRuns[1] = taskRun("scan", "scan", "Unknown") }, "failure"},
		{"a task never ran and was not skipped", func(f *fakeStore) { f.taskRuns = append(f.taskRuns[:1], f.taskRuns[2]) }, "failure"},
		{"a task skipped by its when", func(f *fakeStore) {
			f.taskRuns = append(f.taskRuns[:1], f.taskRuns[2])
			f.runs[run]["status"].(map[string]any)["skippedTasks"] = []any{map[string]any{"name": "scan", "reason": "When Expressions evaluated to false"}}
		}, "success"},
		{"another run's failed TaskRun does not count", func(f *fakeStore) {
			o := taskRun("scan", "scan", "False")
			o["metadata"].(map[string]any)["labels"].(map[string]any)[labelPipelineRun] = "other"
			f.taskRuns = append(f.taskRuns, o)
		}, "success"},
		{"past the tasks timeout", func(f *fakeStore) { f.runs[run]["status"].(map[string]any)["startTime"] = "2026-11-20T11:40:00Z" }, "failure"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := world()
			c.tasks(f)
			a, err := svc(f).Record(context.Background(), runner, ns, run, false)
			if err != nil {
				t.Fatal(err)
			}
			if a.Verdict != c.want || f.records["release-tracking-a"].Data[key] != c.want {
				t.Fatalf("got %s (%s), recorded %q", a.Verdict, a.Reason, f.records["release-tracking-a"].Data[key])
			}
		})
	}
}

func TestDeferredNearTheTimeout(t *testing.T) {
	f := world()
	f.runs[run]["status"].(map[string]any)["startTime"] = "2026-11-20T11:46:30Z" // 30 s left of 14 m
	a, err := svc(f).Record(context.Background(), runner, ns, run, false)
	if err != nil || a.Outcome != Deferred || f.patches != 0 {
		t.Fatalf("within a minute of the timeout nothing is recorded: %+v %v", a, err)
	}
}

func TestOneWritePerPipelineRun(t *testing.T) {
	f := world()
	s := svc(f)
	if _, err := s.Record(context.Background(), runner, ns, run, false); err != nil {
		t.Fatal(err)
	}
	// The run fails now (it cannot, but a replay must not re-derive): the second call writes nothing.
	f.taskRuns[1] = taskRun("scan", "scan", "False")
	a, err := s.Record(context.Background(), runner, ns, run, false)
	if err != nil || a.Outcome != AlreadyRecorded || a.Verdict != "success" || f.patches != 1 {
		t.Fatalf("a second call of the same run: %+v %v (%d patches)", a, err, f.patches)
	}
}

func TestAnOlderRunDoesNotOverwriteANewerOne(t *testing.T) {
	f := world()
	f.records["release-tracking-a"].Data[key] = "success"
	f.records["release-tracking-a"].Data[rkey] = "2026-11-20T11:55:00Z sast-newer"
	f.taskRuns[1] = taskRun("scan", "scan", "False")
	a, err := svc(f).Record(context.Background(), runner, ns, run, false)
	if err != nil || a.Outcome != Superseded || f.patches != 0 || f.records["release-tracking-a"].Data[key] != "success" {
		t.Fatalf("%+v %v", a, err)
	}
	// An older run's entry, and the old direct path's (no gateRun key), are overwritten.
	for _, prev := range []string{"2026-11-20T11:00:00Z sast-older", ""} {
		f := world()
		f.records["release-tracking-a"].Data[key] = "failure"
		if prev != "" {
			f.records["release-tracking-a"].Data[rkey] = prev
		}
		a, err := svc(f).Record(context.Background(), runner, ns, run, false)
		if err != nil || a.Outcome != Recorded || f.records["release-tracking-a"].Data[key] != "success" {
			t.Fatalf("prev %q: %+v %v", prev, a, err)
		}
	}
}

func TestConflictIsRetriedAndSeesTheOtherGate(t *testing.T) {
	f := world()
	f.conflicts = 2
	f.racer = map[string]string{"gate.0123456789ab.image-scan": "success"}
	a, err := svc(f).Record(context.Background(), runner, ns, run, false)
	if err != nil || a.Outcome != Recorded || a.Ledger["gate.0123456789ab.image-scan"] != "success" || a.Ledger[key] != "success" {
		t.Fatalf("the gate that finished at the same time is in the answer: %+v %v", a, err)
	}
	f = world()
	f.conflicts = 10
	f.racer = map[string]string{}
	if _, err := svc(f).Record(context.Background(), runner, ns, run, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("five lost races is an error: %v", err)
	}
}

func TestNoRecordAndDryRun(t *testing.T) {
	f := world()
	delete(f.records, "release-tracking-a")
	a, err := svc(f).Record(context.Background(), runner, ns, run, false)
	if err != nil || a.Outcome != NoRecord || f.patches != 0 {
		t.Fatalf("%+v %v", a, err)
	}
	f = world()
	a, err = svc(f).Record(context.Background(), runner, ns, run, true)
	if err != nil || a.Outcome != Recorded || !a.DryRun || f.patches != 0 || a.Ledger[key] != "success" {
		t.Fatalf("a dry run answers and writes nothing: %+v %v", a, err)
	}
}

func TestOnlyTheRunsOwnCommentPRPod(t *testing.T) {
	cases := []struct {
		name   string
		caller auth.Caller
		change func(f *fakeStore)
		want   string
	}{
		{"another account of the namespace", auth.Caller{User: "system:serviceaccount:" + ns + ":release-record-writer", PodName: run + "-comment-pr-pod", PodUID: "pod-uid"}, nil, "may not record"},
		{"another app's runner", auth.Caller{User: "system:serviceaccount:app-cart-cicd:pipeline-runner", PodName: run + "-comment-pr-pod", PodUID: "pod-uid"}, nil, "may not record"},
		{"a person", auth.Caller{User: "people:jfillman"}, nil, "may not record"},
		{"no pod claims", auth.Caller{User: runner.User}, nil, "not bound to a pod"},
		{"a pod that is gone", auth.Caller{User: runner.User, PodName: "gone", PodUID: "x"}, nil, "does not exist"},
		{"a recreated pod of the same name", auth.Caller{User: runner.User, PodName: runner.PodName, PodUID: "other-uid"}, nil, "not the pod it was minted for"},
		{"a pod of another run", *runner, func(f *fakeStore) { f.pods[runner.PodName].Labels[labelPipelineRun] = "other-run" }, "belongs to run"},
		{"another task of the run (notify-slack)", *runner, func(f *fakeStore) { f.pods[runner.PodName].Labels[labelPipelineTask] = "notify-slack" }, "not the finally task"},
		{"a DAG task named comment-pr", *runner, func(f *fakeStore) { delete(f.pods[runner.PodName].Labels, labelMemberOf) }, "not the finally task"},
		{"a TaskRun owned by another run", *runner, func(f *fakeStore) {
			f.taskRuns[2]["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"kind": "PipelineRun", "name": run, "uid": "older-run-uid"}}
		}, "is not this run's"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := world()
			if c.change != nil {
				c.change(f)
			}
			cl := c.caller
			_, err := svc(f).Record(context.Background(), &cl, ns, run, false)
			var r Refused
			if !errors.As(err, &r) || !strings.Contains(err.Error(), c.want) || f.patches != 0 {
				t.Fatalf("got %v, want Refused containing %q", err, c.want)
			}
		})
	}
}

func TestNotAGateRunOfAPullRequest(t *testing.T) {
	cases := map[string]func(f *fakeStore){
		"no pull request label": func(f *fakeStore) {
			delete(f.runs[run]["metadata"].(map[string]any)["labels"].(map[string]any), pacPullRequest)
		},
		"not a gitops repo": func(f *fakeStore) {
			f.runs[run]["metadata"].(map[string]any)["annotations"].(map[string]any)[pacRepoURL] = "https://github.com/jfillman/shop"
		},
		"no commit": func(f *fakeStore) {
			delete(f.runs[run]["metadata"].(map[string]any)["annotations"].(map[string]any), pacSha)
			delete(f.runs[run]["metadata"].(map[string]any)["labels"].(map[string]any), pacSha)
		},
		"a gate name that is not one": func(f *fakeStore) {
			f.taskRuns[2]["spec"] = map[string]any{"params": []any{map[string]any{"name": "gate-name", "value": "$(params.gate-name)"}}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := world()
			change(f)
			_, err := svc(f).Record(context.Background(), runner, ns, run, false)
			var ng NotGate
			if !errors.As(err, &ng) || f.patches != 0 {
				t.Fatalf("got %v", err)
			}
		})
	}
	if _, err := svc(world()).Record(context.Background(), runner, ns, "no-such-run", false); err == nil {
		t.Fatal("a run that does not exist")
	}
}
