package main

// /github-read-token (Glidepath API spec phase 4): a read-only GitHub token on one app's own repo, for glidepath-api.
//
// glidepath-api's promote and rollback (docs/admin/api.md#promote-and-roll-back) read GitHub the way Tower's do: the
// app's cicd.yaml (its release order and tiers), an environment's release file in gitops-<app> (what to promote), a
// commit (the image tag's sha, expanded), and the open release PR (so a retry finds it instead of starting a second
// release). glidepath-api holds no GitHub credential. It calls this endpoint once per repo per request, uses the
// token for that request only, and never caches, logs or returns it.
//
// The rules, all checked here, never by the caller:
//
//   - The caller is one of a configured list of ServiceAccounts (GITHUB_READ_CALLERS, the chart's githubRead.callers;
//     default glidepath-api's own), authenticated by TokenReview with this endpoint's audience. An app pipeline's
//     ServiceAccount is not on it: pipelines have /github-installation-token, scoped to their own namespace.
//   - The repo is the app's own: <app>, gitops-<app>, or an InfraService's gitops-infra-<app>, and a PaC Repository CR
//     in the app's -cicd namespace maps to it (appNamespaceOwnsRepo's rule) with a spec.url under the requested owner.
//     So a caller on the list still cannot read a repo no app on this cluster owns, or another org's copy of one.
//   - The token is down-scoped twice: repositories [repo], and permissions contents, pull_requests and metadata, all
//     read (githubapp.RepoReadPermissions). It cannot push, open or merge a PR, or touch any other repo.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/glidepath/broker/token-review-interceptor/internal/githubapp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const githubReadAudience = "github-read-token"

var (
	githubReadAppRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	githubReadRepoRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

type githubReadRequest struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	App   string `json:"app"`
}

// loadGitHubReadCallers reads GITHUB_READ_CALLERS: comma-separated usernames as TokenReview names them
// (system:serviceaccount:<ns>:<name>). Empty: the endpoint answers 503.
func loadGitHubReadCallers() map[string]bool {
	callers := map[string]bool{}
	for _, c := range strings.Split(os.Getenv("GITHUB_READ_CALLERS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			callers[c] = true
		}
	}
	return callers
}

// githubReadCheck is the caller and request half of the decision, pure so it is tested as a table: who may ask, and
// which repo names can be an app's own at all. The Repository CR half is githubReadOwns.
func githubReadCheck(callers map[string]bool, caller string, req githubReadRequest) (int, error) {
	if len(callers) == 0 {
		return http.StatusServiceUnavailable, fmt.Errorf("GitHub read tokens are not configured (GITHUB_READ_CALLERS unset)")
	}
	if !callers[caller] {
		return http.StatusForbidden, fmt.Errorf("%s may not ask for GitHub read tokens", caller)
	}
	if !ownerPattern.MatchString(req.Owner) || !githubReadRepoRE.MatchString(req.Repo) || !githubReadAppRE.MatchString(req.App) {
		return http.StatusBadRequest, fmt.Errorf("request body must be {\"owner\":..., \"repo\":..., \"app\":...}, each a plain name")
	}
	if req.Repo != req.App && req.Repo != "gitops-"+req.App && req.Repo != "gitops-infra-"+req.App {
		return http.StatusForbidden, fmt.Errorf("%s is not %s's own repo (%[2]s, gitops-%[2]s or gitops-infra-%[2]s)", req.Repo, req.App)
	}
	return http.StatusOK, nil
}

// githubReadOwns checks the app's -cicd namespace (app- or infra-) has a PaC Repository CR that maps to repo, by
// appNamespaceOwnsRepo's name rule, whose spec.url is under owner.
func githubReadOwns(dynClient dynamic.Interface, req githubReadRequest) error {
	for _, ns := range []string{"app-" + req.App + "-cicd", "infra-" + req.App + "-cicd"} {
		list, err := dynClient.Resource(repositoryGVR).Namespace(ns).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			continue // no such namespace, or nothing readable: the other type, or a refusal below
		}
		for _, item := range list.Items {
			name := item.GetName()
			if name == "" || (name != req.Repo && "gitops-"+name != req.Repo) {
				continue
			}
			url, _, _ := unstructured.NestedString(item.Object, "spec", "url")
			if m := githubRepoRE.FindStringSubmatch(url); m != nil && strings.EqualFold(m[1], req.Owner) {
				return nil
			}
		}
	}
	return fmt.Errorf("no Repository CR in app-%[1]s-cicd or infra-%[1]s-cicd maps to %s/%s", req.App, req.Owner, req.Repo)
}

func handleGitHubReadToken(clientset kubernetes.Interface, dynClient dynamic.Interface, appCreds *githubapp.Creds, callers map[string]bool) http.HandlerFunc {
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
		caller, authErr := verifyCallerUsername(clientset, token, githubReadAudience)
		if authErr != "" {
			fail(http.StatusUnauthorized, authErr)
			return
		}
		var req githubReadRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			fail(http.StatusBadRequest, "request body must be {\"owner\":..., \"repo\":..., \"app\":...}")
			return
		}
		if code, err := githubReadCheck(callers, caller, req); err != nil {
			fail(code, err.Error())
			return
		}
		if err := githubReadOwns(dynClient, req); err != nil {
			fail(http.StatusForbidden, err.Error())
			return
		}
		appJWT, err := appCreds.SignAppJWT()
		if err != nil {
			fail(http.StatusInternalServerError, "signing App JWT: "+err.Error())
			return
		}
		installationID, err := githubapp.InstallationIDForRepo(appJWT, req.Owner, req.Repo)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		instToken, expiresAt, err := githubapp.MintRepoReadToken(appJWT, installationID, req.Repo)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("github read token: %s for %s/%s (app %s)", caller, req.Owner, req.Repo, req.App)
		_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Token: instToken, ExpiresAt: expiresAt})
	}
}
