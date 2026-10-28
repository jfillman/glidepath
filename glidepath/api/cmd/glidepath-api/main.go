// glidepath-api: Glidepath's API for people, Tower and agents (docs/admin/api.md).
//
// Phases 1 and 2 of the Glidepath API spec are reads: runs and their logs from Tekton Results,
// release records from the app's -cicd namespace, an image's signatures and attestations from GHCR
// re-verified against the cluster's Fulcio root and Rekor, test runs from Testkube, and the
// contract. Phase 3's first writes (tier T1) re-run and cancel a PipelineRun in the app's namespace.
// Every call is authenticated with the cluster's TokenReview and authorized with a
// SubjectAccessReview in the app's namespace.
//
// Results is read through glidepath's own tekton-results-relay (charts/glidepath-control-plane),
// the hop Tower already uses: the relay holds the Results read grant, and this service decides who
// may ask before it asks.
//
// Configuration, all by environment:
//
//	LISTEN              address to serve on (default :8080)
//	CONTRACT_PATH       contract/glidepath-contract.json, baked into the image (default /contract/glidepath-contract.json)
//	RESULTS_URL         the Results relay (default http://tekton-results-relay.platform-system.svc.cluster.local:8080)
//	REKOR_URL           Rekor, for evidence (default http://rekor-server.rekor-system.svc.cluster.local)
//	TESTKUBE_URL        Testkube's agent API (default http://testkube-api-server.testkube.svc.cluster.local:8088)
//	DELEGATES           comma-separated usernames that may send X-Glidepath-On-Behalf-Of
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/evidence"
	"github.com/glidepath/api/internal/explain"
	"github.com/glidepath/api/internal/pipelineruns"
	"github.com/glidepath/api/internal/releases"
	"github.com/glidepath/api/internal/results"
	"github.com/glidepath/api/internal/server"
	"github.com/glidepath/api/internal/testkube"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	contract, err := os.ReadFile(env("CONTRACT_PATH", "/contract/glidepath-contract.json"))
	if err != nil {
		log.Fatalf("glidepath-api: reading the contract: %v", err)
	}
	known, version, err := explain.Load(contract)
	if err != nil {
		log.Fatalf("glidepath-api: %v", err)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("glidepath-api: in-cluster config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("glidepath-api: kubernetes client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("glidepath-api: dynamic client: %v", err)
	}
	delegates := map[string]bool{}
	for _, d := range strings.Split(os.Getenv("DELEGATES"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			delegates[d] = true
		}
	}
	logger := server.NewLogger()
	s := &server.Server{
		Auth: &auth.Auth{Reviewer: auth.NewKubeReviewer(cs), Delegates: delegates},
		Apps: &apps.Resolver{CS: cs, TTL: 5 * time.Minute},
		Results: &results.Client{
			Base: env("RESULTS_URL", "http://tekton-results-relay.platform-system.svc.cluster.local:8080"),
			HTTP: &http.Client{Timeout: 30 * time.Second},
		},
		Releases: releases.Reader{CS: cs},
		// Evidence reads each app's registry-credentials Secret per request (never cached), the Fulcio
		// root ConfigMap and Rekor (cached 10 minutes, as Tower does).
		Evidence: &evidence.Reader{
			CS:          cs,
			Credentials: evidence.SecretCredentials(cs),
			RegistryURL: "https://" + evidence.Registry,
			RekorURL:    env("REKOR_URL", "http://rekor-server.rekor-system.svc.cluster.local"),
			HTTP:        &http.Client{Timeout: 30 * time.Second},
			Log:         logger,
		},
		Tests: &testkube.Client{
			Base: env("TESTKUBE_URL", "http://testkube-api-server.testkube.svc.cluster.local:8088"),
			HTTP: &http.Client{Timeout: 30 * time.Second},
			Log:  logger,
		},
		Runs:     &pipelineruns.Client{Store: pipelineruns.NewKubeStore(dyn)},
		Contract: contract,
		Known:    known,
		Version:  version,
		Log:      logger,
		Now:      time.Now,
	}
	srv := &http.Server{Addr: env("LISTEN", ":8080"), Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("glidepath-api: %v", err)
		}
	}()
	log.Printf("glidepath-api: listening on %s, %d delegate(s)", srv.Addr, len(delegates))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
