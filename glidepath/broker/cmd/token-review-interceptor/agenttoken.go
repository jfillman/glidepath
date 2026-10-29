package main

// /agent-installation-token (Autopilot AP-B1, hangar decision D4): a GitHub token for the agents' own GitHub App,
// on one app's own repo, for Clearance's repo.pr.open backend.
//
// Agents never hold a GitHub credential. Clearance decides every PR (its tool policy, the agent definition's
// repos.allow, the path and field rules), then calls this endpoint once for the repo, opens the PR with the token
// for that call only, and never caches, logs or returns it. The agents' App is a different App from the platform's
// (which pipelines use) and from Clearance's provenance App, so a PR it opens is attributable to an agent.
//
// The rules, all checked here, never by the caller:
//
//   - The caller is one of a configured list of ServiceAccounts (AGENT_TOKEN_CALLERS, the chart's
//     agentToken.callers; Clearance's own), authenticated by TokenReview with this endpoint's audience.
//   - The repo is an onboarded app's own, by exactly /github-read-token's rule (githubReadCheck's names and
//     githubReadOwns' Repository CR): so even if the App is installed on a cluster or platform repo, and even if
//     Clearance were wrong, nothing is minted for it. Clearance's repos.allow is the first check; this is the second.
//   - The token is down-scoped twice: repositories [repo], and permissions contents and pull_requests write,
//     metadata read. Never workflows, so a push touching .github/workflows is refused by GitHub, and never checks,
//     administration or merge-bypass: the App opens PRs a person merges.
//
// The App's id and key are mounted at /etc/agent-app from the agents' Infisical keys (agentToken.appIdKey and
// privateKeyKey). The mount is optional and loaded late, like the promoter's: until it syncs, this answers 503.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/glidepath/broker/token-review-interceptor/internal/githubapp"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const agentTokenAudience = "agent-installation-token"

// agentAppDir is a var only so tests can point it at a temp dir.
var agentAppDir = "/etc/agent-app"

// agentTokenPermissions is the whole grant: open a PR from a branch the agent pushes. No workflows, no checks.
func agentTokenPermissions() map[string]string {
	return map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}
}

// loadAgentTokenCallers reads AGENT_TOKEN_CALLERS: comma-separated usernames as TokenReview names them. Empty: 503.
func loadAgentTokenCallers() map[string]bool {
	callers := map[string]bool{}
	for _, c := range strings.Split(os.Getenv("AGENT_TOKEN_CALLERS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			callers[c] = true
		}
	}
	return callers
}

// agentTokenCheck is the caller and request half of the decision, pure so it is tested as a table. The repo-name rule
// is /github-read-token's; the Repository CR half is githubReadOwns.
func agentTokenCheck(callers map[string]bool, caller string, req githubReadRequest) (int, error) {
	if len(callers) == 0 {
		return http.StatusServiceUnavailable, fmt.Errorf("agent tokens are not configured (AGENT_TOKEN_CALLERS unset)")
	}
	if !callers[caller] {
		return http.StatusForbidden, fmt.Errorf("%s may not ask for agent tokens", caller)
	}
	// githubReadCheck with a caller list of exactly this caller: its name rules, not its caller rule.
	return githubReadCheck(map[string]bool{caller: true}, caller, req)
}

type agentAppLoader struct {
	mu   sync.Mutex
	app  *githubapp.Creds
	load func() (*githubapp.Creds, error)
}

// get returns the agents' App credentials, loading them now if they were not there before: the kubelet fills an
// optional Secret volume once the Secret exists, without a restart.
func (l *agentAppLoader) get() (*githubapp.Creds, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app != nil {
		return l.app, nil
	}
	app, err := l.load()
	if err != nil {
		return nil, err
	}
	l.app = app
	log.Printf("agents' App credentials loaded from %s", agentAppDir)
	return app, nil
}

func handleAgentInstallationToken(clientset kubernetes.Interface, dynClient dynamic.Interface, callers map[string]bool) http.HandlerFunc {
	loader := &agentAppLoader{load: func() (*githubapp.Creds, error) { return githubapp.LoadCredsFrom(agentAppDir) }}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: msg})
		}
		if r.Method != http.MethodPost {
			fail(http.StatusMethodNotAllowed, "POST only")
			return
		}
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			fail(http.StatusUnauthorized, "missing or malformed Authorization: Bearer <token> header")
			return
		}
		caller, authErr := verifyCallerUsername(clientset, token, agentTokenAudience)
		if authErr != "" {
			fail(http.StatusUnauthorized, authErr)
			return
		}
		var req githubReadRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			fail(http.StatusBadRequest, "request body must be {\"owner\":..., \"repo\":..., \"app\":...}")
			return
		}
		if code, err := agentTokenCheck(callers, caller, req); err != nil {
			fail(code, err.Error())
			return
		}
		if err := githubReadOwns(dynClient, req); err != nil {
			fail(http.StatusForbidden, err.Error())
			return
		}
		app, err := loader.get()
		if err != nil {
			// The reason names the mount, never its contents.
			fail(http.StatusServiceUnavailable, "the agents' App credentials are not mounted at "+agentAppDir)
			return
		}
		appJWT, err := app.SignAppJWT()
		if err != nil {
			fail(http.StatusInternalServerError, "signing App JWT: "+err.Error())
			return
		}
		installationID, err := githubapp.InstallationIDForRepo(appJWT, req.Owner, req.Repo)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		perms := agentTokenPermissions()
		instToken, expiresAt, err := githubapp.MintScopedToken(appJWT, installationID, req.Repo, perms)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("agent token: %s for %s/%s (app %s) %v", caller, req.Owner, req.Repo, req.App, perms)
		_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Token: instToken, ExpiresAt: expiresAt})
	}
}
