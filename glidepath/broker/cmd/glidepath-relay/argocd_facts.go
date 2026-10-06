package main

// POST /argocd/<cluster> - ADR-0021 phase 2. The Argo CD notifications controller on an upper
// cluster reports a sync that is failing: the Rollouts engine never sees it (the Rollout is
// not touched, or the sync is rejected before it), so without this a release that never
// started looks identical to one that is still queued.
//
// The tenant Applications retry (limit 5, backoff to 10 minutes, so about 15 minutes to a
// final Failed), and phase 0 showed Application.phase stays Running through the retries, so
// the notification config fires on the first failed attempt (retryCount > 0) and again when
// the sync finally fails. The body names the Application, its hangar.io/app and hangar.io/env
// labels, the phase, the retry count and the error message.
//
// The release is found by (app, env, cluster) among that app's records, newest merged,
// progressing or sync-failed one. A sync with no such release (someone edited values.yaml
// directly) is not a release failing, and is ignored.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var labelValue = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type argoFact struct {
	Source     string `json:"source"`
	Name       string `json:"name"`
	App        string `json:"app"`
	Env        string `json:"env"`
	Phase      string `json:"phase"`
	RetryCount int    `json:"retryCount"`
	Message    string `json:"message"`
}

func (h *handler) handleArgoFacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cluster := strings.TrimPrefix(r.URL.Path, "/argocd/")
	if cluster == "" || strings.Contains(cluster, "/") {
		http.Error(w, "cluster name required in path: /argocd/<cluster>", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if err := h.authenticate(ctx, cluster, r.Header.Get("Authorization")); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, requestBodyMaxSize))
	if err != nil {
		http.Error(w, "reading request body", http.StatusBadRequest)
		return
	}
	var f argoFact
	if err := json.Unmarshal(body, &f); err != nil {
		http.Error(w, fmt.Sprintf("invalid fact JSON: %v", err), http.StatusBadRequest)
		return
	}
	if !labelValue.MatchString(f.App) || !labelValue.MatchString(f.Env) {
		// Not a tenant Application (no hangar.io/app or env label): nothing to attribute.
		w.Header().Set("X-Fact-Ignored", "unlabelled application")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	failing := f.Phase == "Failed" || f.Phase == "Error" || (f.Phase == "Running" && f.RetryCount > 0)
	if !failing {
		w.Header().Set("X-Fact-Ignored", "not a failure")
		w.WriteHeader(http.StatusAccepted)
		return
	}

	load := func() (*record, *corev1.ConfigMap, error) {
		ns := "app-" + f.App + "-cicd"
		list, err := h.clientset.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf(
			"hangar.io/subcomponent=release-tracking,hangar.io/app=%s,hangar.io/env=%s,hangar.io/cluster=%s", f.App, f.Env, cluster)})
		if err != nil {
			return nil, nil, ignoreErr("cannot list records: " + err.Error())
		}
		var pick *corev1.ConfigMap
		for i := range list.Items {
			c := &list.Items[i]
			switch c.Data[keyState] {
			case stMerged, stProgressing, stSyncFailed:
			default:
				continue
			}
			if pick == nil || parseTime(c.Data["prCreatedAt"]).After(parseTime(pick.Data["prCreatedAt"])) {
				pick = c
			}
		}
		if pick == nil {
			return nil, nil, ignoreErr("no merged release for this application in flight")
		}
		d := pick.Data
		rec := &record{
			AppNamespace: d["appNamespace"], AppName: d["appName"], Env: d["env"], Cluster: d["cluster"],
			GitURL: d["gitUrl"], GitRevision: d["gitRevision"], FlowStartTime: d["flowStartTime"],
			ConfigJSON: d["configJson"], ChainID: strings.TrimPrefix(pick.Name, recordPrefix),
		}
		if rec.AppNamespace != ns || rec.AppName == "" || rec.Env != f.Env || rec.Cluster != cluster {
			return nil, nil, ignoreErr("record does not match the application")
		}
		return rec, pick, nil
	}
	h.apply(w, ctx, cluster, "argocd:"+f.Name, "", load, func(st *relState, now time.Time) decision {
		return reduceSyncFailed(st, f.Message, now)
	})
}
