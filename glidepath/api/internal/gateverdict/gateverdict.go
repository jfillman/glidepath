// Package gateverdict records one gate's verdict for one commit in the release record's gate ledger, for
// POST /v1/apps/{app}/pipelineruns/{name}/gate-verdict (docs/admin/api.md, "Gate verdicts"). Pipeline trust T4, W-8: the write
// moves out of the gate's pod (comment-pr-check-result, a finally task, running as pipeline-runner) into glidepath-api, which
// makes it under its own account, so no task account needs to write a release record for a gate.
//
// The request carries nothing that is believed. Who asks is the projected token's pod: it must be the comment-pr pod of that
// same PipelineRun, running as one of the allowed task accounts of that app's -cicd namespace. What is recorded is read from
// objects the Tekton controller and Pipelines-as-Code wrote:
//
//   - pull request: the PipelineRun's label pipelinesascode.tekton.dev/pull-request;
//   - commit: its annotation pipelinesascode.tekton.dev/sha; the pull request URL from its repo-url annotation;
//   - gate: the gate-name parameter of the caller's own comment-pr TaskRun (what the Pipeline passed it);
//   - verdict: the Pipeline's DAG tasks and their TaskRuns, with comment-pr's rule: success when every DAG task is done and
//     every failed one is an -advisory task (ADR-0021); a success past the tasks timeout is a failure, and within a minute of
//     it nothing is recorded (the same rule as the promotion-ready step).
//
// One write per PipelineRun: beside gate.<sha12>.<gate> the same patch writes gateRun.<sha12>.<gate> = "<created> <run>". A
// second call for the same run writes nothing; a run older than the one that wrote last writes nothing (a slow old run cannot
// overwrite a /retest that finished first). The patch carries the record's resourceVersion and is retried on a conflict, so two
// gates finishing together are serialized.
package gateverdict

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/glidepath/api/internal/auth"

	corev1 "k8s.io/api/core/v1"
)

// Outcomes.
const (
	Recorded        = "recorded"
	AlreadyRecorded = "already-recorded"
	Superseded      = "superseded"
	Deferred        = "deferred"
	NoRecord        = "no-record"
)

// Audience is the projected token's audience the route accepts (and nothing else does).
const Audience = "glidepath-gate-verdict"

// The labels Tekton puts on a TaskRun and its pod, and the ones Pipelines-as-Code puts on the PipelineRun.
const (
	labelPipelineRun  = "tekton.dev/pipelineRun"
	labelPipelineTask = "tekton.dev/pipelineTask"
	labelTask         = "tekton.dev/task"
	labelMemberOf     = "tekton.dev/memberOf"
	labelTaskRun      = "tekton.dev/taskRun"
	pacPullRequest    = "pipelinesascode.tekton.dev/pull-request"
	pacSha            = "pipelinesascode.tekton.dev/sha"
	pacRepoURL        = "pipelinesascode.tekton.dev/repo-url"
	recordLabel       = "hangar.io/subcomponent=release-tracking"
	// The pipeline task and Task that may ask (every gate Pipeline's finally names it comment-pr).
	CallerPipelineTask = "comment-pr"
	CallerTask         = "comment-pr-check-result"
	maxAttempts        = 5
	timeoutMargin      = 60 * time.Second
)

var (
	gateRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	shaRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repoRE   = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+)/(gitops-[a-z0-9][a-z0-9-]*)(\.git)?/?$`)
	numberRE = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
)

// ErrConflict is a patch whose resourceVersion is no longer the record's.
var ErrConflict = errors.New("the release record changed underneath")

// NotFound: no such PipelineRun.
type NotFound struct{ What string }

func (e NotFound) Error() string { return e.What }

// Refused: the caller is not the comment-pr pod of this run (403).
type Refused struct{ What string }

func (e Refused) Error() string { return e.What }

// NotGate: the run is not a gate run of a pull request, so there is nothing to record (422).
type NotGate struct{ What string }

func (e NotGate) Error() string { return e.What }

// Store is the slice of the cluster the service reads and writes.
type Store interface {
	PipelineRun(ctx context.Context, ns, name string) (map[string]any, error) // NotFound when absent
	TaskRun(ctx context.Context, ns, name string) (map[string]any, error)     // NotFound when absent
	TaskRuns(ctx context.Context, ns, pipelineRun string) ([]map[string]any, error)
	Pod(ctx context.Context, ns, name string) (*corev1.Pod, error) // NotFound when absent
	Records(ctx context.Context, ns string) ([]corev1.ConfigMap, error)
	// PatchRecord merges data into the ConfigMap if its resourceVersion is still rv (ErrConflict otherwise).
	PatchRecord(ctx context.Context, ns, name, rv string, data map[string]string) error
}

// Service answers the route.
type Service struct {
	Store Store
	// ServiceAccounts are the task accounts (names in the app's own -cicd namespace) that may ask.
	ServiceAccounts []string
	Now             func() time.Time
}

// Answer is what the route returns.
type Answer struct {
	Outcome     string            `json:"outcome"`
	Gate        string            `json:"gate"`
	Sha         string            `json:"sha"`
	PullRequest int               `json:"pullRequest"`
	PrURL       string            `json:"prUrl"`
	Verdict     string            `json:"verdict,omitempty"`
	Record      string            `json:"record,omitempty"`
	Key         string            `json:"key"`
	Reason      string            `json:"reason,omitempty"`
	DryRun      bool              `json:"dryRun,omitempty"`
	Ledger      map[string]string `json:"ledger"`
}

// Record derives the verdict of PipelineRun name in namespace ns for caller c and writes it (unless dry).
func (s *Service) Record(ctx context.Context, c *auth.Caller, ns, name string, dry bool) (*Answer, error) {
	tr, run, err := s.authorize(ctx, c, ns, name)
	if err != nil {
		return nil, err
	}
	a, err := derive(run, tr)
	if err != nil {
		return nil, err
	}
	trs, err := s.Store.TaskRuns(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	a.Verdict, a.Reason = verdict(run, trs)
	deferred := ""
	if a.Verdict == "success" {
		if left, ok := tasksLeft(run, s.Now()); ok {
			switch {
			case left < 0:
				a.Verdict, a.Reason = "failure", fmt.Sprintf("the run is past its tasks timeout by %ds: Tekton ends it timed out", int(-left.Seconds()))
			case left < timeoutMargin:
				// Not returned yet: a run that already recorded answers already-recorded (a retry near the deadline), not deferred.
				deferred = fmt.Sprintf("the run is %ds from its tasks timeout and may end timed out: nothing recorded", int(left.Seconds()))
			}
		}
	}
	a.DryRun = dry
	return a, s.write(ctx, ns, name, stamp(run, name), a, dry, deferred)
}

// authorize returns the caller's own comment-pr TaskRun and the PipelineRun, or Refused.
func (s *Service) authorize(ctx context.Context, c *auth.Caller, ns, name string) (map[string]any, map[string]any, error) {
	ok := false
	for _, sa := range s.ServiceAccounts {
		ok = ok || c.User == "system:serviceaccount:"+ns+":"+sa
	}
	if !ok {
		return nil, nil, Refused{fmt.Sprintf("%s may not record a gate verdict in %s: only %s of that namespace may (the comment-pr task of the gate's own run)",
			c.User, ns, strings.Join(s.ServiceAccounts, ", "))}
	}
	if c.PodName == "" || c.PodUID == "" {
		return nil, nil, Refused{"the token is not bound to a pod: send the projected token the comment-pr-check-result Task mounts"}
	}
	pod, err := s.Store.Pod(ctx, ns, c.PodName)
	var nf NotFound
	if errors.As(err, &nf) {
		return nil, nil, Refused{"the token's pod " + c.PodName + " does not exist"}
	}
	if err != nil {
		return nil, nil, err
	}
	l := pod.Labels
	switch {
	case string(pod.UID) != c.PodUID:
		return nil, nil, Refused{"the token's pod " + c.PodName + " is not the pod it was minted for"}
	case l[labelPipelineRun] != name:
		return nil, nil, Refused{fmt.Sprintf("the token's pod %s belongs to run %q, not %s: a run's verdict is asked for by its own comment-pr task", c.PodName, l[labelPipelineRun], name)}
	case l[labelPipelineTask] != CallerPipelineTask || l[labelTask] != CallerTask || l[labelMemberOf] != "finally" || l[labelTaskRun] == "":
		return nil, nil, Refused{fmt.Sprintf("the token's pod %s is the %q task, not the finally task %s (%s)", c.PodName, l[labelPipelineTask], CallerPipelineTask, CallerTask)}
	}
	tr, err := s.Store.TaskRun(ctx, ns, l[labelTaskRun])
	if errors.As(err, &nf) {
		return nil, nil, Refused{"the token's TaskRun " + l[labelTaskRun] + " does not exist"}
	}
	if err != nil {
		return nil, nil, err
	}
	run, err := s.Store.PipelineRun(ctx, ns, name)
	if err != nil {
		return nil, nil, err
	}
	if !ownedBy(tr, name, str(run, "metadata", "uid")) || str(tr, "status", "podName") != pod.Name {
		return nil, nil, Refused{fmt.Sprintf("TaskRun %s is not this run's (%s) task of pod %s", l[labelTaskRun], name, pod.Name)}
	}
	return tr, run, nil
}

// derive reads the pull request, commit and gate.
func derive(run, tr map[string]any) (*Answer, error) {
	a := &Answer{Ledger: map[string]string{}}
	pr := mapOf(run, "metadata", "labels")[pacPullRequest]
	if !numberRE.MatchString(pr) {
		return nil, NotGate{"the run has no pull request (label " + pacPullRequest + "): only a gate run of a pull request has a verdict to record"}
	}
	a.PullRequest, _ = strconv.Atoi(pr)
	ann := mapOf(run, "metadata", "annotations")
	a.Sha = ann[pacSha]
	if a.Sha == "" {
		a.Sha = mapOf(run, "metadata", "labels")[pacSha]
	}
	if !shaRE.MatchString(a.Sha) {
		return nil, NotGate{"the run records no commit (" + pacSha + ")"}
	}
	m := repoRE.FindStringSubmatch(ann[pacRepoURL])
	if m == nil {
		return nil, NotGate{fmt.Sprintf("the run's repository %q is not a github.com gitops-<app> repository: not a release", ann[pacRepoURL])}
	}
	a.PrURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", m[1], m[2], a.PullRequest)
	for _, p := range list(tr, "spec", "params") {
		pm, _ := p.(map[string]any)
		if pm["name"] == "gate-name" {
			a.Gate, _ = pm["value"].(string)
		}
	}
	if !gateRE.MatchString(a.Gate) {
		return nil, NotGate{fmt.Sprintf("the comment-pr task's gate-name %q is not a gate name", a.Gate)}
	}
	a.Key = "gate." + a.Sha[:12] + "." + a.Gate
	return a, nil
}

// doneSkips are the status.skippedTasks reasons (Tekton's SkippingReason) of a task that was skipped by the Pipeline's own logic.
var doneSkips = map[string]bool{"When Expressions evaluated to false": true, "Parent Tasks were skipped": true}

// ownedBy: o has an owner reference to the PipelineRun name with uid.
func ownedBy(o map[string]any, name, uid string) bool {
	for _, r := range list(o, "metadata", "ownerReferences") {
		m, _ := r.(map[string]any)
		if m["kind"] == "PipelineRun" && m["name"] == name && m["uid"] == uid && uid != "" {
			return true
		}
	}
	return false
}

// verdict is comment-pr's rule, from the TaskRuns: success when every DAG task is done and the only failures are -advisory
// tasks; otherwise failure.
func verdict(run map[string]any, trs []map[string]any) (string, string) {
	skipped := map[string]bool{}
	for _, s := range list(run, "status", "skippedTasks") {
		if m, ok := s.(map[string]any); ok {
			n, _ := m["name"].(string)
			// Only a skip the Pipeline asked for counts as done: a when expression false, or a parent skipped for one. A skip
			// because the run was cancelled, stopped or timed out ("PipelineRun was gracefully cancelled", ...) is not finished.
			if r, _ := m["reason"].(string); doneSkips[r] {
				skipped[n] = true
			}
		}
	}
	state := map[string][]string{} // pipeline task -> the Succeeded condition status of each of its TaskRuns
	for _, tr := range trs {
		// A TaskRun of this run: the label and an owner reference to this run's uid (an earlier run of the same name, being
		// deleted or orphaned, keeps the label).
		if mapOf(tr, "metadata", "labels")[labelPipelineRun] != str(run, "metadata", "name") || !ownedBy(tr, str(run, "metadata", "name"), str(run, "metadata", "uid")) {
			continue
		}
		t := mapOf(tr, "metadata", "labels")[labelPipelineTask]
		st := "Unknown"
		for _, c := range list(tr, "status", "conditions") {
			if m, ok := c.(map[string]any); ok && m["type"] == "Succeeded" {
				st, _ = m["status"].(string)
			}
		}
		state[t] = append(state[t], st)
	}
	var tasks []string
	for _, t := range list(run, "status", "pipelineSpec", "tasks") {
		if m, ok := t.(map[string]any); ok {
			n, _ := m["name"].(string)
			tasks = append(tasks, n)
		}
	}
	if len(tasks) == 0 {
		return "failure", "the run's resolved Pipeline (status.pipelineSpec) lists no tasks"
	}
	var failed, advisory, pending []string
	for _, t := range tasks {
		sts := state[t]
		switch {
		case len(sts) == 0 && skipped[t]:
		case len(sts) == 0 || slices.Contains(sts, "Unknown") || slices.Contains(sts, ""):
			pending = append(pending, t)
		case slices.Contains(sts, "False") && strings.HasSuffix(t, "-advisory"):
			advisory = append(advisory, t)
		case slices.Contains(sts, "False"):
			failed = append(failed, t)
		}
	}
	switch {
	case len(failed) > 0:
		return "failure", "failed: " + strings.Join(failed, ", ")
	case len(pending) > 0:
		return "failure", "not finished (cancelled or timed out): " + strings.Join(pending, ", ")
	case len(advisory) > 0:
		return "success", "only advisory tasks failed: " + strings.Join(advisory, ", ")
	}
	return "success", ""
}

// tasksLeft is how long until startTime + timeouts.tasks (else timeouts.pipeline); false when the run has neither.
func tasksLeft(run map[string]any, now time.Time) (time.Duration, bool) {
	t := str(run, "spec", "timeouts", "tasks")
	if t == "" {
		t = str(run, "spec", "timeouts", "pipeline")
	}
	d, err := time.ParseDuration(t)
	if err != nil || d <= 0 {
		return 0, false
	}
	start, err := time.Parse(time.RFC3339, str(run, "status", "startTime"))
	if err != nil {
		return 0, false
	}
	return start.Add(d).Sub(now), true
}

// stamp is the gateRun value of this run: "<creationTimestamp> <name>".
func stamp(run map[string]any, name string) string {
	return str(run, "metadata", "creationTimestamp") + " " + name
}

// newer: stamp a was made by a run created after b's. creationTimestamp has one-second granularity and comes from the API
// server's clock: an equal (or unparseable) one is not newer, so the write proceeds (last writer wins, as the direct path does).
// The guard is best effort; promote re-checks every gate's check run (PROM-03), so the ledger never passes a red gate.
func newer(a, b string) bool {
	ta, _, okA := strings.Cut(a, " ")
	tb, _, okB := strings.Cut(b, " ")
	pa, errA := time.Parse(time.RFC3339, ta)
	pb, errB := time.Parse(time.RFC3339, tb)
	return okA && okB && errA == nil && errB == nil && pa.After(pb)
}

// write records a (unless dry). deferred non-empty: nothing is written; the answer is already-recorded or superseded when the record
// says so, else deferred with that reason.
func (s *Service) write(ctx context.Context, ns, name, mine string, a *Answer, dry bool, deferred string) error {
	runKey := "gateRun." + a.Sha[:12] + "." + a.Gate
	for attempt := 1; ; attempt++ {
		recs, err := s.Store.Records(ctx, ns)
		if err != nil {
			return err
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
		var rec *corev1.ConfigMap
		for i := range recs {
			if recs[i].Data["prUrl"] == a.PrURL {
				rec = &recs[i]
				break
			}
		}
		if rec == nil && deferred != "" {
			a.Outcome, a.Reason, a.Verdict = Deferred, deferred, ""
			return nil
		}
		if rec == nil {
			a.Outcome, a.Reason = NoRecord, "no release record has prUrl "+a.PrURL
			return nil
		}
		a.Record = rec.Name
		prev := rec.Data[runKey]
		_, prevRun, _ := strings.Cut(prev, " ")
		switch {
		case prevRun == name:
			a.Outcome, a.Verdict = AlreadyRecorded, rec.Data[a.Key]
			a.Ledger = ledger(rec.Data, a.Sha)
			return nil
		case newer(prev, mine):
			a.Outcome, a.Reason = Superseded, "a later run of this gate on this commit ("+prevRun+") already recorded "+rec.Data[a.Key]
			a.Ledger = ledger(rec.Data, a.Sha)
			return nil
		}
		if deferred != "" {
			a.Outcome, a.Reason, a.Verdict = Deferred, deferred, ""
			return nil
		}
		data := map[string]string{a.Key: a.Verdict, runKey: mine}
		after := map[string]string{}
		for k, v := range rec.Data {
			after[k] = v
		}
		for k, v := range data {
			after[k] = v
		}
		if dry {
			a.Outcome, a.Ledger = Recorded, ledger(after, a.Sha)
			return nil
		}
		err = s.Store.PatchRecord(ctx, ns, rec.Name, rec.ResourceVersion, data)
		if err == nil {
			a.Outcome, a.Ledger = Recorded, ledger(after, a.Sha)
			return nil
		}
		if !errors.Is(err, ErrConflict) || attempt == maxAttempts {
			return err
		}
	}
}

// ledger is the gate.<sha12>.* entries of data.
func ledger(data map[string]string, sha string) map[string]string {
	p := "gate." + sha[:12] + "."
	out := map[string]string{}
	for k, v := range data {
		if strings.HasPrefix(k, p) {
			out[k] = v
		}
	}
	return out
}

func walk(o map[string]any, path ...string) any {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func str(o map[string]any, path ...string) string {
	s, _ := walk(o, path...).(string)
	return s
}

func list(o map[string]any, path ...string) []any {
	l, _ := walk(o, path...).([]any)
	return l
}

func mapOf(o map[string]any, path ...string) map[string]string {
	out := map[string]string{}
	m, _ := walk(o, path...).(map[string]any)
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
