package explain

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/glidepath/api/internal/results"
)

func contract(t *testing.T) ([]KnownFailure, int) {
	b, err := os.ReadFile("../../../../contract/glidepath-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	k, v, err := Load(b)
	if err != nil {
		t.Fatalf("every knownFailures pattern must compile in Go: %v", err)
	}
	if len(k) == 0 || v == 0 {
		t.Fatalf("contract has no knownFailures or version: %d %d", len(k), v)
	}
	return k, v
}

func taskRun(name, task, step, status, message string) results.Obj {
	return results.Obj{
		"metadata": results.Obj{"name": name, "labels": results.Obj{"tekton.dev/pipelineTask": task}},
		"status": results.Obj{
			"conditions": []any{results.Obj{"type": "Succeeded", "status": status, "reason": "Failed", "message": message}},
			"steps":      []any{results.Obj{"name": "prepare", "terminated": results.Obj{"exitCode": float64(0)}}, results.Obj{"name": step, "terminated": results.Obj{"exitCode": float64(1)}}},
		},
	}
}

func run(trs ...results.Obj) *results.Run {
	return &results.Run{Result: "r1", TaskRuns: trs, PipelineRun: results.Obj{
		"metadata": results.Obj{"name": "itsm-bh5bn"},
		"status":   results.Obj{"conditions": []any{results.Obj{"type": "Succeeded", "status": "False", "reason": "Failed"}}},
	}}
}

// A real archived log from hangar-dev (gate-api, 2026-10-28): an ITSM change that was never approved.
const itsmLog = `[prepare] 2026-10-28T00:34:45.794627343Z 2026/10/28 00:34:45 Entrypoint initialization

[verify] 2026-10-28T00:37:00.087431555Z warn: no change-tool credentials at /var/run/secrets/itsm - calling webhook unauthenticated
[verify] 2026-10-28T00:37:01.654182805Z error: change CHG0010035 for https://github.com/jfillman/gitops-gate-api/pull/53 is in state '-5' with approval 'not requested' - not approved or scheduled. Get it approved in webhook, then comment /retest itsm- on the PR.
`

func TestExplainsARealItsmFailure(t *testing.T) {
	known, v := contract(t)
	ex, err := Run(run(taskRun("itsm-bh5bn-verify-change", "verify-change", "verify", "False", `"step-verify" exited with code 1: Error`),
		taskRun("itsm-bh5bn-comment-pr", "comment-pr", "comment", "True", "")), known, v,
		func(string) (string, error) { return itsmLog, nil })
	if err != nil {
		t.Fatal(err)
	}
	if ex.Status != "failed" || len(ex.Failures) != 1 {
		t.Fatalf("one failed TaskRun, the succeeded one skipped: %+v", ex)
	}
	f := ex.Failures[0]
	if f.Known == nil || f.Known.ID != "ItsmChangeNotApproved" || f.Step != "verify" || f.Task != "verify-change" {
		t.Fatalf("%+v", f)
	}
	if !strings.HasPrefix(f.Evidence, "error: change CHG0010035") {
		t.Fatalf("evidence is the matching line without step prefix or timestamp: %q", f.Evidence)
	}
}

func TestUnmatchedFailureReturnsTheStepTail(t *testing.T) {
	known, v := contract(t)
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("[build] 2026-10-28T00:00:00.000000000Z line ")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\n[other] 2026-10-28T00:00:00.000000000Z no space left on device\n")
	}
	ex, _ := Run(run(taskRun("t", "build", "build", "False", "exit 1")), known, v, func(string) (string, error) { return b.String(), nil })
	f := ex.Failures[0]
	if f.Known != nil {
		t.Fatalf("another step's line must not classify this step's failure: %+v", f.Known)
	}
	if n := len(strings.Split(f.Evidence, "\n")); n != tailLines || strings.Contains(f.Evidence, "[build]") {
		t.Fatalf("want the last %d lines of the failing step, got %d: %q", tailLines, n, f.Evidence)
	}
}

func TestMissingLogStillClassifiesTheCondition(t *testing.T) {
	known, v := contract(t)
	ex, err := Run(run(taskRun("t", "deploy", "wait", "False", "timed out waiting for rollout gate-api to run 1.2.3 Healthy")), known, v,
		func(string) (string, error) { return "", results.NoLog{What: "none"} })
	if err != nil {
		t.Fatal(err)
	}
	f := ex.Failures[0]
	if f.Log != "missing" || f.Known == nil || f.Known.ID != "RolloutTimeout" {
		t.Fatalf("%+v", f)
	}
}

func TestSourceErrorIsNotAMiss(t *testing.T) {
	known, v := contract(t)
	boom := errors.New("results down")
	if _, err := Run(run(taskRun("t", "b", "s", "False", "")), known, v, func(string) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("a failed log read must fail the call, got %v", err)
	}
}

func TestBadPatternFailsLoad(t *testing.T) {
	if _, _, err := Load([]byte(`{"contractVersion":1,"knownFailures":[{"id":"X","match":"("}]}`)); err == nil {
		t.Fatal("an uncompilable pattern must fail")
	}
}

// Real failure lines from hangar-dev's archived runs (2026-10-27/28), one per knownFailure explain
// has been checked against. A pattern edit that stops matching its own evidence fails here.
func TestRealFailureLines(t *testing.T) {
	known, _ := contract(t)
	for line, want := range map[string]string{
		"error: change CHG0010035 for https://github.com/jfillman/gitops-gate-api/pull/53 is in state '-5' with approval 'not requested' - not approved or scheduled.": "ItsmChangeNotApproved",
		"error: timed out waiting for rollout gate-api in app-gate-api-dev to run ghcr.io/jfillman/gate-api:2.0.3-5d33ebc Healthy (last phase: Healthy)":               "RolloutTimeout",
		"fatal: repository 'https://github.com/jfillman/backstage.git/' not found":                                                                                     "SourceRepoNotFound",
		"TestWorkflowExecution 6ae04cdcc151f87ac364981d finished with status: failed":                                                                                  "TestsFailed",
		"image-scan: trivy found 89 HIGH/CRITICAL vulnerabilities":                                                                                                     "ImageVulnerabilities",
	} {
		if k, _ := Classify(known, []string{line}); k == nil || k.ID != want {
			t.Errorf("%q: got %v, want %s", line, k, want)
		}
	}
}
