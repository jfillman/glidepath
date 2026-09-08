// Implements the Tekton Triggers ClusterInterceptor webhook contract, registered as
// "cdevents-broker-auth" and referenced from every Application's Trigger CRs.
//
// Authenticates callers of the shared CDEvents broker via each caller's own
// cluster-issued, audience-bound SA token (Kubernetes TokenReview), replacing the old
// JWT-minting-server. On success this sets extensions.app_namespace to the calling SA's
// namespace; each Trigger's CEL filter checks that against the CDEvent's source
// namespace - that check, not network topology, is the real app-isolation boundary
// here. See docs/admin/chaining.md.
//
// Also mints scoped GitHub App installation tokens for the release GitOps PR flow
// (internal/githubapp, handleGitHubInstallationToken below) - an extension of this same
// trusted component rather than a new service, since copying the App's private key into
// every Application namespace would let one compromised Task mint tokens for every
// other Application's repos. cmd/argocd-outcome-relay shares the same githubapp client
// for a different reason (see its own header).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/platform-cicd/broker/token-review-interceptor/internal/githubapp"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The PaC Repository CRD - read-only, used only to check "does this Application actually
// own an app that resolves to the gitops repo it's asking a token for" (see
// handleGitHubInstallationToken). Never written to.
var repositoryGVR = schema.GroupVersionResource{Group: "pipelinesascode.tekton.dev", Version: "v1alpha1", Resource: "repositories"}

type interceptorRequest struct {
	Body              string                 `json:"body"`
	Header            map[string][]string    `json:"header"`
	Extensions        map[string]interface{} `json:"extensions"`
	InterceptorParams map[string]interface{} `json:"interceptor_params"`
}

type status struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type interceptorResponse struct {
	Extensions map[string]interface{} `json:"extensions,omitempty"`
	Continue   bool                   `json:"continue"`
	Status     status                 `json:"status"`
}

// gRPC-style codes used in the Status.Code field of the interceptor contract.
const (
	codeOK              = 0
	codeInvalidArgument = 3
	codeUnauthenticated = 16
)

const defaultAudience = "cdevents-broker"

// Plain HTTP, not HTTPS - deliberate, not an oversight. A cert-manager-issued
// self-signed-CA certificate (matching how Tekton's built-in ClusterInterceptors use
// spec.clientConfig.caBundle) was tried first, but the EventListener sink rejected it
// with "x509: certificate signed by unknown authority" on every call, and neither
// recreating the ClusterInterceptor with caBundle set at creation nor patching the
// sink's trust store worked (the latter blocked by the EventListener CRD's admission
// webhook, and a direct Deployment patch got reverted by the reconciler). Whatever
// makes caBundle work for the built-in interceptors wasn't reachable here.
//
// Not a security downgrade: the real trust boundary is TokenReview-verified caller
// identity, not transport TLS, and this call never leaves the cluster (see
// docs/admin/chaining.md). Revisit if this needs to run somewhere caBundle can work.
func main() {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("loading in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("building clientset: %v", err)
	}
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("building dynamic client: %v", err)
	}

	// Loaded once at startup so a misconfigured github-app-creds mount fails fast,
	// not on the first release PR attempt.
	appCreds, err := githubapp.LoadCreds()
	if err != nil {
		log.Fatalf("loading GitHub App credentials: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handle(clientset))
	mux.HandleFunc("/github-installation-token", handleGitHubInstallationToken(clientset, dynClient, appCreds))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Println("cdevents-broker-auth interceptor listening on :8080 (plain HTTP - see comment above)")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func handle(clientset kubernetes.Interface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req interceptorRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, interceptorResponse{Continue: false, Status: status{Code: codeInvalidArgument, Message: "invalid request body: " + err.Error()}})
			return
		}

		audience, _ := req.InterceptorParams["audience"].(string)
		if audience == "" {
			audience = defaultAudience
		}

		token := bearerToken(firstHeader(req.Header, "Authorization"))
		if token == "" {
			writeJSON(w, interceptorResponse{Continue: false, Status: status{Code: codeUnauthenticated, Message: "missing or malformed Authorization: Bearer <token> header"}})
			return
		}

		appNamespace, authErr := verifyCallerAppNamespace(clientset, token, audience)
		if authErr != "" {
			writeJSON(w, interceptorResponse{Continue: false, Status: status{Code: codeUnauthenticated, Message: authErr}})
			return
		}

		writeJSON(w, interceptorResponse{
			Continue:   true,
			Extensions: map[string]interface{}{"app_namespace": appNamespace},
			Status:     status{Code: codeOK},
		})
	}
}

// verifyCallerAppNamespace is the one TokenReview trust primitive, shared by both the
// CDEvents broker path and handleGitHubInstallationToken below.
func verifyCallerAppNamespace(clientset kubernetes.Interface, token, audience string) (namespace string, errMsg string) {
	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{audience},
		},
	}
	result, err := clientset.AuthenticationV1().TokenReviews().Create(context.Background(), review, metav1.CreateOptions{})
	if err != nil {
		return "", "TokenReview call failed: " + err.Error()
	}
	if !result.Status.Authenticated {
		return "", "token not authenticated: " + result.Status.Error
	}

	// SA-token usernames are "system:serviceaccount:<namespace>:<name>".
	parts := strings.Split(result.Status.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return "", "caller is not a ServiceAccount identity"
	}
	return parts[2], ""
}

func bearerToken(authHeader string) string {
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == authHeader {
		return "" // no "Bearer " prefix present
	}
	return token
}

type githubInstallationTokenRequest struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

type githubInstallationTokenResponse struct {
	Token     string `json:"token,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleGitHubInstallationToken mints a GitHub App installation token scoped to exactly
// one repo, for a caller whose Application namespace genuinely owns that repo - either
// directly (its own app repo) or via the gitops-<app-name> convention. A valid SA token
// alone only proves which Application is asking; the Application also needs a PaC
// Repository CR whose name matches the requested repo. Needs only list RBAC on
// repositories.pipelinesascode.tekton.dev.
func handleGitHubInstallationToken(clientset kubernetes.Interface, dynClient dynamic.Interface, appCreds *githubapp.Creds) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: "missing or malformed Authorization: Bearer <token> header"})
			return
		}
		appNamespace, authErr := verifyCallerAppNamespace(clientset, token, "github-installation-token")
		if authErr != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: authErr})
			return
		}

		var body githubInstallationTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Owner == "" || body.Repo == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: "request body must be {\"owner\":..., \"repo\":...}"})
			return
		}

		if err := verifyAppOwnsRepo(dynClient, appNamespace, body.Repo); err != nil {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: err.Error()})
			return
		}

		appJWT, err := appCreds.SignAppJWT()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: "signing App JWT: " + err.Error()})
			return
		}
		installationID, err := githubapp.InstallationIDForRepo(appJWT, body.Owner, body.Repo)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: err.Error()})
			return
		}
		instToken, expiresAt, err := githubapp.MintInstallationToken(appJWT, installationID, body.Repo)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Error: err.Error()})
			return
		}

		_ = json.NewEncoder(w).Encode(githubInstallationTokenResponse{Token: instToken, ExpiresAt: expiresAt})
	}
}

// verifyAppOwnsRepo lists PaC Repository CRs in the caller's own namespace only, and
// checks whether any match the requested repo directly (the app's own repo) or via the
// gitops-<app-name> convention. Deliberately ignores the requested "owner" - the app
// name -> gitops repo mapping is owner-agnostic (an app's own repo org and its gitops
// repo org don't have to match).
// prEnvNamespacePattern matches a PR-based ephemeral environment's own namespace
// (glidepath-app.envNamespace's "pr-<number>" convention, e.g.
// "app-checkout-api-pr-9") - capture group 1 is that same app's "-cicd" namespace name
// ("app-checkout-api"), where its Repository CR actually lives.
var prEnvNamespacePattern = regexp.MustCompile(`^(.+)-pr-\d+$`)

func verifyAppOwnsRepo(dynClient dynamic.Interface, appNamespace, requestedRepo string) error {
	if err := appNamespaceOwnsRepo(dynClient, appNamespace, requestedRepo); err == nil {
		return nil
	} else if m := prEnvNamespacePattern.FindStringSubmatch(appNamespace); m != nil {
		// A PR-based ephemeral environment (ephemeral-envs.yaml) has no Repository CR of
		// its own - only the app's shared "-cicd" namespace does. Narrowly scoped: this
		// still only ever authorizes the SAME app's own repo, exactly the one namespace
		// pattern glidepath-app.envNamespace generates for it - not a general
		// cross-namespace grant. Added for the PR-preview-notify PostSync hook, which
		// needs to post a PR comment from inside its own per-PR namespace (2026-08-24).
		cicdNamespace := m[1] + "-cicd"
		if err := appNamespaceOwnsRepo(dynClient, cicdNamespace, requestedRepo); err == nil {
			return nil
		}
	}
	return fmt.Errorf("app namespace %q has no Repository CR that maps to repo %q", appNamespace, requestedRepo)
}

func appNamespaceOwnsRepo(dynClient dynamic.Interface, appNamespace, requestedRepo string) error {
	list, err := dynClient.Resource(repositoryGVR).Namespace(appNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing Repository CRs in %s: %w", appNamespace, err)
	}
	for _, item := range list.Items {
		appName, _, _ := unstructured.NestedString(item.Object, "metadata", "name")
		if appName != "" && (appName == requestedRepo || "gitops-"+appName == requestedRepo) {
			return nil
		}
	}
	return fmt.Errorf("app namespace %q has no Repository CR that maps to repo %q", appNamespace, requestedRepo)
}

func firstHeader(h map[string][]string, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, resp interceptorResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
