package main

// POST /facts/<cluster> - ADR-0021. The Argo Rollouts notifications engine on an upper
// cluster posts one small JSON fact per Rollout phase change (docs/admin/adr/0021-*).
// A fact is never trusted for meaning: this file joins it to the dev-side release record
// (the release-tracking-<chain-id> ConfigMap open-release-pr.yaml writes) and rebuilds the
// same CDEvent argocd-outcome-hook.sh builds today, so everything downstream of the
// broker is unchanged in phase 1.
//
// Two modes, FACTS_MODE:
//   shadow (default) - build the event, log it normalized, forward nothing. Used while the
//                      hook Jobs are still live so a release is not reported twice.
//   emit             - forward the event to the broker like /outcome does.
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
	"strings"
	"time"

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
		log.Printf("argocd-outcome-relay: facts auth failed for cluster=%s: %v", cluster, err)
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

	ev, reason, err := h.eventForFact(ctx, cluster, &f)
	if err != nil {
		log.Printf("argocd-outcome-relay: fact rejected (cluster=%s rollout=%s/%s): %v", cluster, f.NS, f.Name, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ev == nil {
		// Not an error: untracked rollout, a phase with no CDEvent, or a stale generation.
		w.Header().Set("X-Fact-Ignored", reason)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if h.factsMode != "emit" {
		log.Printf("argocd-outcome-relay: shadow-event %s", normalizeEvent(ev))
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := h.forward(ctx, ev); err != nil {
		log.Printf("argocd-outcome-relay: forwarding fact event failed (cluster=%s rollout=%s/%s): %v", cluster, f.NS, f.Name, err)
		http.Error(w, "failed to forward event", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// eventForFact returns (nil, reason, nil) for a fact that is valid but produces no event,
// and an error for one that must be rejected.
func (h *handler) eventForFact(ctx context.Context, cluster string, f *fact) ([]byte, string, error) {
	relID := f.Annotations[annReleaseID]
	if relID == "" {
		return nil, "untracked", nil
	}
	chain, relCluster, env, ok := parseReleaseID(relID)
	if !ok {
		return nil, "", fmt.Errorf("malformed release-id %q", relID)
	}
	// Cluster identity is what the shared secret proves; a release-id naming another
	// cluster is the same spoof the /outcome path rejects.
	if relCluster != cluster {
		return nil, "", fmt.Errorf("release-id claims cluster %q, authenticated as %q", relCluster, cluster)
	}
	// A fact for a generation the controller has not observed yet describes the old
	// state under the new annotations (found in phase 0). The prod triggers already
	// guard on this; checking again costs nothing.
	if f.Status.ObservedGeneration != fmt.Sprint(f.Generation) {
		return nil, "stale-generation", nil
	}

	var evType, phase, outcome, status string
	switch f.Status.Phase {
	case "Progressing":
		evType, phase, outcome, status = eventDeploying, "Syncing", "in_progress", "Syncing"
	case "Healthy":
		evType, phase, outcome, status = eventDeployed, "Succeeded", "success", "Succeeded"
	case "Degraded":
		evType, phase, outcome, status = eventDeployed, "Failed", "failure", "Failed"
	default:
		return nil, "no-event-for-phase", nil
	}

	rec, err := h.loadRecord(ctx, f, chain)
	if err != nil {
		// A missing record is expected after the record has been consumed (heartbeat
		// facts arriving after the outcome was reported). Not an error.
		return nil, "no-record: " + err.Error(), nil
	}
	if rec.Env != env || rec.Cluster != cluster {
		return nil, "", fmt.Errorf("record for chain %q is for %s/%s, fact says %s/%s", chain, rec.Cluster, rec.Env, cluster, env)
	}

	source := fmt.Sprintf("/platform-cicd/%s/%s-%s-%s-outcome", rec.AppNamespace, rec.Cluster, rec.AppName, rec.Env)
	sum := sha256.Sum256([]byte(source + ":" + evType + ":" + relID + ":" + outcome))
	id := hex.EncodeToString(sum[:])[:20]
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// The hook builds this field with jq's tojson (compact, key order kept); compacting
	// the stored text gives the same string.
	cfg := "{}"
	var cbuf bytes.Buffer
	if json.Compact(&cbuf, []byte(rec.ConfigJSON)) == nil {
		cfg = cbuf.String()
	}
	ev := map[string]any{
		"context": map[string]any{
			"version": "0.4.1", "id": id, "source": source, "type": evType,
			"timestamp": now, "chainId": rec.ChainID,
		},
		"subject": map[string]any{
			"id": source, "source": source, "type": "environment",
			"content": map[string]any{
				"appNamespace": rec.AppNamespace, "appName": rec.AppName, "env": rec.Env,
				"cluster": rec.Cluster, "phase": phase, "outcome": outcome, "status": status,
				"revision": "", "gitUrl": rec.GitURL, "gitRevision": rec.GitRevision,
				"flowStartTime": rec.FlowStartTime, "finishedAt": now,
				"configJson": cfg,
			},
		},
		"customData": map[string]any{
			"platform": map[string]any{"traceparent": "", "flow_start_time": rec.FlowStartTime, "config_json": "{}"},
		},
		"customDataContentType": "application/json",
	}
	out, err := json.Marshal(ev)
	return out, "", err
}

// loadRecord reads release-tracking-<chain-id> from the Application namespace the fact
// names. The ConfigMap name is fixed by code and the namespace must look like a dev
// Application namespace, so a fact cannot be used to read arbitrary ConfigMaps.
func (h *handler) loadRecord(ctx context.Context, f *fact, chain string) (*record, error) {
	ns := f.Annotations[annAppNamespace]
	if !strings.HasPrefix(ns, appNamespaceKey) || strings.Contains(ns, "/") {
		return nil, fmt.Errorf("fact names app-namespace %q", ns)
	}
	cm, err := h.clientset.CoreV1().ConfigMaps(ns).Get(ctx, recordPrefix+chain, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	d := cm.Data
	rec := &record{
		AppNamespace: d["appNamespace"], AppName: d["appName"], Env: d["env"], Cluster: d["cluster"],
		GitURL: d["gitUrl"], GitRevision: d["gitRevision"], FlowStartTime: d["flowStartTime"],
		ConfigJSON: d["configJson"], ChainID: chain,
	}
	if rec.AppNamespace == "" || rec.AppName == "" || rec.Env == "" || rec.Cluster == "" {
		return nil, fmt.Errorf("record %s/%s predates the fields the reducer needs", ns, recordPrefix+chain)
	}
	if rec.AppNamespace != ns {
		return nil, fmt.Errorf("record says appNamespace %q but lives in %q", rec.AppNamespace, ns)
	}
	return rec, nil
}

// normalizeEvent drops the fields that legitimately differ between the hook path and
// this one (ids, timestamps) and returns the rest as stable JSON, so the two streams can
// be diffed. It is also what the hook path logs for each event it forwards.
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
