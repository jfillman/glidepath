package live

// GET /v1/apps/{app}/releases/runs/{name} (follow-on 4, 2026-10-31): one release run, as the person who started it
// wants to follow it. Promote and rollback answer with the run's name at once; the PR the run opens comes minutes
// later (8 on the dev cluster the first time). This is the read that fills that gap: the run's status and tasks as
// the cluster holds them, the chain id its preflight task minted, the PR and change request once open-release-pr has
// them. The server joins the release record the chain id names.

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Gone: the run is not on the cluster (Tekton deletes a finished run after about an hour), or never was.
type Gone struct{ What string }

func (e Gone) Error() string { return e.What }

// RunTask is one task of the run.
type RunTask struct {
	Name           string `json:"name"` // the pipeline task
	TaskRun        string `json:"taskRun"`
	Status         string `json:"status"` // pending, running, succeeded, failed, cancelled
	StartTime      string `json:"startTime,omitempty"`
	CompletionTime string `json:"completionTime,omitempty"`
}

// ReleaseRun is a release run as the cluster holds it.
type ReleaseRun struct {
	Name           string    `json:"name"`
	Kind           string    `json:"kind"` // release, rollback (the run's release-kind)
	Env            string    `json:"env,omitempty"`
	Cluster        string    `json:"cluster,omitempty"`
	Initiator      string    `json:"initiator,omitempty"`
	Status         string    `json:"status"` // running, succeeded, failed, cancelled
	Reason         string    `json:"reason,omitempty"`
	Message        string    `json:"message,omitempty"`
	StartTime      string    `json:"startTime,omitempty"`
	CompletionTime string    `json:"completionTime,omitempty"`
	Tasks          []RunTask `json:"tasks"`
	ChainID        string    `json:"chainId,omitempty"`
	PRURL          string    `json:"prUrl,omitempty"`
	ChangeRequest  string    `json:"changeRequest,omitempty"`
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// statusOf reads Tekton's Succeeded condition: Unknown is running, True succeeded, False failed or cancelled.
func statusOf(o Obj) (status, reason, message string) {
	st := mapOf(o["status"])
	conds, _ := st["conditions"].([]any)
	for _, c := range conds {
		cm := mapOf(c)
		if str(cm, "type") != "Succeeded" {
			continue
		}
		reason, message = str(cm, "reason"), str(cm, "message")
		switch str(cm, "status") {
		case "True":
			return "succeeded", reason, message
		case "False":
			if reason == "Cancelled" || reason == "PipelineRunCancelled" || reason == "TaskRunCancelled" || reason == "StoppedRunFinally" || reason == "CancelledRunFinally" {
				return "cancelled", reason, message
			}
			return "failed", reason, message
		}
		return "running", reason, message
	}
	return "pending", "", ""
}

// results is a TaskRun's results by name, as strings.
func results(o Obj) map[string]string {
	out := map[string]string{}
	rs, _ := mapOf(o["status"])["results"].([]any)
	for _, r := range rs {
		rm := mapOf(r)
		if v, ok := rm["value"].(string); ok {
			out[str(rm, "name")] = v
		}
	}
	return out
}

func params(o Obj) map[string]string {
	out := map[string]string{}
	ps, _ := mapOf(o["spec"])["params"].([]any)
	for _, p := range ps {
		pm := mapOf(p)
		if v, ok := pm["value"].(string); ok {
			out[str(pm, "name")] = v
		}
	}
	return out
}

// ReleaseRun reads the release run name in namespace and its TaskRuns. Not cached: it is followed a run at a time.
func (c *Client) ReleaseRun(ctx context.Context, namespace, name string) (*ReleaseRun, error) {
	if !labelValue.MatchString(name) {
		return nil, Invalid{fmt.Sprintf("invalid PipelineRun name '%s'", name)}
	}
	pr, err := c.Store.Get(ctx, GVRPipelineRuns, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, Gone{fmt.Sprintf("no PipelineRun '%s' in %s: it finished more than about an hour ago and Tekton deleted it, or it never existed", name, namespace)}
	}
	if err != nil {
		return nil, err
	}
	meta := mapOf(pr["metadata"])
	lbl := mapOf(meta["labels"])
	if str(lbl, "hangar.io/stage") != "release" && str(lbl, "tekton.dev/pipeline") != "release" {
		return nil, Invalid{fmt.Sprintf("PipelineRun '%s' is not a release run", name)}
	}
	p := params(pr)
	out := &ReleaseRun{Name: name, Kind: "release", Env: p["env"], Cluster: p["cluster"], Initiator: p["initiator"], Tasks: []RunTask{}}
	if p["release-kind"] != "" {
		out.Kind = p["release-kind"]
	}
	out.Status, out.Reason, out.Message = statusOf(pr)
	st := mapOf(pr["status"])
	out.StartTime, out.CompletionTime = str(st, "startTime"), str(st, "completionTime")

	trs, err := c.Store.List(ctx, GVRTaskRuns, namespace, LabelPipelineRun+"="+name)
	if err != nil {
		return nil, err
	}
	for _, tr := range trs {
		tm := mapOf(tr["metadata"])
		task := str(mapOf(tm["labels"]), "tekton.dev/pipelineTask")
		ts, _, _ := statusOf(tr)
		trs := mapOf(tr["status"])
		out.Tasks = append(out.Tasks, RunTask{Name: task, TaskRun: str(tm, "name"), Status: ts,
			StartTime: str(trs, "startTime"), CompletionTime: str(trs, "completionTime")})
		r := results(tr)
		switch task {
		case "preflight":
			out.ChainID = r["chain-id"]
		case "open-release-pr":
			out.PRURL, out.ChangeRequest = r["pr-url"], r["change-request-number"]
		}
	}
	// In the order they started; not started yet last.
	sort.Slice(out.Tasks, func(i, j int) bool {
		a, b := out.Tasks[i], out.Tasks[j]
		if (a.StartTime == "") != (b.StartTime == "") {
			return b.StartTime == ""
		}
		if a.StartTime != b.StartTime {
			return a.StartTime < b.StartTime
		}
		return a.Name < b.Name
	})
	if out.ChainID == "" {
		out.ChainID = p["chain-id"]
	}
	return out, nil
}
