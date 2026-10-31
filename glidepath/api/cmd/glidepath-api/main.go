// glidepath-api: Glidepath's API for people, Tower and agents (docs/admin/api.md).
//
// Phases 1 and 2 of the Glidepath API spec are reads: runs and their logs from Tekton Results,
// release records from the app's -cicd namespace, an image's signatures and attestations from GHCR
// re-verified against the cluster's Fulcio root and Rekor, test runs from Testkube, and the
// contract. Phase 3's first writes (tier T1) re-run and cancel a PipelineRun in the app's namespace.
// Phase 4's (tier T2, propose) promote to a Flight environment and roll one back, by starting the
// app's own release Pipeline, whose PR a human merges. Every call is authenticated with the
// cluster's TokenReview and authorized with a SubjectAccessReview in the app's namespace.
//
// Results is read through glidepath's own tekton-results-relay (charts/glidepath-control-plane),
// the hop Tower used itself until P6: the relay holds the Results read grant, and this service decides who
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
//	GITHUB_OWNER        the GitHub owner of the apps' repos, for promote and rollback (the chart's githubOwner)
//	BROKER_URL          the token broker, for per-request GitHub read tokens (default http://cdevents-broker-auth.platform-system.svc.cluster.local:8080)
//	BROKER_TOKEN_PATH   this pod's projected ServiceAccount token for the broker, audience github-read-token
//	                    (default /var/run/secrets/glidepath/broker/token)
//
// Phase 6b's live reads (internal/live) need no configuration: PipelineRuns, TaskRuns and pod logs as
// the cluster holds them now, with this pod's own identity, for Tower's views of runs in flight.
//
// Phase 5's event stream (GET /v1/events, internal/events):
//
//	EVENTS_MAX            events each replica buffers (default 2000)
//	EVENTS_MAX_AGE        and for how long at most (default 24h)
//	EVENTS_MAX_STREAMS    concurrent streams per replica (default 100)
//	EVENTS_HEARTBEAT      comment line on an idle stream (default 15s)
//	EVENTS_RESUME_GRACE   how far before a Last-Event-ID a resume also replays (default 10s)
//	EVENTS_AUDIENCE       the broker's token audience the tap re-verifies (default cdevents-broker)
//	EVENTS_RELAYS         comma-separated usernames that forward events for any app
//	                      (default system:serviceaccount:platform-system:glidepath-relay)
//	POD_IP, POD_NAMESPACE this pod (downward API): peers are the glidepath-api Service's other endpoints
//	EVENTS_PEER_TOKEN_PATH this pod's projected token for its peers, audience glidepath-api-peer
//	                      (default /var/run/secrets/glidepath/peer/token)
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/auth"
	"github.com/glidepath/api/internal/cicd"
	"github.com/glidepath/api/internal/environments"
	"github.com/glidepath/api/internal/events"
	"github.com/glidepath/api/internal/evidence"
	"github.com/glidepath/api/internal/explain"
	"github.com/glidepath/api/internal/github"
	"github.com/glidepath/api/internal/live"
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

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			log.Fatalf("glidepath-api: %s must be a positive integer", k)
		}
		return n
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			log.Fatalf("glidepath-api: %s must be a positive duration", k)
		}
		return d
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
	// The Flight promoter repository (owner/repo): only its promote.yaml on main signs a promotion
	// attestation that verifies. Unset: promotion attestations show, never verified.
	promotion, err := evidence.NewPromotionTrust(os.Getenv("PROMOTER_REPO"))
	if err != nil {
		log.Fatalf("glidepath-api: %v", err)
	}
	flightOwner, flightCredentials := "", evidence.CredentialSource(nil)
	if repo := os.Getenv("PROMOTER_REPO"); repo != "" {
		flightOwner, _, _ = strings.Cut(repo, "/")
	}
	if ref := os.Getenv("FLIGHT_PULL_SECRET"); ref != "" {
		ns, name, ok := strings.Cut(ref, "/")
		if !ok || ns == "" || name == "" {
			log.Fatalf("glidepath-api: FLIGHT_PULL_SECRET %q is not <namespace>/<name>", ref)
		}
		flightCredentials = evidence.FixedSecretCredentials(cs, ns, name)
	}
	// The GitHub owner of every app's repos (the chart's githubOwner). Unset: promote and rollback fail at their
	// first GitHub read; nothing else needs it.
	githubOwner := os.Getenv("GITHUB_OWNER")
	broker := &github.Broker{
		URL:       env("BROKER_URL", "http://cdevents-broker-auth.platform-system.svc.cluster.local:8080"),
		TokenPath: env("BROKER_TOKEN_PATH", "/var/run/secrets/glidepath/broker/token"),
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
	logger := server.NewLogger()
	store := &events.Store{
		Max:        envInt("EVENTS_MAX", 2000),
		MaxAge:     envDuration("EVENTS_MAX_AGE", 24*time.Hour),
		MaxStreams: envInt("EVENTS_MAX_STREAMS", 100),
	}
	relays := map[string]bool{}
	for _, r := range strings.Split(env("EVENTS_RELAYS", "system:serviceaccount:platform-system:glidepath-relay"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			relays[r] = true
		}
	}
	podNS := env("POD_NAMESPACE", "platform-system")
	reviewer := events.NewKubeReviewer(cs)
	var peers *events.Peers
	if ip := os.Getenv("POD_IP"); ip != "" {
		peers = &events.Peers{
			CS: cs, Namespace: podNS, Service: "glidepath-api", Port: 8080, SelfIP: ip,
			TokenPath: env("EVENTS_PEER_TOKEN_PATH", "/var/run/secrets/glidepath/peer/token"),
			HTTP:      &http.Client{Timeout: 10 * time.Second},
			Log:       logger,
			TTL:       5 * time.Second,
		}
	}
	liveRuns := &live.Client{Store: live.NewKubeStore(dyn, cs)}
	s := &server.Server{
		Auth: &auth.Auth{Reviewer: auth.NewKubeReviewer(cs), Delegates: delegates},
		Apps: &apps.Resolver{CS: cs, TTL: 5 * time.Minute},
		Results: &results.Client{
			Base: env("RESULTS_URL", "http://tekton-results-relay.platform-system.svc.cluster.local:8080"),
			HTTP: &http.Client{Timeout: 30 * time.Second},
		},
		Releases: releases.Reader{CS: cs},
		// Evidence reads each app's registry-credentials Secret per request (never cached), the Fulcio
		// root ConfigMap and Rekor (cached 10 minutes, as Tower does). Promotion attestations verify
		// against the embedded public Sigstore root instead.
		Evidence: &evidence.Reader{
			CS:          cs,
			Credentials: evidence.SecretCredentials(cs),
			RegistryURL: "https://" + evidence.Registry,
			RekorURL:    env("REKOR_URL", "http://rekor-server.rekor-system.svc.cluster.local"),
			HTTP:        &http.Client{Timeout: 30 * time.Second},
			Log:         logger,
			Promotion:   promotion,
			// Flight images are read with the Flight registry's read credential (the chart's
			// FLIGHT_PULL_SECRET, <namespace>/<name>); the Flight owner is the promoter repository's
			// owner, the only account its workflow can push packages to.
			FlightOwner:       flightOwner,
			FlightCredentials: flightCredentials,
		},
		Tests: &testkube.Client{
			Base: env("TESTKUBE_URL", "http://testkube-api-server.testkube.svc.cluster.local:8088"),
			HTTP: &http.Client{Timeout: 30 * time.Second},
			Log:  logger,
		},
		Runs: &pipelineruns.Client{Store: pipelineruns.NewKubeStore(dyn)},
		// Live runs, TaskRuns and step logs straight from the API server (phase 6b: Tower's live views moved
		// here from its own Kubernetes proxy reads). Lists are cached 2 s; logs never.
		Live: liveRuns,
		// GET /v1/apps/{app}/live/stream: one list per watched namespace every 2 s (through Live's cache), shared by
		// every viewer of it on this replica.
		LiveHub: &live.Hub{Client: liveRuns, MaxStreams: envInt("LIVE_MAX_STREAMS", 200), Log: logger},
		// Promote and rollback read GitHub with a token the broker mints per request and per repo (read only, that
		// one repo), and start the app's release Pipeline from its own TriggerTemplate.
		Envs: &environments.Client{
			Owner: githubOwner,
			GitHub: func(app string) environments.GitHub {
				return broker.Session(app)
			},
			Templates: environments.NewKubeTemplates(dyn),
			Runs:      pipelineruns.NewKubeStore(dyn),
			Records:   releases.Reader{CS: cs},
		},
		// A cicd.yaml change, validated by the glidepath-validate sidecar (`glidepath validate --serve`, the chart's
		// cicdValidatorImage) on the pod's loopback: one copy of the rules, the one the PR check runs.
		Cicd: &cicd.Client{
			Owner: githubOwner,
			GitHub: func(app string) cicd.Source {
				return broker.Session(app)
			},
			Validator: &cicd.Validator{
				URL:  env("VALIDATOR_URL", "http://127.0.0.1:8081"),
				HTTP: &http.Client{Timeout: 20 * time.Second},
			},
		},
		Events: store,
		Tap: &events.Tap{
			Verifier: &events.Verifier{Reviewer: reviewer, Audience: env("EVENTS_AUDIENCE", "cdevents-broker"), Relays: relays},
			Store:    store, Peers: peers, Log: logger,
		},
		Peer: &events.PeerHandler{
			Reviewer: reviewer, Audience: "glidepath-api-peer",
			Identity: "system:serviceaccount:" + podNS + ":glidepath-api",
			Store:    store, Log: logger,
		},
		Heartbeat:   envDuration("EVENTS_HEARTBEAT", 15*time.Second),
		ResumeGrace: envDuration("EVENTS_RESUME_GRACE", 10*time.Second),
		Contract:    contract,
		Known:       known,
		Version:     version,
		Log:         logger,
		Now:         time.Now,
	}
	srv := &http.Server{Addr: env("LISTEN", ":8080"), Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// Event streams never finish on their own; end them when the server stops.
	srv.RegisterOnShutdown(store.Close)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("glidepath-api: %v", err)
		}
	}()
	log.Printf("glidepath-api: listening on %s, %d delegate(s)", srv.Addr, len(delegates))
	go warm(context.Background(), cs, s.Apps, s.Results, time.Now)
	if peers != nil {
		// Copy what the other replicas hold, now and once more after this one is surely in their
		// peer lists, so a deploy doesn't empty the buffer resumes read from.
		go func() {
			for _, wait := range []time.Duration{2 * time.Second, 30 * time.Second} {
				time.Sleep(wait)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				n := peers.Backfill(ctx, store)
				cancel()
				logger.Info("events.backfill", "added", n)
			}
		}()
	} else {
		logger.Warn("events: POD_IP unset; this replica forwards no events to peers")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
