package live

import "testing"

func run(spec map[string]any, cond map[string]any) Obj {
	o := Obj{"spec": spec}
	if cond != nil {
		o["status"] = map[string]any{"conditions": []any{cond}}
	}
	return o
}

// A run Pipelines-as-Code created beyond the per-repository concurrency limit is queued, not running; every other state is as before.
func TestStatusOfQueued(t *testing.T) {
	unknown := func(reason string) map[string]any {
		return map[string]any{"type": "Succeeded", "status": "Unknown", "reason": reason}
	}
	for name, c := range map[string]struct {
		o      Obj
		status string
		reason string
	}{
		"pending, reason PipelineRunPending":      {run(map[string]any{"status": "PipelineRunPending"}, unknown("PipelineRunPending")), "queued", "PipelineRunPending"},
		"reason alone":                            {run(map[string]any{}, unknown("PipelineRunPending")), "queued", "PipelineRunPending"},
		"spec pending, condition not yet set":     {run(map[string]any{"status": "PipelineRunPending"}, nil), "queued", "PipelineRunPending"},
		"spec pending, other unknown reason":      {run(map[string]any{"status": "PipelineRunPending"}, unknown("Started")), "queued", "Started"},
		"started: running":                        {run(map[string]any{}, unknown("Running")), "running", "Running"},
		"no condition, not pending: pending":      {run(map[string]any{}, nil), "pending", ""},
		"queued label alone is not queued":        {Obj{"metadata": map[string]any{"labels": map[string]any{"pipelinesascode.tekton.dev/state": "queued"}}, "spec": map[string]any{}, "status": map[string]any{"conditions": []any{unknown("Running")}}}, "running", "Running"},
		"succeeded":                               {run(map[string]any{}, map[string]any{"type": "Succeeded", "status": "True", "reason": "Succeeded"}), "succeeded", "Succeeded"},
		"failed":                                  {run(map[string]any{}, map[string]any{"type": "Succeeded", "status": "False", "reason": "Failed"}), "failed", "Failed"},
		"cancelled while pending stays cancelled": {run(map[string]any{"status": "Cancelled"}, map[string]any{"type": "Succeeded", "status": "False", "reason": "Cancelled"}), "cancelled", "Cancelled"},
	} {
		got, reason, _ := statusOf(c.o)
		if got != c.status || reason != c.reason {
			t.Errorf("%s: %s/%s, want %s/%s", name, got, reason, c.status, c.reason)
		}
	}
}
