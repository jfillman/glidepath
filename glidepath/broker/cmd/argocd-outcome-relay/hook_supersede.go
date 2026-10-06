package main

// ADR-0021 phase 3a. Once the relay is in emit mode, the fact path reports releases and the
// hook Jobs' events (still arriving on /outcome until the Jobs are removed) would be a second
// copy of every event, with different ids so the broker would not collapse them. This decides
// which one wins, per release:
//
//   - the record says whether the fact path can own the release: it has a releaseId (written by
//     open-release-pr for every release since phase 1), so the Rollout carries the annotation;
//   - the PreSync "deploying" event is always dropped for such a release: facts cannot have
//     arrived yet (PreSync runs before the Rollout changes), and the first Progressing fact sends
//     the same event;
//   - a terminal event is dropped when the facts have already moved the record (progressing,
//     healthy, aborted, degraded, sync-failed...). If they have not (lost, or the Rollout
//     carries no annotation), the hook's event is the fallback: it is forwarded, and its kinds
//     are recorded as sent so a heartbeat fact that arrives later does not send them again.
//   - a release with no record, or a record with no releaseId, is not the fact path's: forwarded.

import (
	"context"
	"log"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// supersededByFacts reports whether the hook event should be dropped. When it returns false
// for a terminal event the fact path could have owned, the fallback has already been recorded.
func (h *handler) supersededByFacts(ctx context.Context, env *cdEventEnvelope) (bool, string) {
	c := env.Subject.Content
	chain := env.Context.ChainID
	if chain == "" || !strings.HasPrefix(c.AppNamespace, appNamespaceKey) || strings.Contains(c.AppNamespace, "/") {
		return false, ""
	}
	cm, err := h.clientset.CoreV1().ConfigMaps(c.AppNamespace).Get(ctx, recordPrefix+chain, metav1.GetOptions{})
	if err != nil || cm.Data["releaseId"] == "" {
		return false, "" // no record, or a release that predates the fact path
	}

	kind := kindFailure
	switch {
	case strings.Contains(env.Context.Type, "deploying"):
		kind = kindDeploying
	case c.Outcome == "success":
		kind = kindSuccess
	}
	if kind == kindDeploying {
		return true, "PreSync deploying; the first Progressing fact reports it"
	}

	switch cm.Data[keyState] {
	case stProgressing, stHealthy, stAborted, stDegraded, stSyncFailed, stSuperseded, stRolledBack:
		return true, "the facts have reported this release (state " + cm.Data[keyState] + ")"
	}
	// The facts have not reported it: let the hook's event through, and remember that it was sent.
	h.markEmittedLive(ctx, c.AppNamespace, cm.Name, kindDeploying, kind)
	log.Printf("argocd-outcome-relay: no fact reported release %s (state %q); forwarding the hook's %s as the fallback", cm.Data["releaseId"], cm.Data[keyState], kind)
	return false, ""
}

// markEmittedLive adds kinds to the record's live sent-set (best effort, with a conflict retry).
func (h *handler) markEmittedLive(ctx context.Context, ns, name string, kinds ...string) {
	for attempt := 0; attempt < 3; attempt++ {
		cm, err := h.clientset.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return
		}
		set := map[string]bool{}
		for _, k := range strings.Split(cm.Data[keyEmittedLive], ",") {
			if k != "" {
				set[k] = true
			}
		}
		for _, k := range kinds {
			set[k] = true
		}
		var out []string
		for k := range set {
			out = append(out, k)
		}
		sort.Strings(out)
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[keyEmittedLive] = strings.Join(out, ",")
		if _, err := h.clientset.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			log.Printf("argocd-outcome-relay: could not record the fallback as sent on %s/%s: %v", ns, name, err)
		}
		return
	}
}
