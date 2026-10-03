// platform/dora-exporter/cmd/dora-exporter/main.go
//
// Two input paths feeding the same four DORA metrics, not one:
//
//  1. Same-cluster envs (no cluster: in deploy.upperEnvironments): watches ArgoCD
//     Application objects directly (applications.argoproj.io, in the argocd
//     namespace, filtered to hangar.io/dora-track=true) for confirmed terminal sync
//     outcomes - unchanged since this service's original design. See
//     docs/dora-metrics.md for the full mechanism and why this watches ArgoCD
//     directly instead of subscribing to the CDEvents broker like the original
//     architecture plan assumed.
//  2. Cluster-mapped envs (Phase 3 item 4, docs/multi-cluster.md): a small HTTP
//     endpoint (/argocd-outcome) that platform/broker/cmd/argocd-outcome-relay calls
//     directly once an upper cluster's ArgoCD confirms a real outcome - this service
//     has no live API access to a remote cluster's Application object, so there's
//     nothing to watch there. NOT a replacement for path 1 - same-cluster
//     Applications still live in THIS cluster's argocd namespace and the informer is
//     still the right tool for them.
//
// Both paths funnel into the same recordOutcome() - the actual metric-recording logic
// doesn't care which path it came from. What's genuinely different is WHERE each path
// persists "last failure time" for MTTR correlation: path 1 patches an annotation back
// onto the live Application object it's already watching; path 2 has no such object on
// this cluster to patch (same "no direct API access to a remote cluster" principle as
// everywhere else in this feature - see mark-release-pending.yaml), so it persists the
// same information into a ConfigMap this service owns instead
// (clusterMappedStateCM, in this cluster's own platform-system namespace - see
// getClusterMappedLastFailure/patchClusterMappedLastFailure). Originally a known,
// deliberate gap (MTTR simply didn't work for cluster-mapped apps) - closed once
// /argocd-outcome moved from the relay's own direct HTTP call to a Tekton Task
// (release-outcome-notify.yaml's update-dora-metrics) that already runs on THIS
// cluster, same as everything else this service reads.
//
// Durable counters, not just in-memory ones (added after a live incident): the four
// Prometheus metrics below (deploymentsTotal/leadTimeSeconds/releasesTotal/
// timeToRestoreSecondsExperimental) are ordinary client_golang collectors - they only
// ever lived in this process's memory, with nothing behind them but the informer replay
// (path 1, and only for Applications still mid-flight when this process died) or
// nothing at all (path 2). A routine pod replacement - a redeploy, a node drain, an
// image update - zeroed every app's history with no warning: confirmed live 2026-08-22,
// checkout-api had two real ArgoCD-confirmed releases earlier the same day, both
// silently gone from the dashboard the moment this service's Deployment rolled a new
// ReplicaSet, because Prometheus scrapes (pull, not push - see docs/dora-metrics.md)
// only ever see whatever this process currently holds. Fixed the same way MTTR's own
// cluster-mapped gap was fixed above: a second ConfigMap this service owns
// (dora-metrics-state, primeMetricsFromState/persistMetricsState below) durably
// mirrors every counter/histogram observation for BOTH paths, and gets replayed back
// into the live Prometheus collectors once at startup, before either the informer or
// the HTTP server can process a single live event - so a restart resumes from the last
// recorded totals instead of zero.
//
// Correlation with a specific release attempt happens via the hangar.io/dora-*
// annotations the release Pipeline stamps directly onto the Application object
// (catalog/tasks/mark-release-pending.yaml for path 1, open-release-pr.yaml's
// GitOps-committed manifest for path 2) - not a separate datastore, not image/revision
// matching (status.summary.images is empty on this ArgoCD install - checked live
// before this was designed, see docs/dora-metrics.md).
//
// Same dynamic-client + GVR pattern platform/broker/cmd/token-review-interceptor/main.go
// already uses for the pipelinesascode.tekton.dev Repository CRD - proven in this exact
// codebase, not a new one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

var applicationGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

const (
	annoPending            = "hangar.io/dora-pending"
	annoFlowStartTime      = "hangar.io/dora-flow-start-time"
	annoBaselineStartedAt  = "hangar.io/dora-baseline-started-at"
	annoAppNamespace       = "hangar.io/dora-app-namespace"
	annoApp                = "hangar.io/dora-app"
	annoLastFailureTime    = "hangar.io/dora-last-failure-time"
	argocdNamespace        = "argocd"
	platformNamespace      = "platform-system"
	doraTrackLabelSelector = "hangar.io/dora-track=true"
	clusterMappedStateCM   = "dora-cluster-mapped-state"
	metricsStateCM         = "dora-metrics-state"
)

// Lead Time buckets are deliberately aligned to DORA's own published elite/high/medium/
// low bands (1h/1d/1w/1mo) so histogram_quantile() in Grafana directly shows which band
// most changes fall into - not an arbitrary Prometheus default bucket set.
var leadTimeBuckets = []float64{3600, 86400, 604800, 2592000}

// MTTR buckets are sized for human-response-time scale (minutes to a week), not
// deploy-time scale - a different shape of measurement than lead time.
var mttrBuckets = []float64{300, 1800, 3600, 14400, 86400, 604800}

var (
	deploymentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dora_deployments_total",
		Help: "Confirmed successful releases (ArgoCD sync Succeeded), by app_namespace/app. Deployment Frequency is increase() over this in Grafana, not precomputed here.",
	}, []string{"app_namespace", "app"})

	leadTimeSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dora_lead_time_seconds",
		Help:    "Time from a flow's original commit-triggered start (build's start-flow-root-span) to confirmed ArgoCD sync success.",
		Buckets: leadTimeBuckets,
	}, []string{"app_namespace", "app"})

	releasesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dora_releases_total",
		Help: "Every confirmed terminal release outcome, by app_namespace/app/outcome (succeeded|failed). Change Failure Rate is failed/total in Grafana.",
	}, []string{"app_namespace", "app", "outcome"})

	// _experimental is deliberate and structural, not just a dashboard note - this
	// platform has no incident-tracking or production-health signal, so this can only
	// ever measure "time to next confirmed successful release for this app", not a true
	// incident MTTR. Same honest caveat the architecture plan already applied to this
	// metric from the start.
	timeToRestoreSecondsExperimental = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dora_time_to_restore_seconds_experimental",
		Help:    "Time between a confirmed failed release and the next confirmed successful one for the same app. Best-effort - cannot see rollbacks/fixes that happen outside this pipeline.",
		Buckets: mttrBuckets,
	}, []string{"app_namespace", "app"})
)

func init() {
	prometheus.MustRegister(deploymentsTotal, leadTimeSeconds, releasesTotal, timeToRestoreSecondsExperimental)
}

// appMetricsState is dora-metrics-state's per-app JSON value (keyed "<appNamespace>.
// <appName>", same key shape appStateKey already uses for the cluster-mapped MTTR
// ConfigMap). Counters are plain running totals; the observation slices exist because
// client_golang's HistogramVec has no public API to set a histogram's cumulative
// bucket/sum/count state directly - the only way to rebuild the exact same distribution
// is to persist every raw observation and replay it through Observe() again at startup.
// Unbounded growth is a real tradeoff of that choice, deliberately accepted here: real
// release volume on this platform is a handful of events per app per day, nowhere near
// where ConfigMap's 1MiB size limit would become a concern.
type appMetricsState struct {
	Deployments          int64     `json:"deployments"`
	ReleasesSucceeded    int64     `json:"releasesSucceeded"`
	ReleasesFailed       int64     `json:"releasesFailed"`
	LeadTimeObservations []float64 `json:"leadTimeObservations,omitempty"`
	MTTRObservations     []float64 `json:"mttrObservations,omitempty"`
}

func main() {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}
	// Typed clientset, alongside the dynamic one above - only needed for the
	// ConfigMap-backed state stores (path 2's MTTR correlation, and both paths' durable
	// metrics state) - path 1's annotation patch-back already goes through dynClient,
	// since Applications aren't a built-in type.
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("clientset: %v", err)
	}

	srv := &server{clientset: clientset}

	// Rehydrate every counter/histogram from dora-metrics-state before the informer or
	// HTTP server can process a single live event, so a fresh event and a priming read
	// can never race and double-count the same observation.
	primeCtx, primeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	primeMetricsFromState(primeCtx, clientset)
	primeCancel()

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynClient, 10*time.Minute, argocdNamespace,
		func(opts *metav1.ListOptions) { opts.LabelSelector = doraTrackLabelSelector },
	)
	informer := factory.ForResource(applicationGVR).Informer()

	handler := func(obj interface{}) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		reconcile(context.Background(), dynClient, srv, u)
	}
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: handler,
		// Add fires for every already-existing matching Application on startup too -
		// deliberately reused here so a restart correctly catches up on any release
		// that was confirmed by ArgoCD while this service was down, rather than losing
		// it silently.
		UpdateFunc: func(_, newObj interface{}) { handler(newObj) },
	})

	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)
	if !cache.WaitForCacheSync(stop, informer.HasSynced) {
		log.Fatal("failed to sync informer cache")
	}
	log.Println("dora-exporter: informer cache synced, watching applications.argoproj.io in argocd (hangar.io/dora-track=true)")

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/argocd-outcome", srv.handleArgoCDOutcome)
	log.Println("dora-exporter: listening on :8080 (/metrics, /argocd-outcome)")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// primeMetricsFromState reads dora-metrics-state once at startup and replays every
// persisted counter/observation back into the live Prometheus collectors - Add() for
// the plain Counters, Observe() per historical value for the Histograms (the only way
// to reconstruct their exact bucket/sum/count state, see appMetricsState's own
// comment). A missing ConfigMap or an unparseable entry just means "nothing to
// rehydrate for this app" - never fatal, since starting at zero (today's actual bug) is
// already the fallback behavior this function exists to improve on, not something a
// read failure should make worse by crashing the process.
func primeMetricsFromState(ctx context.Context, clientset kubernetes.Interface) {
	cm, err := clientset.CoreV1().ConfigMaps(platformNamespace).Get(ctx, metricsStateCM, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		log.Println("dora-exporter: no prior dora-metrics-state found, starting counters at zero")
		return
	}
	if err != nil {
		log.Printf("dora-exporter: reading %s failed, starting counters at zero: %v", metricsStateCM, err)
		return
	}

	for key, raw := range cm.Data {
		appNamespace, appName, ok := splitStateKey(key)
		if !ok {
			log.Printf("dora-exporter: %s: unrecognized key %q, skipping", metricsStateCM, key)
			continue
		}
		var state appMetricsState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			log.Printf("dora-exporter: %s/%s: corrupt %s entry, skipping: %v", appNamespace, appName, metricsStateCM, err)
			continue
		}

		deploymentsTotal.WithLabelValues(appNamespace, appName).Add(float64(state.Deployments))
		releasesTotal.WithLabelValues(appNamespace, appName, "succeeded").Add(float64(state.ReleasesSucceeded))
		releasesTotal.WithLabelValues(appNamespace, appName, "failed").Add(float64(state.ReleasesFailed))
		for _, v := range state.LeadTimeObservations {
			leadTimeSeconds.WithLabelValues(appNamespace, appName).Observe(v)
		}
		for _, v := range state.MTTRObservations {
			timeToRestoreSecondsExperimental.WithLabelValues(appNamespace, appName).Observe(v)
		}
		log.Printf("dora-exporter: %s/%s: rehydrated %d deployment(s), %d succeeded/%d failed release(s) from %s",
			appNamespace, appName, state.Deployments, state.ReleasesSucceeded, state.ReleasesFailed, metricsStateCM)
	}
}

type server struct {
	clientset kubernetes.Interface

	// stateMu serializes read-modify-write cycles against dora-metrics-state
	// (persistMetricsState does a Get then a Patch, not a single atomic call - a
	// concurrent pair of HTTP requests for the same app could otherwise race and lose
	// an update). Deliberately a plain in-process mutex, not resourceVersion-based
	// optimistic concurrency: this Deployment is always 1 replica (see deployment.yaml's
	// own comment), so the only possible concurrent writers are goroutines inside this
	// same process, which a mutex already fully serializes.
	stateMu sync.Mutex
}

// argocdOutcomeRequest mirrors argocd-outcome-relay's own outcomeRequest shape (the
// relay forwards close to what it itself received, adding nothing dora-exporter-
// specific) - see platform/broker/cmd/argocd-outcome-relay/main.go.
type argocdOutcomeRequest struct {
	AppNamespace  string `json:"appNamespace"`
	AppName       string `json:"appName"`
	Phase         string `json:"phase"`
	FinishedAt    string `json:"finishedAt"`
	FlowStartTime string `json:"flowStartTime"`
}

// handleArgoCDOutcome is path 2 from this file's own header - no Application-object
// annotation patch-back (nothing on THIS cluster to patch), the same terminal-phase
// metric recording the informer path already does, plus its own last-failure-time
// bookkeeping against clusterMappedStateCM (this cluster's own ConfigMap, not a
// remote object). Called by release-outcome-notify.yaml's update-dora-metrics Task
// (runs on this cluster, same as everything else this service reads/writes) - not
// authenticated independently; that Task only exists because a cluster-mapped
// release's outcome already made it through argocd-outcome-relay's own auth once,
// same trust boundary as before.
func (s *server) handleArgoCDOutcome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req argocdOutcomeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.AppNamespace == "" || req.AppName == "" || req.Phase == "" {
		http.Error(w, "appNamespace, appName, and phase are required", http.StatusBadRequest)
		return
	}
	if !isTerminalPhase(req.Phase) {
		w.WriteHeader(http.StatusAccepted) // not an error - just nothing to record yet
		return
	}
	finishedAt, err := parseTime(req.FinishedAt)
	if err != nil {
		http.Error(w, "unparseable finishedAt", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	key := appStateKey(req.AppNamespace, req.AppName)
	lastFailureTimeStr, err := s.getClusterMappedLastFailure(ctx, key)
	if err != nil {
		// Best-effort: a read failure here shouldn't block recording the other three
		// metrics, it should just mean this particular MTTR sample is skipped (same
		// as an unparseable lastFailureTimeStr already does inside recordOutcome).
		log.Printf("dora-exporter: %s/%s: reading %s state failed (non-fatal, MTTR sample may be skipped): %v", req.AppNamespace, req.AppName, clusterMappedStateCM, err)
	}

	result := recordOutcome(req.AppNamespace, req.AppName, req.Phase, finishedAt, req.FlowStartTime, lastFailureTimeStr)
	s.persistMetricsState(ctx, req.AppNamespace, req.AppName, req.Phase, result)
	if req.Phase == "Succeeded" && result.LastFailureCleared {
		if err := s.patchClusterMappedLastFailure(ctx, key, nil); err != nil {
			log.Printf("dora-exporter: %s/%s: clearing %s state failed: %v", req.AppNamespace, req.AppName, clusterMappedStateCM, err)
		}
	} else if req.Phase == "Failed" || req.Phase == "Error" {
		finishedAtStr := finishedAt.Format(time.RFC3339)
		if err := s.patchClusterMappedLastFailure(ctx, key, &finishedAtStr); err != nil {
			log.Printf("dora-exporter: %s/%s: recording %s state failed: %v", req.AppNamespace, req.AppName, clusterMappedStateCM, err)
		}
	}

	log.Printf("dora-exporter: %s/%s: recorded %s via /argocd-outcome (cluster-mapped env)", req.AppNamespace, req.AppName, req.Phase)
	w.WriteHeader(http.StatusAccepted)
}

// appStateKey mirrors the DNS-1123 subdomain rules appName/appNamespace already
// satisfy (both come from validated cicd.yaml/Kubernetes namespace names) - a
// ConfigMap data key just needs to be a valid key, "." is fine and reads naturally.
// Shared by both of this service's ConfigMap-backed state stores (clusterMappedStateCM
// and metricsStateCM), not just the cluster-mapped-specific one its name used to imply.
func appStateKey(appNamespace, appName string) string {
	return appNamespace + "." + appName
}

// splitStateKey reverses appStateKey's "ns.app" join. Kubernetes namespace names are
// DNS-1123 labels and can't contain a dot, so the first dot always marks the boundary
// regardless of what characters appName itself contains.
func splitStateKey(key string) (appNamespace, appName string, ok bool) {
	i := strings.IndexByte(key, '.')
	if i < 0 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// getClusterMappedLastFailure reads clusterMappedStateCM's own key for this app - a
// missing ConfigMap or missing key both just mean "no prior failure on record", not an
// error the caller needs to treat specially.
func (s *server) getClusterMappedLastFailure(ctx context.Context, key string) (string, error) {
	cm, err := s.clientset.CoreV1().ConfigMaps(platformNamespace).Get(ctx, clusterMappedStateCM, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return cm.Data[key], nil
}

// patchClusterMappedLastFailure sets (value non-nil) or clears (value nil) this app's
// key via a JSON merge patch - additive/idempotent regardless of what else is in the
// ConfigMap, same pattern as persistMetricsState below. clusterMappedStateCM itself is
// rendered by the Helm chart with metadata only, deliberately no `data:` field at all
// (not even `data: {}`) - see that manifest's own comment for why declaring the field,
// even empty, would let a later `kubectl apply`/ArgoCD sync prune every key this
// service's own PATCH calls have added since.
func (s *server) patchClusterMappedLastFailure(ctx context.Context, key string, value *string) error {
	var jsonValue interface{} = value
	if value != nil {
		jsonValue = *value
	}
	patch := map[string]interface{}{"data": map[string]interface{}{key: jsonValue}}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.clientset.CoreV1().ConfigMaps(platformNamespace).Patch(ctx, clusterMappedStateCM, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// persistMetricsState mirrors the outcome recordOutcome just recorded in-memory into
// dora-metrics-state, so primeMetricsFromState can rebuild the same live state after a
// restart. Called from both input paths (reconcile for path 1, handleArgoCDOutcome for
// path 2) - the durability gap this closes affected both equally, not just the
// cluster-mapped one MTTR already had a fix for. Read-modify-write, not a blind patch
// (each app's counters need the CURRENT persisted value to increment from) - see
// stateMu's own comment for why that's safe without resourceVersion-based retries here.
func (s *server) persistMetricsState(ctx context.Context, appNamespace, appName, phase string, result outcomeResult) {
	if !isTerminalPhase(phase) {
		return // nothing to persist - recordOutcome only ever mutates metrics for terminal phases
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	key := appStateKey(appNamespace, appName)
	var state appMetricsState
	cm, err := s.clientset.CoreV1().ConfigMaps(platformNamespace).Get(ctx, metricsStateCM, metav1.GetOptions{})
	switch {
	case k8serrors.IsNotFound(err):
		// no prior state for anything yet - state stays zero-valued, same as a
		// first-ever cold start
	case err != nil:
		log.Printf("dora-exporter: %s/%s: reading %s failed, metrics state not persisted: %v", appNamespace, appName, metricsStateCM, err)
		return
	default:
		if raw, ok := cm.Data[key]; ok {
			if err := json.Unmarshal([]byte(raw), &state); err != nil {
				log.Printf("dora-exporter: %s/%s: corrupt %s entry, resetting persisted counters: %v", appNamespace, appName, metricsStateCM, err)
				state = appMetricsState{}
			}
		}
	}

	switch phase {
	case "Succeeded":
		state.Deployments++
		state.ReleasesSucceeded++
		if result.HasLeadTime {
			state.LeadTimeObservations = append(state.LeadTimeObservations, result.LeadTimeSeconds)
		}
		if result.HasMTTR {
			state.MTTRObservations = append(state.MTTRObservations, result.MTTRSeconds)
		}
	case "Failed", "Error":
		state.ReleasesFailed++
	}

	body, err := json.Marshal(state)
	if err != nil {
		log.Printf("dora-exporter: %s/%s: marshaling %s state failed: %v", appNamespace, appName, metricsStateCM, err)
		return
	}
	patch := map[string]interface{}{"data": map[string]interface{}{key: string(body)}}
	patchBody, err := json.Marshal(patch)
	if err != nil {
		log.Printf("dora-exporter: %s/%s: marshaling %s patch failed: %v", appNamespace, appName, metricsStateCM, err)
		return
	}
	if _, err := s.clientset.CoreV1().ConfigMaps(platformNamespace).Patch(ctx, metricsStateCM, types.MergePatchType, patchBody, metav1.PatchOptions{}); err != nil {
		log.Printf("dora-exporter: %s/%s: persisting %s state failed: %v", appNamespace, appName, metricsStateCM, err)
	}
}

// reconcile implements the annotation-based hand-off described in
// catalog/tasks/mark-release-pending.yaml and docs/dora-metrics.md. It is safe to call
// repeatedly for the same object state: once dora-pending is cleared (by this function's
// own patch), the next informer event for that same change carries no dora-pending
// annotation, so the early return below makes it a no-op - no separate dedup tracking
// needed beyond the annotations themselves.
func reconcile(ctx context.Context, dynClient dynamic.Interface, srv *server, app *unstructured.Unstructured) {
	annos := app.GetAnnotations()
	if annos[annoPending] != "true" {
		return
	}

	phase, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "phase")
	startedAtStr, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "startedAt")
	finishedAtStr, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "finishedAt")

	if !isTerminalPhase(phase) {
		return // still Running/Terminating/unset - not our moment yet
	}

	startedAt, err := parseTime(startedAtStr)
	if err != nil {
		log.Printf("dora-exporter: %s/%s: unparseable operationState.startedAt %q, skipping", app.GetNamespace(), app.GetName(), startedAtStr)
		return
	}

	// The baseline check: operationState gets overwritten by ANY sync, including
	// ArgoCD's own unrelated selfHeal drift-correction syncs. Only react once a sync
	// STRICTLY NEWER than the one recorded at pending-time appears - otherwise this
	// would react to a sync that had already finished before this release attempt even
	// started. An empty baseline (the Application had no operationState at all yet when
	// mark-release-pending ran) means anything terminal now counts.
	if baselineStr := annos[annoBaselineStartedAt]; baselineStr != "" {
		baseline, err := parseTime(baselineStr)
		if err == nil && !startedAt.After(baseline) {
			return // this is the same (or an older) sync we already had a baseline for
		}
	}

	finishedAt, err := parseTime(finishedAtStr)
	if err != nil {
		log.Printf("dora-exporter: %s/%s: unparseable operationState.finishedAt %q, skipping", app.GetNamespace(), app.GetName(), finishedAtStr)
		return
	}

	appNamespace := annos[annoAppNamespace]
	appName := annos[annoApp]

	patch := map[string]interface{}{annoPending: nil} // JSON merge patch: null removes the key
	result := recordOutcome(appNamespace, appName, phase, finishedAt, annos[annoFlowStartTime], annos[annoLastFailureTime])
	if phase == "Succeeded" && result.LastFailureCleared {
		patch[annoLastFailureTime] = nil
	} else if phase == "Failed" || phase == "Error" {
		patch[annoLastFailureTime] = finishedAt.Format(time.RFC3339)
	}
	srv.persistMetricsState(ctx, appNamespace, appName, phase, result)
	log.Printf("dora-exporter: %s/%s: confirmed %s via informer (same-cluster env)", app.GetNamespace(), app.GetName(), phase)

	if err := patchAnnotations(ctx, dynClient, app.GetNamespace(), app.GetName(), patch); err != nil {
		log.Printf("dora-exporter: %s/%s: failed to clear tracking annotations: %v", app.GetNamespace(), app.GetName(), err)
	}
}

// outcomeResult reports exactly what recordOutcome observed, so persistMetricsState can
// durably record the same values it just fed into the live Prometheus collectors -
// rather than re-deriving them from the raw inputs a second time and risking the two
// falling out of sync with each other.
type outcomeResult struct {
	LastFailureCleared bool
	LeadTimeSeconds    float64
	HasLeadTime        bool
	MTTRSeconds        float64
	HasMTTR            bool
}

// recordOutcome is the one piece of actual DORA business logic, shared by both input
// paths described in this file's own header. lastFailureTimeStr is only ever non-empty
// on the informer path (path 1) - path 2 always passes "", which simply means an
// eventual success for a cluster-mapped app never produces an MTTR sample (HasMTTR
// stays false) - a known, deliberate gap (this file's header), not a silent one:
// there's no live Application object on a remote cluster for this service to persist
// "last failure time" onto between separate HTTP calls, and an in-memory map would be
// wrong the moment this Deployment runs more than one replica or restarts - not worth
// that fragility for a best-effort, already-`_experimental` metric.
func recordOutcome(appNamespace, appName, phase string, finishedAt time.Time, flowStartTimeStr, lastFailureTimeStr string) outcomeResult {
	switch phase {
	case "Succeeded":
		releasesTotal.WithLabelValues(appNamespace, appName, "succeeded").Inc()
		deploymentsTotal.WithLabelValues(appNamespace, appName).Inc()

		var result outcomeResult
		if flowStart, err := parseTime(flowStartTimeStr); err == nil {
			leadTime := finishedAt.Sub(flowStart).Seconds()
			leadTimeSeconds.WithLabelValues(appNamespace, appName).Observe(leadTime)
			result.LeadTimeSeconds = leadTime
			result.HasLeadTime = true
		} else {
			log.Printf("dora-exporter: %s/%s: unparseable flow-start-time %q, skipping lead-time sample", appNamespace, appName, flowStartTimeStr)
		}

		if lastFailureTimeStr != "" {
			if lastFailure, err := parseTime(lastFailureTimeStr); err == nil {
				if mttr := finishedAt.Sub(lastFailure).Seconds(); mttr > 0 {
					timeToRestoreSecondsExperimental.WithLabelValues(appNamespace, appName).Observe(mttr)
					result.MTTRSeconds = mttr
					result.HasMTTR = true
				}
			}
			result.LastFailureCleared = true
		}
		return result

	case "Failed", "Error":
		releasesTotal.WithLabelValues(appNamespace, appName, "failed").Inc()
		return outcomeResult{}
	}
	return outcomeResult{}
}

func isTerminalPhase(phase string) bool {
	return phase == "Succeeded" || phase == "Failed" || phase == "Error"
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	return time.Parse(time.RFC3339, s)
}

func patchAnnotations(ctx context.Context, dynClient dynamic.Interface, namespace, name string, annos map[string]interface{}) error {
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": annos,
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = dynClient.Resource(applicationGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}
