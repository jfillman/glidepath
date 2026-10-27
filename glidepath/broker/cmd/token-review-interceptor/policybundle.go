// The policy-validation release check's policy source (glidepath ADR-0028, docs/admin/policy-validation.md).
//
// POST /policy-bundle {"cluster": "<name>", "owner": "<github owner>"} returns the admission policies of a target
// cluster, read from its own GitOps repo (the directory and revision POLICY_SOURCES names, `30-policy` on `main` by
// default): ValidatingPolicy, NamespacedValidatingPolicy, ClusterPolicy, Policy, ValidatingAdmissionPolicy and
// ValidatingAdmissionPolicyBinding documents only, as JSON, with the commit they were read at.
//
// Why here and not a token: a cluster repo holds more than policies (Apron commits per-cluster generated passwords
// there by design), and an app's pipeline namespace also runs the app's own build scripts, so a read token for the
// cluster repo handed to the gate would hand those to every app. The broker reads the repo itself with a
// contents:read token it never returns, and only policy documents leave. Policies are not secret: any Glidepath
// app namespace (one with a PaC Repository CR) may read any cluster's bundle.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/glidepath/broker/token-review-interceptor/internal/githubapp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var githubAPI = "https://api.github.com"

const (
	policyBundleAudience = "policy-bundle"
	maxPolicyFileBytes   = 1 << 20
	maxPolicyTotalBytes  = 8 << 20
)

// The kinds the gate evaluates, by API group. Everything else in the directory (Argo CD Applications, RBAC, image
// policies, mutating policies) stays in the broker.
var policyKinds = map[string]map[string]bool{
	"policies.kyverno.io":          {"ValidatingPolicy": true, "NamespacedValidatingPolicy": true},
	"kyverno.io":                   {"ClusterPolicy": true, "Policy": true},
	"admissionregistration.k8s.io": {"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true},
}

var (
	clusterNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	ownerRE       = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9]{0,38})$`)
	githubRepoRE  = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+?)(\.git)?/?$`)
)

type policySource struct {
	RepoURL  string `json:"repoURL,omitempty"`
	Path     string `json:"path,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// policySources is the control plane's policyValidation value (POLICY_SOURCES): a default with {owner} and {cluster}
// placeholders, and per-cluster overrides.
type policySources struct {
	policySource
	Clusters map[string]policySource `json:"clusters,omitempty"`
}

type policyBundleRequest struct {
	Cluster string `json:"cluster"`
	Owner   string `json:"owner"`
}

type policyBundleResponse struct {
	RepoURL   string                   `json:"repoURL,omitempty"`
	Path      string                   `json:"path,omitempty"`
	Revision  string                   `json:"revision,omitempty"`
	Commit    string                   `json:"commit,omitempty"`
	Files     int                      `json:"files"`
	Documents []map[string]interface{} `json:"documents"`
	Error     string                   `json:"error,omitempty"`
}

func loadPolicySources() (*policySources, error) {
	raw := os.Getenv("POLICY_SOURCES")
	if raw == "" {
		return nil, nil
	}
	var s policySources
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("POLICY_SOURCES: %w", err)
	}
	return &s, nil
}

// resolve picks the cluster's source (its override, field by field over the default) and substitutes placeholders.
func (s *policySources) resolve(cluster, owner string) (policySource, error) {
	src := s.policySource
	if o, ok := s.Clusters[cluster]; ok {
		if o.RepoURL != "" {
			src.RepoURL = o.RepoURL
		}
		if o.Path != "" {
			src.Path = o.Path
		}
		if o.Revision != "" {
			src.Revision = o.Revision
		}
	}
	src.RepoURL = strings.NewReplacer("{cluster}", cluster, "{owner}", owner).Replace(src.RepoURL)
	if src.Revision == "" {
		src.Revision = "main"
	}
	src.Path = strings.Trim(path.Clean("/"+src.Path), "/")
	if !githubRepoRE.MatchString(src.RepoURL) {
		return src, fmt.Errorf("policy source %q for cluster %s is not a https://github.com/<owner>/<repo> URL", src.RepoURL, cluster)
	}
	return src, nil
}

func handlePolicyBundle(clientset kubernetes.Interface, dynClient dynamic.Interface, appCreds *githubapp.Creds, sources *policySources) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(policyBundleResponse{Error: msg})
		}
		if sources == nil {
			fail(http.StatusServiceUnavailable, "policy bundles are not configured on this control plane (glidepath-control-plane policyValidation)")
			return
		}
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			fail(http.StatusUnauthorized, "missing or malformed Authorization: Bearer <token> header")
			return
		}
		ns, authErr := verifyCallerAppNamespace(clientset, token, policyBundleAudience)
		if authErr != "" {
			fail(http.StatusUnauthorized, authErr)
			return
		}
		if err := namespaceHasRepository(dynClient, ns); err != nil {
			fail(http.StatusForbidden, err.Error())
			return
		}
		var body policyBundleRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !clusterNameRE.MatchString(body.Cluster) || !ownerRE.MatchString(body.Owner) {
			fail(http.StatusBadRequest, `request body must be {"cluster": <cluster name>, "owner": <GitHub owner>}`)
			return
		}
		src, err := sources.resolve(body.Cluster, body.Owner)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		m := githubRepoRE.FindStringSubmatch(src.RepoURL)
		owner, repo := m[1], m[2]

		appJWT, err := appCreds.SignAppJWT()
		if err != nil {
			fail(http.StatusInternalServerError, "signing App JWT: "+err.Error())
			return
		}
		installationID, err := githubapp.InstallationIDForRepo(appJWT, owner, repo)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		readToken, _, err := githubapp.MintReadOnlyToken(appJWT, installationID, repo)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		commit, err := resolveCommit(readToken, owner, repo, src.Revision)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		tarball, err := githubGet(readToken, fmt.Sprintf("%s/repos/%s/%s/tarball/%s", githubAPI, owner, repo, commit), maxPolicyTotalBytes*8)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		docs, files, err := policyDocumentsFromTarball(tarball, src.Path)
		if err != nil {
			fail(http.StatusBadGateway, fmt.Sprintf("reading %s/%s at %s: %v", src.RepoURL, src.Path, commit, err))
			return
		}
		_ = json.NewEncoder(w).Encode(policyBundleResponse{
			RepoURL: src.RepoURL, Path: src.Path, Revision: src.Revision, Commit: commit, Files: files, Documents: docs,
		})
	}
}

func namespaceHasRepository(dynClient dynamic.Interface, ns string) error {
	list, err := dynClient.Resource(repositoryGVR).Namespace(ns).List(context.Background(), metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("listing Repository CRs in %s: %w", ns, err)
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("namespace %q is not a Glidepath application namespace (no PaC Repository CR)", ns)
	}
	return nil
}

func githubGet(token, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", url, limit)
	}
	return body, nil
}

func resolveCommit(token, owner, repo, revision string) (string, error) {
	body, err := githubGet(token, fmt.Sprintf("%s/repos/%s/%s/commits/%s", githubAPI, owner, repo, revision), 4<<20)
	if err != nil {
		return "", err
	}
	var c struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &c); err != nil || c.SHA == "" {
		return "", fmt.Errorf("no commit for %s/%s@%s", owner, repo, revision)
	}
	return c.SHA, nil
}

// policyDocumentsFromTarball reads every *.yaml/*.yml under dir (GitHub's tarball has one top-level directory) and
// keeps the admission-policy documents. A file that does not parse is skipped, never half-read.
func policyDocumentsFromTarball(tarball []byte, dir string) ([]map[string]interface{}, int, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, 0, err
	}
	tr := tar.NewReader(gz)
	docs := []map[string]interface{}{}
	files, total := 0, 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, files, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := h.Name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if dir != "" && !strings.HasPrefix(name, dir+"/") {
			continue
		}
		if ext := path.Ext(name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		if h.Size > maxPolicyFileBytes {
			continue
		}
		total += int(h.Size)
		if total > maxPolicyTotalBytes {
			return nil, files, fmt.Errorf("more than %d bytes of YAML under %s", maxPolicyTotalBytes, dir)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, files, err
		}
		fileDocs, ok := admissionPolicyDocuments(data)
		if !ok || len(fileDocs) == 0 {
			continue
		}
		files++ // files that hold at least one policy
		docs = append(docs, fileDocs...)
	}
	return docs, files, nil
}

func admissionPolicyDocuments(data []byte) ([]map[string]interface{}, bool) {
	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []map[string]interface{}
	for {
		var d map[string]interface{}
		if err := dec.Decode(&d); err != nil {
			if errors.Is(err, io.EOF) {
				return out, true
			}
			return nil, false
		}
		if d == nil {
			continue
		}
		apiVersion, _ := d["apiVersion"].(string)
		kind, _ := d["kind"].(string)
		group, _, _ := strings.Cut(apiVersion, "/")
		if policyKinds[group][kind] {
			out = append(out, d)
		}
	}
}
