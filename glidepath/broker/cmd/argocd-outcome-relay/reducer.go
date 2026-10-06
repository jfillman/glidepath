package main

// The release state machine (ADR-0021 phase 2). A pure function over the state kept in the
// release record ConfigMap and one Rollout fact; it decides which CDEvents to emit, whether
// the fact is drift, and what the new state is. No I/O here so the scenarios captured in
// phase 0 can be replayed as plain tests.
//
//	proposed -> merged -> progressing -> healthy -> superseded | rolled-back
//	                          |  ^
//	                          v  |
//	                       aborted / degraded
//
// Facts are at-most-once and arrive out of order, repeat (heartbeat), and describe scale or
// restart as if they were releases (all seen in phase 0). The rules that make that safe:
//   - a release is identified by its release-id; the pod template hash seen for it is fixed
//     by the first fact, and a different hash afterwards is drift, never a new release;
//   - a CDEvent is emitted at most once per (release, kind) as recorded in state, and the id
//     is deterministic anyway, so a repeat after a failed state write is harmless;
//   - a Progressing fact after the release is terminal is ignored (phase 1 saw one arrive in
//     the same second as the Healthy fact).

import (
	"fmt"
	"strings"
	"time"
)

const (
	stProposed    = "proposed"
	stMerged      = "merged"
	stProgressing = "progressing"
	stHealthy     = "healthy"
	stDegraded    = "degraded"
	stAborted     = "aborted"
	stSuperseded  = "superseded"
	stRolledBack  = "rolled-back"
	stClosed      = "closed"

	kindDeploying = "deploying"
	kindSuccess   = "deployed-success"
	kindFailure   = "deployed-failure"
)

// relState is what is stored under the release record's keys.
type relState struct {
	State         string
	StateAt       time.Time
	LastFactAt    time.Time
	LastFactPhase string
	PodHash       string
	Drift         string
	// Emitted is the set of CDEvent kinds already sent for this release, per mode, so that
	// switching shadow -> emit sends the history instead of believing it already did.
	Emitted map[string]bool
}

type emission struct {
	Kind, EventType, Phase, Outcome, Status string
}

type decision struct {
	Emits  []emission
	Drift  string // non-empty: out-of-band change, alert it
	Ignore string // non-empty: why this fact changed nothing
}

var (
	emDeploying = emission{kindDeploying, eventDeploying, "Syncing", "in_progress", "Syncing"}
	emSuccess   = emission{kindSuccess, eventDeployed, "Succeeded", "success", "Succeeded"}
	emFailure   = emission{kindFailure, eventDeployed, "Failed", "failure", "Failed"}
)

func terminal(state string) bool {
	switch state {
	case stHealthy, stAborted, stDegraded, stSuperseded, stRolledBack, stClosed:
		return true
	}
	return false
}

// reduce applies one fact to s, mutating it, and returns what to do. now is the arrival time.
func reduce(s *relState, f *fact, now time.Time) decision {
	if s.Emitted == nil {
		s.Emitted = map[string]bool{}
	}
	s.LastFactAt = now
	s.LastFactPhase = f.Status.Phase
	var d decision

	hash := f.Status.CurrentPodHash
	switch {
	case s.PodHash == "":
		s.PodHash = hash
	case hash != "" && hash != s.PodHash:
		// Same release-id, different pod template: someone changed the Rollout without a
		// release (an edit, an undo that selfHeal has not yet reverted). Not a new release.
		d.Drift = fmt.Sprintf("pod template changed outside a release: %s -> %s (rollout phase %s)", s.PodHash, hash, f.Status.Phase)
		s.Drift = d.Drift
		d.Ignore = "drift"
		return d
	}

	// emit adds kind to the decision once, in order, and records it.
	emit := func(e emission) {
		if !s.Emitted[e.Kind] {
			s.Emitted[e.Kind] = true
			d.Emits = append(d.Emits, e)
		}
	}
	// A fact can be the first one we ever see for a release (earlier ones were lost), so a
	// terminal event carries the "deploying" that should have preceded it.
	setState := func(st string) {
		if s.State != st {
			s.State = st
			s.StateAt = now
		}
	}

	switch f.Status.Phase {
	case "Progressing", "Paused":
		if terminal(s.State) {
			d.Ignore = fmt.Sprintf("%s fact after the release is %s", strings.ToLower(f.Status.Phase), s.State)
			return d
		}
		setState(stProgressing)
		emit(emDeploying)
	case "Healthy":
		if s.State == stSuperseded || s.State == stRolledBack || s.State == stClosed {
			d.Ignore = "release already " + s.State
			return d
		}
		if s.Emitted[kindSuccess] {
			d.Ignore = "already reported healthy"
			return d
		}
		setState(stHealthy)
		emit(emDeploying)
		emit(emSuccess)
	case "Degraded":
		if s.State == stSuperseded || s.State == stRolledBack || s.State == stClosed {
			d.Ignore = "release already " + s.State
			return d
		}
		if s.Emitted[kindFailure] {
			d.Ignore = "already reported failed"
			return d
		}
		if s.Emitted[kindSuccess] {
			// Healthy was already reported; Degraded now is the workload failing after a
			// good release, not the release failing. Tower and SLOs own that story.
			d.Drift = "rollout became Degraded after the release was reported healthy: " + f.Status.Message
			s.Drift = d.Drift
			d.Ignore = "drift"
			return d
		}
		if f.Status.Abort {
			setState(stAborted)
		} else {
			setState(stDegraded)
		}
		emit(emDeploying)
		emit(emFailure)
	default:
		d.Ignore = "no state for phase " + f.Status.Phase
	}
	if len(d.Emits) == 0 && d.Ignore == "" {
		d.Ignore = "nothing new"
	}
	return d
}
