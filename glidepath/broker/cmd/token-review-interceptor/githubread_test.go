package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glidepath/broker/token-review-interceptor/internal/githubapp"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const apiSA = "system:serviceaccount:platform-system:glidepath-api"
const otherSA = "system:serviceaccount:platform-system:some-later-reader"

func TestGitHubReadCheck(t *testing.T) {
	callers := map[string]bool{apiSA: true}
	ok := githubReadRequest{Owner: "jfillman", Repo: "gitops-boarding-api", App: "boarding-api"}
	cases := []struct {
		name    string
		callers map[string]bool
		caller  string
		req     githubReadRequest
		code    int
	}{
		{"gitops repo", callers, apiSA, ok, 200},
		{"app repo", callers, apiSA, githubReadRequest{Owner: "jfillman", Repo: "boarding-api", App: "boarding-api"}, 200},
		{"infra gitops repo", callers, apiSA, githubReadRequest{Owner: "jfillman", Repo: "gitops-infra-broker", App: "broker"}, 200},
		{"another app's repo", callers, apiSA, githubReadRequest{Owner: "jfillman", Repo: "gitops-gate-api", App: "boarding-api"}, 403},
		{"a cluster repo", callers, apiSA, githubReadRequest{Owner: "jfillman", Repo: "gitops-hangar-dev", App: "boarding-api"}, 403},
		{"a path in the repo", callers, apiSA, githubReadRequest{Owner: "jfillman", Repo: "gitops-x/../y", App: "x"}, 400},
		{"a bad owner", callers, apiSA, githubReadRequest{Owner: "a/b", Repo: "gitops-x", App: "x"}, 400},
		{"a bad app", callers, apiSA, githubReadRequest{Owner: "jfillman", Repo: "X", App: "X"}, 400},
		{"an app pipeline SA", callers, "system:serviceaccount:app-boarding-api-cicd:pipeline-runner", ok, 403},
		{"another SA in platform-system", callers, "system:serviceaccount:platform-system:default", ok, 403},
		{"Tower's federated identity", callers, "hangar:hangar-prod:tower-actor", ok, 403},
		{"not configured", map[string]bool{}, apiSA, ok, 503},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, err := githubReadCheck(c.callers, c.caller, c.req)
			if code != c.code || (code == 200) != (err == nil) {
				t.Fatalf("code %d err %v, want %d", code, err, c.code)
			}
		})
	}
}

func repoCR(ns, name, url string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "pipelinesascode.tekton.dev/v1alpha1", "kind": "Repository",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"url": url},
	}}
}

func repos(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{repositoryGVR: "RepositoryList"}, objs...)
}

func TestGitHubReadOwns(t *testing.T) {
	dyn := repos(
		repoCR("app-boarding-api-cicd", "boarding-api", "https://github.com/jfillman/boarding-api"),
		repoCR("app-gate-api-cicd", "gate-api", "https://github.com/jfillman/gate-api"),
		repoCR("app-gate-api-cicd", "gitops-gate-api", "https://github.com/jfillman/gitops-gate-api"),
		repoCR("infra-broker-cicd", "gitops-infra-broker", "https://github.com/jfillman/gitops-infra-broker.git"),
	)
	for _, c := range []struct {
		req githubReadRequest
		ok  bool
	}{
		{githubReadRequest{"jfillman", "boarding-api", "boarding-api", ""}, true},
		// No gitops CR of its own: the app's CR maps gitops-<name>, as appNamespaceOwnsRepo does.
		{githubReadRequest{"jfillman", "gitops-boarding-api", "boarding-api", ""}, true},
		{githubReadRequest{"JFillman", "gitops-gate-api", "gate-api", ""}, true},
		{githubReadRequest{"jfillman", "gitops-infra-broker", "broker", ""}, true},
		// The same repo name under another owner is not the app's.
		{githubReadRequest{"someone-else", "boarding-api", "boarding-api", ""}, false},
		// An app with no namespace on this cluster.
		{githubReadRequest{"jfillman", "gitops-ghost", "ghost", ""}, false},
	} {
		if err := githubReadOwns(dyn, c.req); (err == nil) != c.ok {
			t.Errorf("%+v: %v, want ok=%v", c.req, err, c.ok)
		}
	}
}

func testCreds(t *testing.T) *githubapp.Creds {
	t.Helper()
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "github-application-id"), []byte("123\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "github-private-key"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
	creds, err := githubapp.LoadCredsFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

// The whole endpoint against a GitHub stub: the token request GitHub sees is down-scoped to the one repo and to read
// permissions, and nothing is minted for a refused caller or repo.
func TestGitHubReadTokenEndpoint(t *testing.T) {
	var mints []map[string]any
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/jfillman/gitops-boarding-api/installation":
			io.WriteString(w, `{"id": 42}`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mints = append(mints, body)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"token": "ghs_read", "expires_at": "2026-10-28T13:00:00Z"}`)
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	defer githubapp.SetAPIBaseForTest(gh.URL)()

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != githubReadAudience {
			return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Error: "wrong audience"}}, nil
		}
		if !strings.HasPrefix(tr.Spec.Token, "t-") {
			return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Error: "bad token"}}, nil
		}
		user := strings.TrimPrefix(tr.Spec.Token, "t-")
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
			Authenticated: true, User: authenticationv1.UserInfo{Username: user}}}, nil
	})
	dyn := repos(repoCR("app-boarding-api-cicd", "boarding-api", "https://github.com/jfillman/boarding-api"))
	h := handleGitHubReadToken(cs, dyn, testCreds(t), map[string]bool{apiSA: true}, nil, nil)

	call := func(token, body string) (int, githubInstallationTokenResponse) {
		req := httptest.NewRequest(http.MethodPost, "/github-read-token", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		var out githubInstallationTokenResponse
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	good := `{"owner":"jfillman","repo":"gitops-boarding-api","app":"boarding-api"}`
	if code, out := call("t-"+apiSA, good); code != 200 || out.Token != "ghs_read" || out.ExpiresAt == "" {
		t.Fatalf("allowed caller: %d %+v", code, out)
	}
	if len(mints) != 1 {
		t.Fatalf("mints: %v", mints)
	}
	raw, _ := json.Marshal(mints[0])
	if string(raw) != `{"permissions":{"contents":"read","metadata":"read","pull_requests":"read"},"repositories":["gitops-boarding-api"]}` {
		t.Fatalf("the mint request is not down-scoped: %s", raw)
	}
	for _, c := range []struct {
		token, body string
		code        int
	}{
		{"", good, 401},
		{"bad", good, 401},
		{"t-system:serviceaccount:app-boarding-api-cicd:pipeline-runner", good, 403},
		{"t-" + apiSA, `{"owner":"jfillman","repo":"gitops-gate-api","app":"boarding-api"}`, 403},
		{"t-" + apiSA, `{"owner":"jfillman","repo":"gitops-gate-api","app":"gate-api"}`, 403}, // no CR for gate-api here
		{"t-" + apiSA, `{"owner":"other","repo":"gitops-boarding-api","app":"boarding-api"}`, 403},
		{"t-" + apiSA, `not json`, 400},
	} {
		if code, out := call(c.token, c.body); code != c.code || out.Token != "" {
			t.Errorf("%s %s: %d %+v, want %d", c.token, c.body, code, out, c.code)
		}
	}
	if len(mints) != 1 {
		t.Fatalf("a refused request minted: %v", mints)
	}
}

// purpose "ref-protection" is the one call that may carry the extra permissions the cluster configured for it
// (githubRead.refProtectionPermissions); no other call, and no cluster that configured nothing, gets more than the
// read-only set.
func TestGitHubReadTokenRefProtectionPurpose(t *testing.T) {
	var mints []map[string]any
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
			io.WriteString(w, `{"id": 42}`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mints = append(mints, body)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"token": "ghs_read", "expires_at": "2026-10-28T13:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	defer githubapp.SetAPIBaseForTest(gh.URL)()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
			Authenticated: true, User: authenticationv1.UserInfo{Username: strings.TrimPrefix(tr.Spec.Token, "t-")}}}, nil
	})
	dyn := repos(repoCR("app-boarding-api-cicd", "boarding-api", "https://github.com/jfillman/boarding-api"))
	rpCallers := map[string]bool{apiSA: true}
	caller := apiSA
	var permsFor func(extra map[string]string, callers map[string]bool, body string) string
	perms := func(extra map[string]string, body string) string {
		return permsFor(extra, rpCallers, body)
	}
	permsFor = func(extra map[string]string, callers map[string]bool, body string) string {
		mints = nil
		h := handleGitHubReadToken(cs, dyn, testCreds(t), map[string]bool{apiSA: true, otherSA: true}, extra, callers)
		req := httptest.NewRequest(http.MethodPost, "/github-read-token", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer t-"+caller)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != 200 {
			return "status " + rec.Result().Status
		}
		raw, _ := json.Marshal(mints[0]["permissions"])
		return string(raw)
	}
	plain := `{"owner":"jfillman","repo":"boarding-api","app":"boarding-api"}`
	rp := `{"owner":"jfillman","repo":"boarding-api","app":"boarding-api","purpose":"ref-protection"}`
	extra := map[string]string{"administration": "write"}
	readOnly := `{"contents":"read","metadata":"read","pull_requests":"read"}`
	if got := perms(extra, rp); got != `{"administration":"write","contents":"read","metadata":"read","pull_requests":"read"}` {
		t.Fatalf("ref-protection purpose with the setting: %s", got)
	}
	if got := perms(extra, plain); got != readOnly {
		t.Fatalf("a call with no purpose must not get the extra permission: %s", got)
	}
	if got := perms(nil, rp); got != readOnly {
		t.Fatalf("with nothing configured the purpose gets today's permissions: %s", got)
	}
	if got := perms(extra, `{"owner":"jfillman","repo":"boarding-api","app":"boarding-api","purpose":"admin"}`); !strings.HasPrefix(got, "status 400") {
		t.Fatalf("an unknown purpose is refused: %s", got)
	}
	// The elevated permission is bound to the ref-protection callers: a caller allowlisted only to read gets today's set even
	// when it asks for the purpose, and so does everyone when no caller is bound.
	caller = otherSA
	if got := perms(extra, rp); got != readOnly {
		t.Fatalf("a reader that is not a ref-protection caller must not be elevated: %s", got)
	}
	caller = apiSA
	if got := permsFor(extra, nil, rp); got != readOnly {
		t.Fatalf("no ref-protection caller configured: nobody is elevated: %s", got)
	}
	// Never a repository that is not registered as the app's source: its gitops repository.
	// Never the gitops repository: ref protection reads the source repo, so a gitops repo is not worth an admin-write token.
	if got := perms(extra, `{"owner":"jfillman","repo":"gitops-boarding-api","app":"boarding-api","purpose":"ref-protection"}`); got != readOnly {
		t.Fatalf("the gitops repo is never elevated: %s", got)
	}
}

func TestLoadRefProtectionPermissions(t *testing.T) {
	for in, want := range map[string]string{"": "", `{"administration":"write"}`: "administration=write", `{"administration":"read"}`: "administration=read"} {
		t.Setenv("GITHUB_READ_REF_PROTECTION_PERMISSIONS", in)
		got, err := loadRefProtectionPermissions()
		if err != nil || (len(got) == 0) != (want == "") || (want != "" && got["administration"] != strings.TrimPrefix(want, "administration=")) {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{`{"contents":"write"}`, `{"administration":"admin"}`, `not json`, `{"administration":"write","members":"read"}`} {
		t.Setenv("GITHUB_READ_REF_PROTECTION_PERMISSIONS", bad)
		if _, err := loadRefProtectionPermissions(); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}

// The elevated permission goes by the repository's registered ROLE, not its name: only the repository the app's own Repository CR
// registers as its source (the CR named for the app, in app-<app>-cicd), or an InfraService's registered repository.
func TestGitHubReadSourceRole(t *testing.T) {
	dyn := repos(
		repoCR("app-boarding-api-cicd", "boarding-api", "https://github.com/jfillman/boarding-api"),
		repoCR("app-boarding-api-cicd", "gitops-boarding-api", "https://github.com/jfillman/gitops-boarding-api"),
		// an app whose CR registers a differently named repository as its source, and a gitops repo with an unrelated name
		repoCR("app-odd-cicd", "odd", "https://github.com/jfillman/odd-service.git"),
		repoCR("app-odd-cicd", "gitops-odd", "https://github.com/jfillman/odd-deploy"),
		// a CR named for the app that registers what is really a gitops repository: the name does not make it a source
		repoCR("app-mis-cicd", "mis", "https://github.com/jfillman/mis-gitops"),
		repoCR("infra-broker-cicd", "gitops-infra-broker", "https://github.com/jfillman/gitops-infra-broker.git"),
	)
	for _, c := range []struct {
		req  githubReadRequest
		want bool
	}{
		{githubReadRequest{"jfillman", "boarding-api", "boarding-api", ""}, true},
		{githubReadRequest{"jfillman", "gitops-boarding-api", "boarding-api", ""}, false},
		// a differently named gitops repository is never the source; a differently named source is
		{githubReadRequest{"jfillman", "odd-deploy", "odd", ""}, false},
		{githubReadRequest{"jfillman", "odd-service", "odd", ""}, true},
		{githubReadRequest{"jfillman", "odd", "odd", ""}, false}, // the name of the app is not a registration
		// the infra repository registered in its own namespace, and nothing else
		{githubReadRequest{"jfillman", "gitops-infra-broker", "broker", ""}, true},
		{githubReadRequest{"jfillman", "broker", "broker", ""}, false},
		// another owner, an unregistered app
		{githubReadRequest{"someone-else", "boarding-api", "boarding-api", ""}, false},
		{githubReadRequest{"jfillman", "ghost", "ghost", ""}, false},
	} {
		if got := githubReadSourceRole(dyn, c.req); got != c.want {
			t.Errorf("%+v: %v, want %v", c.req, got, c.want)
		}
	}
}
