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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const clearanceSA = "system:serviceaccount:autopilot-system:clearance"

func TestAgentTokenCheck(t *testing.T) {
	callers := map[string]bool{clearanceSA: true}
	ok := githubReadRequest{Owner: "jfillman", Repo: "boarding-api", App: "boarding-api"}
	for _, c := range []struct {
		name    string
		callers map[string]bool
		caller  string
		req     githubReadRequest
		code    int
	}{
		{"app repo", callers, clearanceSA, ok, 200},
		{"gitops repo", callers, clearanceSA, githubReadRequest{Owner: "jfillman", Repo: "gitops-boarding-api", App: "boarding-api"}, 200},
		{"a cluster repo", callers, clearanceSA, githubReadRequest{Owner: "jfillman", Repo: "gitops-hangar-dev", App: "boarding-api"}, 403},
		{"a platform repo named as its own app", callers, clearanceSA, githubReadRequest{Owner: "jfillman", Repo: "glidepath", App: "glidepath"}, 200}, // names pass; githubReadOwns refuses (no Repository CR)
		{"another app's repo", callers, clearanceSA, githubReadRequest{Owner: "jfillman", Repo: "gate-api", App: "boarding-api"}, 403},
		{"a path in the repo", callers, clearanceSA, githubReadRequest{Owner: "jfillman", Repo: "a/../b", App: "a"}, 400},
		{"glidepath-api is not a caller here", callers, "system:serviceaccount:platform-system:glidepath-api", ok, 403},
		{"the old pre-deployment identity", callers, "system:serviceaccount:platform-system:clearance", ok, 403},
		{"an agent run's own SA", callers, "system:serviceaccount:agent-r-0001:run", ok, 403},
		{"not configured", map[string]bool{}, clearanceSA, ok, 503},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, err := agentTokenCheck(c.callers, c.caller, c.req)
			if code != c.code || (code == 200) != (err == nil) {
				t.Fatalf("code %d err %v, want %d", code, err, c.code)
			}
		})
	}
}

// The endpoint against a GitHub stub: the mint is down-scoped to the one repo and to contents/pull_requests write and
// metadata read (no workflows); a refused caller or repo mints nothing; a missing App mount is a 503 until it appears.
func TestAgentInstallationTokenEndpoint(t *testing.T) {
	var mints []map[string]any
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/jfillman/boarding-api/installation":
			io.WriteString(w, `{"id": 7}`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mints = append(mints, body)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"token": "ghs_agent", "expires_at": "2026-10-29T18:00:00Z"}`)
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	defer githubapp.SetAPIBaseForTest(gh.URL)()

	dir := t.TempDir()
	old := agentAppDir
	agentAppDir = dir
	defer func() { agentAppDir = old }()

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != agentTokenAudience || !strings.HasPrefix(tr.Spec.Token, "t-") {
			return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Error: "refused"}}, nil
		}
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
			Authenticated: true, User: authenticationv1.UserInfo{Username: strings.TrimPrefix(tr.Spec.Token, "t-")}}}, nil
	})
	dyn := repos(repoCR("app-boarding-api-cicd", "boarding-api", "https://github.com/jfillman/boarding-api"))
	h := handleAgentInstallationToken(cs, dyn, map[string]bool{clearanceSA: true})
	call := func(token, body string) (int, githubInstallationTokenResponse) {
		req := httptest.NewRequest(http.MethodPost, "/agent-installation-token", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		var out githubInstallationTokenResponse
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	good := `{"owner":"jfillman","repo":"boarding-api","app":"boarding-api"}`

	if code, out := call("t-"+clearanceSA, good); code != 503 || out.Token != "" {
		t.Fatalf("no App mounted: %d %+v", code, out)
	}
	// The Secret syncs: the next request loads it, no restart.
	writeTestApp(t, dir)
	if code, out := call("t-"+clearanceSA, good); code != 200 || out.Token != "ghs_agent" {
		t.Fatalf("allowed caller: %d %+v", code, out)
	}
	raw, _ := json.Marshal(mints[0])
	if string(raw) != `{"permissions":{"contents":"write","metadata":"read","pull_requests":"write"},"repositories":["boarding-api"]}` {
		t.Fatalf("the mint is not down-scoped: %s", raw)
	}
	for _, c := range []struct {
		token, body string
		code        int
	}{
		{"", good, 401},
		{"bad", good, 401},
		{"t-system:serviceaccount:platform-system:glidepath-api", good, 403},
		{"t-" + clearanceSA, `{"owner":"jfillman","repo":"gitops-hangar-dev","app":"boarding-api"}`, 403},
		{"t-" + clearanceSA, `{"owner":"jfillman","repo":"glidepath","app":"glidepath"}`, 403},
		{"t-" + clearanceSA, `{"owner":"other","repo":"boarding-api","app":"boarding-api"}`, 403},
		{"t-" + clearanceSA, `nope`, 400},
	} {
		if code, out := call(c.token, c.body); code != c.code || out.Token != "" {
			t.Errorf("%s %s: %d %+v, want %d", c.token, c.body, code, out, c.code)
		}
	}
	if len(mints) != 1 {
		t.Fatalf("a refused request minted: %v", mints)
	}
}

func writeTestApp(t *testing.T, dir string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "github-application-id"), []byte("456\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "github-private-key"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
}
