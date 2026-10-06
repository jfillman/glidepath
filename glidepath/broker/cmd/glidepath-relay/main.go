// The release relay (ADR-0021). Receives facts - Argo Rollouts notifications from each upper
// cluster at /facts/<cluster>, Argo CD sync notifications at /argocd/<cluster> - joins each to the
// release record ConfigMap on this cluster (facts.go), runs it through the release state machine
// (reducer.go), and forwards the resulting CDEvents to the broker.
//
// Auth is a shared secret per upstream cluster (the <cluster> in the URL, resolved live via the
// cluster-registry ConfigMap), not the broker's own TokenReview - TokenReview can't validate a
// token issued by a different cluster's API server. Trust boundary: the secret proves the call
// came from cluster X, not that the payload's claims are accurate. The release-id's cluster must
// match the authenticated one, so a compromised cluster can report on its own releases only.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	platformNamespace  = "platform-system"
	registryConfigMap  = "cluster-registry"
	brokerTokenPath    = "/var/run/secrets/platform/broker-token"
	requestBodyMaxSize = 1 << 16 // a handful of short fields, never legitimately larger
)

// Mirrors cluster-registry.yaml's per-cluster JSON value - only the field this service needs.
type registryEntry struct {
	RelaySecretName string `json:"relaySecretName"`
}

func main() {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("loading in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("building clientset: %v", err)
	}

	brokerURL := os.Getenv("CDEVENTS_BROKER_URL")
	if brokerURL == "" {
		log.Fatal("CDEVENTS_BROKER_URL must be set")
	}

	h := &handler{clientset: clientset, brokerURL: brokerURL, httpClient: &http.Client{Timeout: 10 * time.Second}, factsMode: os.Getenv("FACTS_MODE")}
	h.forward = h.forwardToBroker

	mux := http.NewServeMux()
	mux.HandleFunc("/facts/", h.handleFacts) // ADR-0021; see facts.go
	mux.HandleFunc("/argocd/", h.handleArgoFacts)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Println("glidepath-relay: listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

type handler struct {
	clientset  kubernetes.Interface
	brokerURL  string
	httpClient *http.Client
	// factsMode is "shadow" (the code default) or "emit" (the chart default); see facts.go.
	factsMode string
	// forward sends a CDEvent to the broker; a field so tests can stub it.
	forward func(ctx context.Context, body []byte) error
}

// authenticate resolves cluster -> relaySecretName live off the cluster-registry
// ConfigMap, then compares the caller's bearer token against that Secret's "token" key.
// Not cached - this isn't a hot path, and a live lookup lets a rotated/revoked secret
// take effect without a relay restart.
func (h *handler) authenticate(ctx context.Context, cluster, authHeader string) error {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return fmt.Errorf("missing or malformed Authorization header")
	}
	provided := strings.TrimPrefix(authHeader, prefix)

	cm, err := h.clientset.CoreV1().ConfigMaps(platformNamespace).Get(ctx, registryConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading cluster-registry ConfigMap: %w", err)
	}
	raw, ok := cm.Data[cluster]
	if !ok {
		return fmt.Errorf("cluster %q has no cluster-registry entry", cluster)
	}
	var entry registryEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return fmt.Errorf("cluster-registry entry for %q is not valid JSON: %w", cluster, err)
	}
	if entry.RelaySecretName == "" {
		return fmt.Errorf("cluster %q's registry entry has no relaySecretName", cluster)
	}

	secret, err := h.clientset.CoreV1().Secrets(platformNamespace).Get(ctx, entry.RelaySecretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading relay secret %q for cluster %q: %w", entry.RelaySecretName, cluster, err)
	}
	expected, ok := secret.Data["token"]
	if !ok {
		return fmt.Errorf("secret %q has no 'token' key", entry.RelaySecretName)
	}

	if subtle.ConstantTimeCompare(expected, []byte(provided)) != 1 {
		return fmt.Errorf("token mismatch")
	}
	return nil
}

// forwardToBroker POSTs a CDEvent to the broker,
// swapping in this pod's own projected SA token (verified by the broker's existing
// TokenReview interceptor) in place of the shared-secret auth this call arrived with.
func (h *handler) forwardToBroker(ctx context.Context, body []byte) error {
	tokenBytes, err := os.ReadFile(brokerTokenPath)
	if err != nil {
		return fmt.Errorf("reading broker token: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.brokerURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))
	httpReq.Header.Set("Content-Type", "application/cloudevents+json")

	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("posting to broker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("broker returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
