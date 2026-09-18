// tekton-results-relay exists to work around a real Kubernetes limitation, not a
// missing feature: the generic apiserver services/proxy subresource does not forward a
// caller's Authorization header to the backend pod it proxies to (confirmed live,
// 2026-09-17 - see gitops-cluster-dev's HANDOFF-tower-tekton-results.md for the
// isolating test: the same bearer token that got "unable to find token" through
// services/proxy returned a clean 200 when sent straight to Results' ClusterIP from an
// in-cluster debug pod). Tekton Results' own API server does its own TokenReview/
// SubjectAccessReview check and needs to actually see a bearer token to pass it - one
// that never arrives when reached through services/proxy.
//
// This relay sits where Results itself would from the caller's point of view:
// backstage-ingestor's existing services/proxy RBAC grant (gitops-cluster-dev's
// 00-bootstrap/backstage-ingestor-rbac) targets THIS Service instead of
// tekton-results-api-service directly, so that grant still fully controls who may
// reach it. Once a request arrives here, it's re-issued to the real Results API using
// THIS POD'S OWN ServiceAccount token, never the original caller's - Results' own RBAC
// (the results.tekton.dev group, granted to this relay's own identity via the
// ClusterRole in this chart's own template) is what actually decides what can be read,
// so no authorization is lost, just re-anchored to an identity that can actually reach
// Results the normal way (this pod and Results run in the same cluster).
//
// Deliberately plain HTTP on the incoming side (unlike Results' own TLS-only port) -
// matches the existing Rekor proxy's shape (see the RBAC file's rekor-proxy comment)
// and avoids relying on the `https:`-scheme-prefix apiserver-proxy mechanic, which was
// never exercised anywhere else in this codebase before this integration.
package main

import (
	"crypto/tls"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

const (
	// Matches the projected volume's mountPath+path in this chart's own Deployment
	// template (charts/glidepath-control-plane/templates/tekton/tekton-results-relay.yaml).
	tokenPath = "/var/run/secrets/results/token"
	upstream  = "https://tekton-results-api-service.tekton-pipelines.svc.cluster.local:8080"
)

func main() {
	target, err := url.Parse(upstream)
	if err != nil {
		log.Fatalf("tekton-results-relay: parsing upstream url: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Results' own cert is self-signed (tektoncd/operator generates it) - see
	// glidepath/docs/admin/tekton-results.md's own "--insecure" note for the tkn CLI
	// equivalent of this.
	proxy.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		token, err := os.ReadFile(tokenPath)
		if err != nil {
			// Fail visibly, not silently: Results will reject an unauthenticated
			// request on its own (401 "unable to find token"), which surfaces through
			// useTektonResultsRuns.ts's existing error handling rather than a crash
			// here.
			log.Printf("tekton-results-relay: reading own token: %v", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/", proxy)

	log.Println("tekton-results-relay: listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
