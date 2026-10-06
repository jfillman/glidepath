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
	stSyncFailed  = "sync-failed"

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
	LastError     string
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
		// Reported once per distinct change: while the changed template stays, every fact the
		// Rollout sends carries the new hash (seen live: 9 Events in under a minute from one
		// edit), and the description deliberately leaves out the rollout phase so those facts
		// compare equal.
		desc := fmt.Sprintf("pod template changed outside a release: %s -> %s", s.PodHash, hash)
		d.Ignore = "drift"
		if s.Drift != desc {
			s.Drift = desc
			d.Drift = desc
		} else {
			d.Ignore = "drift (already reported)"
		}
		return d
	case s.Drift != "" && f.Status.Phase == "Healthy":
		// The release's own template is back and healthy: the drift is over, so a later one
		// is reported again.
		s.Drift = ""
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
			desc := "rollout became Degraded after the release was reported healthy: " + f.Status.Message
			d.Ignore = "drift (already reported)"
			if s.Drift != desc {
				s.Drift = desc
				d.Drift = desc
				d.Ignore = "drift"
			}
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

// reduceSyncFailed applies an Argo CD sync failure for the release. The sync failed before,
// or instead of, the Rollout reporting anything: a rejected manifest, an admission webhook,
// a hook, a missing secret. It is not terminal: Argo retries, and a later Progressing or
// Healthy fact for the same release moves it on. One failure event per release, however many
// retries Argo makes (the removed hook path sent one per attempt).
func reduceSyncFailed(s *relState, message string, now time.Time) decision {
	if s.Emitted == nil {
		s.Emitted = map[string]bool{}
	}
	var d decision
	switch s.State {
	case stHealthy, stSuperseded, stRolledBack, stClosed:
		// A sync failure on a release that already ran, or one that is no longer current,
		// is not this release failing to deploy.
		d.Ignore = "sync failure on a release that is " + s.State
		return d
	}
	s.LastError = message
	if s.State != stSyncFailed {
		s.State = stSyncFailed
		s.StateAt = now
	}
	for _, e := range []emission{emDeploying, emFailure} {
		if !s.Emitted[e.Kind] {
			s.Emitted[e.Kind] = true
			d.Emits = append(d.Emits, e)
		}
	}
	if len(d.Emits) == 0 {
		d.Ignore = "sync failure already reported"
	}
	return d
}
