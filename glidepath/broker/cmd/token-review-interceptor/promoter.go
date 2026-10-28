package main

// /promoter-token (ADR-0029 phase 2): the GitHub tokens Glidepath's promote pipeline needs, for that pipeline's
// ServiceAccount only. Two purposes, each a single-repo token with the fewest permissions that do the job:
//
//   - dispatch: the Flight promoter App (a second App, owned by the Flight registry's account) on its one
//     workflow repo, actions: write. Starting that workflow is the only way anything writes Flight: GHCR refuses App
//     tokens, so the workflow's own GITHUB_TOKEN does the copy (phase 0).
//   - release-pr: the platform App on a gitops-<app> repo, to read the release PR and post its image-promotion
//     check run and PR comment. The caller's namespace owns no Repository CR, so /github-installation-token's ownership rule
//     cannot apply; this endpoint trusts exactly one ServiceAccount instead, configured, never a namespace.
//
// The promoter App's key is mounted here, next to the platform App's, so no Task pod ever holds either key.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/glidepath/broker/token-review-interceptor/internal/githubapp"

	"k8s.io/client-go/kubernetes"
)

const promoterAudience = "glidepath-promoter"

// promoterCredsDir is a var only so the late-load test can point it at a temp dir.
var promoterCredsDir = "/etc/flight-promoter-app"

type promoterConfig struct {
	// Caller is the one ServiceAccount allowed, as TokenReview names it: system:serviceaccount:<ns>:<name>.
	Caller string
	// DispatchRepo is <owner>/<repo> of the promoter workflow.
	DispatchRepo string
	// App holds the promoter App's credentials; nil when not mounted (dispatch then answers 503).
	App *githubapp.Creds
}

type promoterTokenRequest struct {
	Purpose string `json:"purpose"`
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
}

type promoterScope struct {
	UsePromoterApp bool
	Owner, Repo    string
	Permissions    map[string]string
}

var (
	gitopsRepoPattern = regexp.MustCompile(`^gitops-[a-z0-9][a-z0-9-]*$`)
	ownerPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
)

// scopeFor is the whole authorization decision, pure so it is tested as a table.
func scopeFor(cfg promoterConfig, caller string, req promoterTokenRequest) (promoterScope, int, error) {
	if cfg.Caller == "" {
		return promoterScope{}, http.StatusServiceUnavailable, fmt.Errorf("promoter tokens are not configured (PROMOTER_CALLER unset)")
	}
	if caller != cfg.Caller {
		return promoterScope{}, http.StatusForbidden, fmt.Errorf("%s is not the promoter", caller)
	}
	switch req.Purpose {
	case "dispatch":
		owner, repo, ok := strings.Cut(cfg.DispatchRepo, "/")
		if !ok || owner == "" || repo == "" {
			return promoterScope{}, http.StatusServiceUnavailable, fmt.Errorf("PROMOTER_DISPATCH_REPO %q is not <owner>/<repo>", cfg.DispatchRepo)
		}
		if cfg.App == nil {
			return promoterScope{}, http.StatusServiceUnavailable, fmt.Errorf("the promoter App's credentials are not mounted at %s", promoterCredsDir)
		}
		return promoterScope{UsePromoterApp: true, Owner: owner, Repo: repo,
			Permissions: map[string]string{"actions": "write"}}, http.StatusOK, nil
	case "release-pr":
		if !ownerPattern.MatchString(req.Owner) || !gitopsRepoPattern.MatchString(req.Repo) {
			return promoterScope{}, http.StatusForbidden, fmt.Errorf("release-pr tokens are for gitops-<app> repos only, not %s/%s", req.Owner, req.Repo)
		}
		return promoterScope{Owner: req.Owner, Repo: req.Repo,
			Permissions: map[string]string{"checks": "write", "contents": "read", "pull_requests": "write"}}, http.StatusOK, nil
	default:
		return promoterScope{}, http.StatusBadRequest, fmt.Errorf("purpose must be dispatch or release-pr, not %q", req.Purpose)
	}
}

// loadPromoter reads the configuration at startup. A missing key mount is not fatal: the broker's other endpoints keep
// working, dispatch answers 503 with the reason, and every request retries the load (withApp), because the mount is
// optional and its Secret usually syncs after the pods start (first seen 2026-10-27).
func loadPromoter() promoterConfig {
	cfg := promoterConfig{Caller: os.Getenv("PROMOTER_CALLER"), DispatchRepo: os.Getenv("PROMOTER_DISPATCH_REPO")}
	if cfg.Caller == "" {
		return cfg
	}
	creds, err := githubapp.LoadCredsFrom(promoterCredsDir)
	if err != nil {
		log.Printf("promoter App credentials not loaded (dispatch will answer 503): %v", err)
		return cfg
	}
	cfg.App = creds
	return cfg
}

// withApp returns cfg with the promoter App's credentials, loading them now if startup could not: the kubelet fills an
// optional Secret volume once the Secret exists, without a restart.
func (l *promoterLoader) withApp() promoterConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cfg.Caller != "" && l.cfg.App == nil {
		if creds, err := githubapp.LoadCredsFrom(promoterCredsDir); err == nil {
			l.cfg.App = creds
			log.Printf("promoter App credentials loaded from %s", promoterCredsDir)
		}
	}
	return l.cfg
}

type promoterLoader struct {
	mu  sync.Mutex
	cfg promoterConfig
}

func handlePromoterToken(clientset kubernetes.Interface, platformApp *githubapp.Creds, startup promoterConfig) http.HandlerFunc {
	loader := &promoterLoader{cfg: startup}
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := loader.withApp()
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: msg})
		}

		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			fail(http.StatusUnauthorized, "missing or malformed Authorization: Bearer <token> header")
			return
		}
		caller, authErr := verifyCallerUsername(clientset, token, promoterAudience)
		if authErr != "" {
			fail(http.StatusUnauthorized, authErr)
			return
		}
		var req promoterTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(http.StatusBadRequest, "request body must be {\"purpose\":..., \"owner\":..., \"repo\":...}")
			return
		}
		scope, code, err := scopeFor(cfg, caller, req)
		if err != nil {
			fail(code, err.Error())
			return
		}

		app := platformApp
		if scope.UsePromoterApp {
			app = cfg.App
		}
		appJWT, err := app.SignAppJWT()
		if err != nil {
			fail(http.StatusInternalServerError, "signing App JWT: "+err.Error())
			return
		}
		installationID, err := githubapp.InstallationIDForRepo(appJWT, scope.Owner, scope.Repo)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		instToken, expiresAt, err := githubapp.MintScopedToken(appJWT, installationID, scope.Repo, scope.Permissions)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("promoter token: %s for %s/%s (%v)", req.Purpose, scope.Owner, scope.Repo, scope.Permissions)
		_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Token: instToken, ExpiresAt: expiresAt})
	}
}
