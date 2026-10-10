package main

// POST /facts/<cluster> - ADR-0021. The Argo Rollouts notifications engine on an upper
// cluster posts one small JSON fact per Rollout phase change (docs/admin/adr/0021-*).
// A fact is never trusted for meaning: this file joins it to the dev-side release record
// (the release-tracking-<chain-id> ConfigMap open-release-pr.yaml writes) and builds
// the CDEvent (the shape the removed hook script used to send), so everything downstream of the
// broker is unchanged.
//
// Two modes, FACTS_MODE:
//   shadow (code default) - build the event, log it normalized, forward nothing. For trying the
//                           path out beside another reporter; the chart default is emit.
//   emit                  - forward the event to the broker.
//
// Idempotency without state: the event id is derived from (release-id, event type,
// outcome), never from the Rollout generation or a timestamp. Heartbeat and scale facts
// for a release already reported therefore collapse onto the same id (the broker's
// Triggers name PipelineRuns by id, so a repeat is a no-op), and a rollback gets its own
// release-id and so its own events.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	annReleaseID    = "hangar.io/release-id"
	annChainID      = "hangar.io/chain-id"
	annAppNamespace = "hangar.io/app-namespace"

	recordPrefix    = "release-tracking-"
	appNamespaceKey = "app-" // dev-cluster Application namespaces; a fact may not point elsewhere

	eventDeploying = "dev.cdevents.environment.deploying.0.1.0"
	eventDeployed  = "dev.cdevents.environment.deployed.0.1.0"
	// ADR-0021 phase 4: the release a rollback replaced. CDEvents' own service.rolledback type, in
	// this relay's current (0.4.1-style) envelope; ADR-0022 moves the rest of the vocabulary later.
	eventRolledBack = "dev.cdevents.service.rolledback.0.2.0"
)

// fact mirrors the body of the Rollouts notification template (see the prod cluster's
// argo-rollouts-notification-configmap): the Rollout's identity, its annotations and its
// status, nothing else.
type fact struct {
	NS          string            `json:"ns"`
	Name        string            `json:"name"`
	UID         string            `json:"uid"`
	Generation  int64             `json:"generation"`
	Annotations map[string]string `json:"annotations"`
	Status      struct {
		Phase              string `json:"phase"`
		Message            string `json:"message"`
		Abort              bool   `json:"abort"`
		CurrentPodHash     string `json:"currentPodHash"`
		CurrentStepIndex   *int   `json:"currentStepIndex"`
		ObservedGeneration string `json:"observedGeneration"`
	} `json:"status"`
}

// record is what open-release-pr.yaml stores on dev for a release.
type record struct {
	AppNamespace  string
	AppName       string
	Env           string
	Cluster       string
	GitURL        string
	GitRevision   string
	FlowStartTime string
	ConfigJSON    string
	ChainID       string
	Kind          string // promote | rollback (phase 4)
	RollbackOf    string // the release-id a rollback replaces
	Image         string
}

// releaseID is "<chain-id>:<cluster>/<env>".
func parseReleaseID(id string) (chain, cluster, env string, ok bool) {
	c, rest, found := strings.Cut(id, ":")
	if !found || c == "" {
		return "", "", "", false
	}
	cl, e, found := strings.Cut(rest, "/")
	if !found || cl == "" || e == "" || strings.Contains(e, "/") {
		return "", "", "", false
	}
	return c, cl, e, true
}

func (h *handler) handleFacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cluster := strings.TrimPrefix(r.URL.Path, "/facts/")
	if cluster == "" || strings.Contains(cluster, "/") {
		http.Error(w, "cluster name required in path: /facts/<cluster>", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if err := h.authenticate(ctx, cluster, r.Header.Get("Authorization")); err != nil {
		log.Printf("glidepath-relay: facts auth failed for cluster=%s: %v", cluster, err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, requestBodyMaxSize))
	if err != nil {
		http.Error(w, fmt.Sprintf("reading request body: %v", err), http.StatusBadRequest)
		return
	}
	var f fact
	if err := json.Unmarshal(body, &f); err != nil {
		http.Error(w, fmt.Sprintf("invalid fact JSON: %v", err), http.StatusBadRequest)
		return
	}

	relID := f.Annotations[annReleaseID]
	if relID == "" {
		w.Header().Set("X-Fact-Ignored", "untracked")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	chain, relCluster, env, ok := parseReleaseID(relID)
	if !ok {
		http.Error(w, fmt.Sprintf("malformed release-id %q", relID), http.StatusBadRequest)
		return
	}
	// Cluster identity is what the shared secret proves; a release-id naming another
	// cluster is the spoof this check rejects.
	if relCluster != cluster {
		http.Error(w, fmt.Sprintf("release-id claims cluster %q, authenticated as %q", relCluster, cluster), http.StatusBadRequest)
		return
	}
	// A fact for a generation the controller has not observed yet describes the old state
	// under the new annotations (found in phase 0). The prod triggers already guard on
	// this; checking again costs nothing.
	if f.Status.ObservedGeneration != fmt.Sprint(f.Generation) {
		w.Header().Set("X-Fact-Ignored", "stale-generation")
		w.WriteHeader(http.StatusAccepted)
		return
	}

	load := func() (*record, *corev1.ConfigMap, error) {
		rec, cm, err := h.loadRecord(ctx, &f, chain)
		if err != nil {
			// Expected after the record has been swept, or for a release that predates the record.
			return nil, nil, ignoreErr("no-record: " + err.Error())
		}
		if rec.Env != env || rec.Cluster != cluster {
			return nil, nil, badErr(fmt.Sprintf("record for chain %q is for %s/%s, fact says %s/%s", chain, rec.Cluster, rec.Env, cluster, env))
		}
		return rec, cm, nil
	}
	h.apply(w, ctx, cluster, f.NS+"/"+f.Name, relID, load, func(st *relState, now time.Time) decision { return reduce(st, &f, now) })
}

// ignoreErr and badErr let a loader say "valid but nothing to do" (202) or "reject" (400).
type ignoreErr string
type badErr string

func (e ignoreErr) Error() string { return string(e) }
func (e badErr) Error() string    { return string(e) }

// apply is the read-modify-write every fact goes through. Two relay replicas can race, so a
// conflict re-reads and re-applies the (pure) reducer. Events go out BEFORE the state is
// written: if the write then fails the next fact repeats the same deterministic event ids,
// which downstream collapses; the other order could lose an event for good.
func (h *handler) apply(w http.ResponseWriter, ctx context.Context, cluster, who, relID string,
	load func() (*record, *corev1.ConfigMap, error), fn func(*relState, time.Time) decision) {
	ignored := ""
	for attempt := 0; attempt < 4; attempt++ {
		rec, cm, err := load()
		if err != nil {
			switch e := err.(type) {
			case ignoreErr:
				w.Header().Set("X-Fact-Ignored", string(e))
				w.WriteHeader(http.StatusAccepted)
			case badErr:
				http.Error(w, string(e), http.StatusBadRequest)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		if relID == "" {
			relID = cm.Data["releaseId"]
		}

		st := stateFromRecord(cm, h.mode())
		firstFact := st.LastFactAt.IsZero()
		dec := fn(&st, time.Now().UTC())

		for _, em := range dec.Emits {
			ev, err := buildEvent(rec, relID, em)
			if err != nil {
				http.Error(w, "building event: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if h.mode() != "emit" {
				log.Printf("glidepath-relay: shadow-event %s", normalizeEvent(ev))
				continue
			}
			if err := h.forward(ctx, ev); err != nil {
				log.Printf("glidepath-relay: forwarding event failed (cluster=%s source=%s kind=%s): %v", cluster, who, em.Kind, err)
				http.Error(w, "failed to forward event", http.StatusBadGateway)
				return
			}
		}

		err = h.persistState(ctx, cm, st)
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			// No permission, or the record vanished: carry on statelessly (the deterministic
			// ids still make repeats harmless) rather than dropping the fact.
			log.Printf("glidepath-relay: not persisting release state for %s/%s: %v", cm.Namespace, cm.Name, err)
		} else if firstFact {
			h.supersedeOlder(ctx, cm)
		}
		if dec.Drift != "" {
			h.recordDrift(ctx, cm, relID, dec.Drift)
		}
		if dec.RolledBack != "" {
			h.markRolledBack(ctx, cm, dec.RolledBack, relID)
		}
		ignored = dec.Ignore
		break
	}
	if ignored != "" {
		w.Header().Set("X-Fact-Ignored", ignored)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *handler) mode() string {
	if h.factsMode == "emit" {
		return "emit"
	}
	return "shadow"
}

// buildEvent builds the release's CDEvent. The shape is the one the removed hook script sent,
// kept so everything downstream of the broker is unchanged.
func buildEvent(rec *record, relID string, em emission) ([]byte, error) {
	source := fmt.Sprintf("/platform-cicd/%s/%s-%s-%s-outcome", rec.AppNamespace, rec.Cluster, rec.AppName, rec.Env)
	sum := sha256.Sum256([]byte(source + ":" + em.EventType + ":" + relID + ":" + em.Outcome))
	id := hex.EncodeToString(sum[:])[:20]
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// The field is compact JSON with key order kept (what the hook script's jq tojson made).
	cfg := "{}"
	var cbuf bytes.Buffer
	if json.Compact(&cbuf, []byte(rec.ConfigJSON)) == nil {
		cfg = cbuf.String()
	}
	ev := map[string]any{
		"context": map[string]any{
			"version": "0.4.1", "id": id, "source": source, "type": em.EventType,
			"timestamp": now, "chainId": rec.ChainID,
		},
		"subject": map[string]any{
			"id": source, "source": source, "type": "environment",
			"content": map[string]any{
				"appNamespace": rec.AppNamespace, "appName": rec.AppName, "env": rec.Env,
				"cluster": rec.Cluster, "phase": em.Phase, "outcome": em.Outcome, "status": em.Status,
				"revision": "", "gitUrl": rec.GitURL, "gitRevision": rec.GitRevision,
				"flowStartTime": rec.FlowStartTime, "finishedAt": now,
				"configJson": cfg,
				"releaseId":  relID, "releaseKind": rec.Kind, "rollbackOf": rec.RollbackOf, "image": rec.Image,
			},
		},
		"customData": map[string]any{
			"platform": map[string]any{"traceparent": "", "flow_start_time": rec.FlowStartTime, "config_json": "{}"},
		},
		"customDataContentType": "application/json",
	}
	return json.Marshal(ev)
}

// loadRecord reads release-tracking-<chain-id> from the Application namespace the fact
// names. The ConfigMap name is fixed by code and the namespace must look like a dev
// Application namespace, so a fact cannot be used to read arbitrary ConfigMaps.
func (h *handler) loadRecord(ctx context.Context, f *fact, chain string) (*record, *corev1.ConfigMap, error) {
	ns := f.Annotations[annAppNamespace]
	if !strings.HasPrefix(ns, appNamespaceKey) || strings.Contains(ns, "/") {
		return nil, nil, fmt.Errorf("fact names app-namespace %q", ns)
	}
	cm, err := h.clientset.CoreV1().ConfigMaps(ns).Get(ctx, recordPrefix+chain, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	d := cm.Data
	rec := &record{
		AppNamespace: d["appNamespace"], AppName: d["appName"], Env: d["env"], Cluster: d["cluster"],
		GitURL: d["gitUrl"], GitRevision: d["gitRevision"], FlowStartTime: d["flowStartTime"],
		ConfigJSON: d["configJson"], ChainID: chain,
		Kind: d["kind"], RollbackOf: d["rollbackOf"], Image: d["image"],
	}
	if rec.AppNamespace == "" || rec.AppName == "" || rec.Env == "" || rec.Cluster == "" {
		return nil, nil, fmt.Errorf("record %s/%s predates the fields the reducer needs", ns, recordPrefix+chain)
	}
	if rec.AppNamespace != ns {
		return nil, nil, fmt.Errorf("record says appNamespace %q but lives in %q", rec.AppNamespace, ns)
	}
	return rec, cm, nil
}

// --- state persistence: keys on the release record ConfigMap -------------------------

const (
	keyState, keyStateAt, keyLastFactAt, keyLastFactPhase = "state", "stateAt", "lastFactAt", "lastFactPhase"
	keyPodHash, keyDrift, keyLastError                    = "podHash", "drift", "lastError"
	keyEmittedLive, keyEmittedShadow                      = "emittedLive", "emittedShadow"
	keyHealthyAt, keyRolledBackBy                         = "healthyAt", "rolledBackBy"
)

func emittedKey(mode string) string {
	if mode == "emit" {
		return keyEmittedLive
	}
	return keyEmittedShadow
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func stateFromRecord(cm *corev1.ConfigMap, mode string) relState {
	d := cm.Data
	st := relState{
		State: d[keyState], StateAt: parseTime(d[keyStateAt]), LastFactAt: parseTime(d[keyLastFactAt]),
		LastFactPhase: d[keyLastFactPhase], PodHash: d[keyPodHash], Drift: d[keyDrift], LastError: d[keyLastError],
		HealthyAt: parseTime(d[keyHealthyAt]), Kind: d["kind"], RollbackOf: d["rollbackOf"],
		Emitted: map[string]bool{},
	}
	if st.State == "" {
		st.State = stProposed
	}
	for _, k := range strings.Split(d[emittedKey(mode)], ",") {
		if k != "" {
			st.Emitted[k] = true
		}
	}
	return st
}

func (h *handler) persistState(ctx context.Context, cm *corev1.ConfigMap, st relState) error {
	next := cm.DeepCopy()
	if next.Data == nil {
		next.Data = map[string]string{}
	}
	ts := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	var em []string
	for k := range st.Emitted {
		em = append(em, k)
	}
	sort.Strings(em)
	d := next.Data
	d[keyState], d[keyStateAt], d[keyLastFactAt], d[keyLastFactPhase] = st.State, ts(st.StateAt), ts(st.LastFactAt), st.LastFactPhase
	d[keyPodHash], d[keyDrift], d[keyLastError] = st.PodHash, st.Drift, st.LastError
	if !st.HealthyAt.IsZero() {
		d[keyHealthyAt] = ts(st.HealthyAt)
	}
	d[emittedKey(h.mode())] = strings.Join(em, ",")
	_, err := h.clientset.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, next, metav1.UpdateOptions{})
	return err
}

// supersedeOlder marks earlier, still-live releases of the same app, environment and cluster
// as superseded when a newer release reports its first fact. Best effort: listing needs a
// permission that older namespaces may not have yet.
func (h *handler) supersedeOlder(ctx context.Context, cur *corev1.ConfigMap) {
	sel := fmt.Sprintf("hangar.io/subcomponent=release-tracking,hangar.io/app=%s,hangar.io/env=%s,hangar.io/cluster=%s",
		cur.Labels["hangar.io/app"], cur.Labels["hangar.io/env"], cur.Labels["hangar.io/cluster"])
	if cur.Labels["hangar.io/app"] == "" || cur.Labels["hangar.io/env"] == "" || cur.Labels["hangar.io/cluster"] == "" {
		return // a record written before the labels existed
	}
	list, err := h.clientset.CoreV1().ConfigMaps(cur.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		log.Printf("glidepath-relay: cannot list earlier releases in %s: %v", cur.Namespace, err)
		return
	}
	for i := range list.Items {
		old := &list.Items[i]
		if old.Name == cur.Name || old.Data[keyState] == stSuperseded || old.Data[keyState] == stRolledBack {
			continue
		}
		// Only releases that actually ran; an unmerged proposal is not superseded, it is stale.
		if old.Data[keyState] != stHealthy && old.Data[keyState] != stProgressing && old.Data[keyState] != stAborted && old.Data[keyState] != stDegraded {
			continue
		}
		if parseTime(old.Data["prCreatedAt"]).After(parseTime(cur.Data["prCreatedAt"])) {
			continue // the other one is newer
		}
		old = old.DeepCopy()
		old.Data[keyState], old.Data[keyStateAt] = stSuperseded, time.Now().UTC().Format(time.RFC3339)
		old.Data["supersededBy"] = cur.Data["releaseId"]
		if _, err := h.clientset.CoreV1().ConfigMaps(old.Namespace).Update(ctx, old, metav1.UpdateOptions{}); err != nil {
			log.Printf("glidepath-relay: marking %s/%s superseded: %v", old.Namespace, old.Name, err)
		}
	}
}

// markRolledBack marks the release a rollback replaced (phase 4). The rollback's record names it
// by release-id; its record is release-tracking-<chain> in the same Application namespace, and
// it must be the same app, environment and cluster, or a record could be used to mark another
// app's release. Best effort, like supersedeOlder: the rolledback event has already been sent.
func (h *handler) markRolledBack(ctx context.Context, cur *corev1.ConfigMap, target, by string) {
	chain, cluster, env, ok := parseReleaseID(target)
	if !ok {
		log.Printf("glidepath-relay: rollback %s names a malformed release-id %q", by, target)
		return
	}
	old, err := h.clientset.CoreV1().ConfigMaps(cur.Namespace).Get(ctx, recordPrefix+chain, metav1.GetOptions{})
	if err != nil {
		log.Printf("glidepath-relay: rollback %s: cannot read the release it replaced (%s): %v", by, target, err)
		return
	}
	if old.Data["appName"] != cur.Data["appName"] || old.Data["env"] != env || old.Data["cluster"] != cluster {
		log.Printf("glidepath-relay: rollback %s names %s, which is not the same app, environment and cluster", by, target)
		return
	}
	if old.Data[keyState] == stRolledBack {
		return
	}
	old = old.DeepCopy()
	old.Data[keyState], old.Data[keyStateAt] = stRolledBack, time.Now().UTC().Format(time.RFC3339)
	old.Data[keyRolledBackBy] = by
	if _, err := h.clientset.CoreV1().ConfigMaps(old.Namespace).Update(ctx, old, metav1.UpdateOptions{}); err != nil {
		log.Printf("glidepath-relay: marking %s/%s rolled-back: %v", old.Namespace, old.Name, err)
	}
}

// recordDrift raises a Kubernetes Event on the release record; the stalled-pipeline
// detector's Events are how this platform already surfaces this class of thing.
func (h *handler) recordDrift(ctx context.Context, cm *corev1.ConfigMap, relID, msg string) {
	log.Printf("glidepath-relay: release drift on %s: %s", relID, msg)
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: "release-drift-", Namespace: cm.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "ConfigMap", Namespace: cm.Namespace, Name: cm.Name, UID: cm.UID},
		Reason:         "ReleaseDrift", Message: msg, Type: corev1.EventTypeWarning,
		Source:         corev1.EventSource{Component: "glidepath-relay"},
		FirstTimestamp: metav1.Now(), LastTimestamp: metav1.Now(), Count: 1,
	}
	if _, err := h.clientset.CoreV1().Events(cm.Namespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		log.Printf("glidepath-relay: could not record the drift event: %v", err)
	}
}

// normalizeEvent drops the fields that legitimately differ between runs (ids, timestamps)
// and returns the rest as stable JSON, so shadow-mode events can be diffed.
func normalizeEvent(raw []byte) string {
	var e map[string]any
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Sprintf("{\"unparseable\":%q}", err.Error())
	}
	if c, ok := e["context"].(map[string]any); ok {
		delete(c, "id")
		delete(c, "timestamp")
	}
	if s, ok := e["subject"].(map[string]any); ok {
		if c, ok := s["content"].(map[string]any); ok {
			delete(c, "finishedAt")
		}
	}
	out, _ := json.Marshal(e) // map keys marshal sorted: stable
	return string(out)
}
