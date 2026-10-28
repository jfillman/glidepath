// Package explain classifies a failed run against the contract's knownFailures: for each failed
// TaskRun, the failing step, the known failure its log matches (first match in contract order
// wins, schemas/cicd.meta.yaml), the line that matched, and the hint and field to change.
//
// An agent or a person reads the id and the hint; neither should match on the message text, which
// can change. A failure no knownFailure matches comes back with no id and the step's last lines.
package explain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/glidepath/api/internal/results"
)

const tailLines = 15

// KnownFailure is one contract entry, compiled.
type KnownFailure struct {
	ID    string `json:"id"`
	Hint  string `json:"hint"`
	Field string `json:"field,omitempty"`
	re    *regexp.Regexp
}

// Load reads knownFailures and contractVersion from the contract. Every pattern must compile: a
// contract the API can't apply is a build error, not a silent miss.
func Load(contract []byte) ([]KnownFailure, int, error) {
	var c struct {
		Version       int `json:"contractVersion"`
		KnownFailures []struct {
			ID, Match, Hint, Field string
		} `json:"knownFailures"`
	}
	if err := json.Unmarshal(contract, &c); err != nil {
		return nil, 0, fmt.Errorf("contract: %w", err)
	}
	out := make([]KnownFailure, 0, len(c.KnownFailures))
	for _, k := range c.KnownFailures {
		re, err := regexp.Compile(k.Match)
		if err != nil {
			return nil, 0, fmt.Errorf("knownFailures %s: %w", k.ID, err)
		}
		out = append(out, KnownFailure{ID: k.ID, Hint: k.Hint, Field: k.Field, re: re})
	}
	return out, c.Version, nil
}

// Failure is one failed TaskRun, explained.
type Failure struct {
	TaskRun string `json:"taskRun"`
	Task    string `json:"task,omitempty"`   // the pipeline task (tekton.dev/pipelineTask)
	Step    string `json:"step,omitempty"`   // the first step that exited non-zero
	Reason  string `json:"reason,omitempty"` // the TaskRun's Succeeded condition
	Message string `json:"message,omitempty"`
	// Known is the matched knownFailure; nil when none matched.
	Known *KnownFailure `json:"knownFailure"`
	// Evidence is the line that matched, or the failing step's last lines when nothing did.
	Evidence string `json:"evidence,omitempty"`
	// Log: "archived", or "missing" when Results stored none (docs/admin/tekton-results.md).
	Log string `json:"log"`
}

// Explanation is a run, explained.
type Explanation struct {
	Result          string    `json:"result"`
	Name            string    `json:"name,omitempty"`
	Status          string    `json:"status"`
	Failures        []Failure `json:"failures"`
	ContractVersion int       `json:"contractVersion"`
}

func m(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// succeeded returns the status and reason/message of an object's Succeeded condition.
func succeeded(obj map[string]any) (status, reason, message string) {
	for _, cond := range list(m(obj["status"])["conditions"]) {
		if str(cond["type"]) == "Succeeded" {
			return str(cond["status"]), str(cond["reason"]), str(cond["message"])
		}
	}
	return "", "", ""
}

func runStatus(pr map[string]any) string {
	st, reason, _ := succeeded(pr)
	switch {
	case st == "True":
		return "succeeded"
	case st == "Unknown":
		return "running"
	case st == "":
		return "unknown"
	case strings.Contains(reason, "Cancelled"):
		return "cancelled"
	}
	return "failed"
}

// list reads a list that is []any when decoded from JSON and []results.Obj when built by the
// trimming in package results (a cached run is never round-tripped through JSON).
func list(v any) []map[string]any {
	switch l := v.(type) {
	case []results.Obj:
		return l
	case []any:
		out := make([]map[string]any, 0, len(l))
		for _, x := range l {
			out = append(out, m(x))
		}
		return out
	}
	return nil
}

func failingStep(tr map[string]any) string {
	for _, step := range list(m(tr["status"])["steps"]) {
		if code, ok := m(step["terminated"])["exitCode"].(float64); ok && code != 0 {
			return str(step["name"])
		}
	}
	return ""
}

// stepLines returns a log's lines for one step ("[step] <timestamp> text"), without the prefix and
// the runtime timestamp. With step "" it returns every line.
func stepLines(log, step string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		rest := line
		if strings.HasPrefix(line, "[") {
			i := strings.Index(line, "] ")
			if i < 0 {
				continue
			}
			if step != "" && line[1:i] != step {
				continue
			}
			rest = line[i+2:]
		} else if step != "" || strings.TrimSpace(line) == "" {
			continue
		}
		if sp := strings.IndexByte(rest, ' '); sp > 0 && looksLikeTimestamp(rest[:sp]) {
			rest = rest[sp+1:]
		}
		out = append(out, rest)
	}
	return out
}

func looksLikeTimestamp(s string) bool {
	return len(s) >= 20 && s[4] == '-' && s[10] == 'T' && strings.HasSuffix(s, "Z")
}

// Classify finds the first knownFailure (in contract order) matching any of the lines, and the
// first line it matched.
func Classify(known []KnownFailure, lines []string) (*KnownFailure, string) {
	for i := range known {
		for _, l := range lines {
			if known[i].re.MatchString(l) {
				return &known[i], strings.TrimSpace(l)
			}
		}
	}
	return nil, ""
}

// LogFunc reads one TaskRun's archived log; results.NoLog when Results stored none.
type LogFunc func(taskRun string) (string, error)

// Run explains one archived run. A log that can't be read for a reason other than NoLog fails the
// whole call, so a caller never mistakes "Results was down" for "nothing matched".
func Run(run *results.Run, known []KnownFailure, version int, logs LogFunc) (*Explanation, error) {
	meta := m(run.PipelineRun["metadata"])
	ex := &Explanation{Result: run.Result, Name: str(meta["name"]), Status: runStatus(run.PipelineRun),
		Failures: []Failure{}, ContractVersion: version}
	for _, tr := range run.TaskRuns {
		st, reason, message := succeeded(tr)
		if st != "False" {
			continue
		}
		trMeta := m(tr["metadata"])
		f := Failure{TaskRun: str(trMeta["name"]), Task: str(m(trMeta["labels"])["tekton.dev/pipelineTask"]),
			Step: failingStep(tr), Reason: reason, Message: message, Log: "archived"}
		text, err := logs(f.TaskRun)
		var lines []string
		switch {
		case err == nil:
			lines = stepLines(text, f.Step)
			if len(lines) == 0 {
				lines = stepLines(text, "")
			}
		case isNoLog(err):
			f.Log = "missing"
		default:
			return nil, err
		}
		// The condition message is evidence too: a timeout or an image pull failure never reaches the log.
		f.Known, f.Evidence = Classify(known, append(lines, message))
		if f.Known == nil && len(lines) > 0 {
			f.Evidence = strings.Join(lines[max(0, len(lines)-tailLines):], "\n")
		}
		ex.Failures = append(ex.Failures, f)
	}
	return ex, nil
}

func isNoLog(err error) bool {
	var nl results.NoLog
	return errors.As(err, &nl)
}
