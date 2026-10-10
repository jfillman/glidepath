package main

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)

func mk(phase, hash string, abort bool) *fact {
	f := &fact{}
	f.Status.Phase, f.Status.CurrentPodHash, f.Status.Abort = phase, hash, abort
	return f
}

func kinds(d decision) []string {
	var k []string
	for _, e := range d.Emits {
		k = append(k, e.Kind)
	}
	return k
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// replay applies facts in order and returns, per fact, the emitted kinds, so a whole
// phase 0 / phase 1 capture reads as one table.
func replay(s *relState, facts []*fact) [][]string {
	var out [][]string
	for i, f := range facts {
		out = append(out, kinds(reduce(s, f, t0.Add(time.Duration(i)*time.Second))))
	}
	return out
}

func want(t *testing.T, got [][]string, w ...[]string) {
	t.Helper()
	if len(got) != len(w) {
		t.Fatalf("got %d steps, want %d", len(got), len(w))
	}
	for i := range w {
		if !eq(got[i], w[i]) {
			t.Errorf("step %d: emitted %v, want %v", i, got[i], w[i])
		}
	}
}

var none = []string{}

func TestNormalCanary(t *testing.T) {
	s := &relState{State: stMerged}
	// the real gate-api sequence from phase 1: repeated Progressing, a pause, then Healthy,
	// with a stray Progressing in the same second as Healthy
	got := replay(s, []*fact{
		mk("Progressing", "h1", false), mk("Progressing", "h1", false), mk("Paused", "h1", false),
		mk("Progressing", "h1", false), mk("Healthy", "h1", false), mk("Progressing", "h1", false),
		mk("Healthy", "h1", false), mk("Healthy", "h1", false),
	})
	want(t, got, []string{kindDeploying}, none, none, none, []string{kindSuccess}, none, none, none)
	if s.State != stHealthy {
		t.Errorf("state %s", s.State)
	}
}

func TestScaleAndHeartbeatAreNotReleases(t *testing.T) {
	s := &relState{State: stMerged}
	replay(s, []*fact{mk("Progressing", "h1", false), mk("Healthy", "h1", false)})
	// scale 2->3: new generation, same hash: Healthy, then Progressing for the new replica, then Healthy
	got := replay(s, []*fact{mk("Healthy", "h1", false), mk("Progressing", "h1", false), mk("Healthy", "h1", false)})
	want(t, got, none, none, none)
	if s.State != stHealthy {
		t.Errorf("a scale must leave the release healthy, got %s", s.State)
	}
}

func TestAbort(t *testing.T) {
	s := &relState{State: stMerged}
	got := replay(s, []*fact{
		mk("Progressing", "h1", false), mk("Degraded", "h1", true),
		mk("Degraded", "h1", true), // restartAt on an aborted rollout: repeat
	})
	want(t, got, []string{kindDeploying}, []string{kindFailure}, none)
	if s.State != stAborted {
		t.Errorf("state %s", s.State)
	}
}

func TestDegradedThenRecovered(t *testing.T) {
	s := &relState{State: stMerged}
	got := replay(s, []*fact{mk("Progressing", "h1", false), mk("Degraded", "h1", false), mk("Healthy", "h1", false)})
	want(t, got, []string{kindDeploying}, []string{kindFailure}, []string{kindSuccess})
	if s.State != stHealthy {
		t.Errorf("state %s", s.State)
	}
}

func TestLostEarlierFactsStillEmitInOrder(t *testing.T) {
	// the engine is at-most-once: the first fact we see may be the Healthy one
	s := &relState{State: stMerged}
	got := replay(s, []*fact{mk("Healthy", "h1", false)})
	want(t, got, []string{kindDeploying, kindSuccess})
}

func TestDriftIsNotARelease(t *testing.T) {
	s := &relState{State: stMerged}
	replay(s, []*fact{mk("Progressing", "h1", false), mk("Healthy", "h1", false)})
	// someone edits the image by hand: same release-id, new pod hash (what selfHeal reverts)
	d := reduce(s, mk("Progressing", "h2", false), t0.Add(time.Minute))
	if d.Drift == "" || len(d.Emits) != 0 {
		t.Fatalf("a changed pod hash on a known release must be drift with no events: %+v", d)
	}
	// and once reverted, the original hash is quietly healthy again
	if d := reduce(s, mk("Healthy", "h1", false), t0.Add(2*time.Minute)); len(d.Emits) != 0 || d.Drift != "" {
		t.Errorf("revert to the release's own hash is not news: %+v", d)
	}
	if s.State != stHealthy || s.PodHash != "h1" {
		t.Errorf("state %s hash %s", s.State, s.PodHash)
	}
}

func TestDegradedAfterHealthyIsNotAFailedDeploy(t *testing.T) {
	s := &relState{State: stMerged}
	replay(s, []*fact{mk("Progressing", "h1", false), mk("Healthy", "h1", false)})
	d := reduce(s, mk("Degraded", "h1", false), t0.Add(time.Hour))
	if d.Drift == "" || len(d.Emits) != 0 {
		t.Fatalf("Degraded after a reported success must not become a deployed-failure event: %+v", d)
	}
	if s.State != stHealthy {
		t.Errorf("state %s", s.State)
	}
}

func TestSupersededReleaseIgnoresFurtherFacts(t *testing.T) {
	s := &relState{State: stSuperseded, Emitted: map[string]bool{kindDeploying: true, kindSuccess: true}, PodHash: "h1"}
	for _, ph := range []string{"Progressing", "Healthy", "Degraded"} {
		if d := reduce(s, mk(ph, "h1", false), t0); len(d.Emits) != 0 {
			t.Errorf("%s on a superseded release emitted %v", ph, kinds(d))
		}
	}
	if s.State != stSuperseded {
		t.Errorf("state %s", s.State)
	}
}

func TestStateTimestamps(t *testing.T) {
	s := &relState{State: stMerged}
	reduce(s, mk("Progressing", "h1", false), t0)
	a := s.StateAt
	reduce(s, mk("Paused", "h1", false), t0.Add(time.Minute))
	if !s.StateAt.Equal(a) {
		t.Errorf("StateAt must move only when the state changes")
	}
	if !s.LastFactAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("LastFactAt must move on every fact")
	}
}

func TestDriftIsReportedOncePerChange(t *testing.T) {
	s := &relState{State: stMerged}
	replay(s, []*fact{mk("Progressing", "h1", false), mk("Healthy", "h1", false)})
	// the edit stays in place: every fact the Rollout sends now carries the new hash, in
	// different phases (one real edit produced 9 Events before this was deduplicated)
	n := 0
	for i, ph := range []string{"Progressing", "Paused", "Progressing", "Healthy", "Healthy", "Progressing"} {
		if d := reduce(s, mk(ph, "h2", false), t0.Add(time.Duration(i)*time.Second)); d.Drift != "" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("one edit must raise one drift, got %d", n)
	}
	// reverting and going healthy ends the drift...
	if d := reduce(s, mk("Healthy", "h1", false), t0.Add(time.Minute)); d.Drift != "" || s.Drift != "" {
		t.Fatalf("a Healthy fact with the release's own hash must clear the drift: %+v %q", d, s.Drift)
	}
	// ...so the next edit is news again
	if d := reduce(s, mk("Progressing", "h3", false), t0.Add(2*time.Minute)); d.Drift == "" {
		t.Errorf("a new drift after the old one cleared must be reported")
	}
}

func TestDegradedAfterHealthyIsReportedOnce(t *testing.T) {
	s := &relState{State: stMerged}
	replay(s, []*fact{mk("Progressing", "h1", false), mk("Healthy", "h1", false)})
	n := 0
	for i := 0; i < 5; i++ {
		if d := reduce(s, mk("Degraded", "h1", false), t0.Add(time.Duration(i)*time.Second)); d.Drift != "" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("repeated Degraded facts must raise one drift, got %d", n)
	}
	if d := reduce(s, mk("Healthy", "h1", false), t0.Add(time.Minute)); d.Drift != "" || s.Drift != "" {
		t.Errorf("recovery must clear it")
	}
}

// Phase 4: Tower's Retry clears the Rollout's abort and the canary runs again under the same
// release-id. The release reopens instead of ignoring the Progressing fact, sends no second
// deploying event, and reports success when it gets there.
func TestRetryOfAnAbortedRelease(t *testing.T) {
	s := &relState{State: stMerged}
	got := replay(s, []*fact{
		mk("Progressing", "h1", false), mk("Degraded", "h1", true),
		mk("Progressing", "h1", false), // retry
		mk("Healthy", "h1", false),
	})
	want(t, got, []string{kindDeploying}, []string{kindFailure}, none, []string{kindSuccess})
	if s.State != stHealthy || s.HealthyAt.IsZero() {
		t.Errorf("state %s healthyAt %v", s.State, s.HealthyAt)
	}
}

func TestRetryThatFailsAgainGoesBackToAborted(t *testing.T) {
	s := &relState{State: stMerged}
	replay(s, []*fact{mk("Progressing", "h1", false), mk("Degraded", "h1", true), mk("Progressing", "h1", false)})
	if s.State != stProgressing {
		t.Fatalf("retry must reopen the release, state %s", s.State)
	}
	d := reduce(s, mk("Degraded", "h1", true), t0.Add(time.Minute))
	if len(d.Emits) != 0 || s.State != stAborted {
		t.Errorf("second abort: emitted %v, state %s", kinds(d), s.State)
	}
}

func TestAbortedRolloutStillAbortedIsNotARetry(t *testing.T) {
	s := &relState{State: stAborted, Emitted: map[string]bool{kindDeploying: true, kindFailure: true}}
	f := mk("Progressing", "h1", true)
	if d := reduce(s, f, t0); s.State != stAborted || d.Ignore == "" {
		t.Errorf("a Progressing fact that still says abort must not reopen: state %s", s.State)
	}
}

func TestHealthyAtOutlivesTheState(t *testing.T) {
	s := &relState{State: stMerged}
	replay(s, []*fact{mk("Progressing", "h1", false), mk("Healthy", "h1", false)})
	at := s.HealthyAt
	s.State = stSuperseded
	reduce(s, mk("Healthy", "h1", false), t0.Add(time.Hour))
	if at.IsZero() || !s.HealthyAt.Equal(at) {
		t.Errorf("healthyAt %v, then %v", at, s.HealthyAt)
	}
}

func TestRollbackReleaseNamesTheReleaseItReplaced(t *testing.T) {
	s := &relState{State: stMerged, Kind: releaseKindRollback, RollbackOf: "old:kind-prod/staging"}
	var last decision
	for i, f := range []*fact{mk("Progressing", "h2", false), mk("Healthy", "h2", false), mk("Healthy", "h2", false)} {
		d := reduce(s, f, t0.Add(time.Duration(i)*time.Second))
		if i == 1 {
			last = d
		}
		if i == 2 && (len(d.Emits) != 0 || d.RolledBack != "") {
			t.Errorf("a repeat Healthy must not roll back twice: %v %q", kinds(d), d.RolledBack)
		}
	}
	if !eq(kinds(last), []string{kindSuccess, kindRolledBack}) || last.RolledBack != "old:kind-prod/staging" {
		t.Errorf("healthy rollback: emitted %v rolledBack %q", kinds(last), last.RolledBack)
	}
}

func TestPromoteReleaseNeverRollsBack(t *testing.T) {
	s := &relState{State: stMerged, Kind: "promote", RollbackOf: "x:kind-prod/staging"}
	replay(s, []*fact{mk("Progressing", "h1", false)})
	if d := reduce(s, mk("Healthy", "h1", false), t0.Add(time.Minute)); d.RolledBack != "" {
		t.Errorf("only a rollback release marks another rolled-back")
	}
}
