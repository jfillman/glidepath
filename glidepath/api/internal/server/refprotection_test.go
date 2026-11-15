package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glidepath/api/internal/github"
	"github.com/glidepath/api/internal/refprotection"
)

const (
	rpSHA  = "2222222222222222222222222222222222222222"
	rpHead = "3333333333333333333333333333333333333333"
	rpPull = `[{"number":5,"html_url":"https://github.com/jfillman/shop/pull/5","state":"closed","merged_at":"2026-11-01T11:00:00Z","merge_commit_sha":"` + rpSHA + `","commits":1,"user":{"login":"alice","type":"User"},"merged_by":{"login":"alice","type":"User"},"head":{"sha":"` + rpHead + `","repo":{"full_name":"jfillman/shop"}},"base":{"ref":"main","repo":{"full_name":"jfillman/shop"}}}]`
)

// rpHost is GitHub and the broker for the ref-protection route: a main branch whose one commit came through a squash-merged,
// approved pull request (the response shapes of internal/refprotection's tests), per-repo tokens, and a record of every
// path asked.
type rpHost struct {
	mu    sync.Mutex
	calls []string
	mints []string
	srv   *httptest.Server
}

func newRPHost(t *testing.T) *rpHost {
	h := &rpHost{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.URL.Path == "/github-read-token" {
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			h.mints = append(h.mints, req["owner"]+"/"+req["repo"])
			json.NewEncoder(w).Encode(map[string]string{"token": "ghs-" + req["repo"]})
			return
		}
		h.calls = append(h.calls, r.URL.RequestURI())
		if r.URL.Path == "/repos/jfillman/gitops-shop/pulls" && r.Header.Get("Authorization") == "Bearer ghs-gitops-shop" {
			io.WriteString(w, `[]`) // no release yet: a first release
			return
		}
		if r.Header.Get("Authorization") != "Bearer ghs-shop" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/repos/jfillman/shop/commits/"+rpSHA:
			io.WriteString(w, `{"sha":"`+rpSHA+`","parents":[{"sha":"9999999999999999999999999999999999999999"}],"author":{"login":"alice","type":"User"},"committer":{"login":"web-flow","type":"User"},"commit":{"author":{"name":"Alice"},"committer":{"name":"GitHub"},"verification":{"verified":true}}}`)
		case r.URL.Path == "/repos/jfillman/shop/collaborators/bob/permission":
			io.WriteString(w, `{"permission":"write","role_name":"write"}`)
		case strings.HasPrefix(r.URL.Path, "/repos/jfillman/shop/rules/branches/main"):
			io.WriteString(w, `[{"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"jfillman/shop","ruleset_id":100},
			 {"type":"non_fast_forward","ruleset_source_type":"Repository","ruleset_source":"jfillman/shop","ruleset_id":100},
			 {"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":true},"ruleset_source_type":"Repository","ruleset_source":"jfillman/shop","ruleset_id":100}]`)
		case r.URL.Path == "/repos/jfillman/shop/rulesets/100":
			io.WriteString(w, `{"id":100,"target":"branch","enforcement":"active","bypass_actors":[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]}`)
		case strings.HasSuffix(r.URL.Path, "/protection"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Branch not protected"}`)
		case strings.HasPrefix(r.URL.Path, "/repos/jfillman/shop/compare/"):
			io.WriteString(w, `{"status":"behind"}`)
		case r.URL.Path == "/repos/jfillman/shop/commits/"+rpSHA+"/pulls":
			io.WriteString(w, rpPull)
		case r.URL.Path == "/repos/jfillman/shop/pulls/5":
			io.WriteString(w, rpPull[1:len(rpPull)-1])
		case r.URL.Path == "/repos/jfillman/shop/pulls/5/commits":
			io.WriteString(w, `[{"sha":"`+rpHead+`","author":{"login":"alice","type":"User"},"committer":{"login":"alice","type":"User"},"parents":[],"commit":{"author":{"name":"Alice"},"committer":{"name":"Alice"}}}]`)
		case r.URL.Path == "/repos/jfillman/shop/pulls/5/reviews":
			io.WriteString(w, `[{"id":1,"user":{"login":"bob","type":"User"},"state":"APPROVED","commit_id":"`+rpHead+`","submitted_at":"2026-11-01T10:00:00Z"}]`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func withRefProtection(t *testing.T, allow map[string]bool) (*Server, *rpHost) {
	s, done := newServer(t, allow)
	t.Cleanup(done)
	h := newRPHost(t)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa-token\n"), 0o600)
	broker := &github.Broker{URL: h.srv.URL, TokenPath: tok, HTTP: h.srv.Client(), API: h.srv.URL}
	s.RefProtection = &refprotection.Client{
		Owner:   "jfillman",
		Checker: &refprotection.Checker{ProtectedBranches: []string{"main"}},
		GitHub:  func(app string) refprotection.Reader { return broker.Session(app) },
	}
	return s, h
}

var refReader = map[string]bool{"dev get refprotection app-shop-cicd": true}

func getRP(t *testing.T, s *Server, query string) (int, map[string]any, http.Header) {
	req := httptest.NewRequest(http.MethodGet, "/v1/apps/shop/ref-protection?"+query, nil)
	req.Header.Set("Authorization", "Bearer t-dev")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

func TestRefProtectionVerdict(t *testing.T) {
	s, h := withRefProtection(t, refReader)
	code, out, hdr := getRP(t, s, "repo=jfillman/shop&ref=refs/heads/main&sha="+rpSHA)
	if code != http.StatusOK || out["verdict"] != "protected" || out["kind"] != "branch" || out["sha"] != rpSHA {
		t.Fatalf("%d %v", code, out)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("a verdict is never cached: %v", hdr)
	}
	ev, _ := out["evidence"].(map[string]any)
	if out["branch"] != "main" || ev == nil || ev["mode"] != "first" || len(ev["approvers"].([]any)) != 1 || out["configuration"] == nil {
		t.Fatalf("the verdict carries its evidence and the informational configuration: %v", out)
	}
	if len(h.mints) != 2 || !(h.mints[0] == "jfillman/shop" && h.mints[1] == "jfillman/gitops-shop" || h.mints[1] == "jfillman/shop" && h.mints[0] == "jfillman/gitops-shop") {
		t.Fatalf("one token per repo of the app (its source, and its gitops repository for the release history), this request: %v", h.mints)
	}
	// A second call reads the host again and mints again.
	getRP(t, s, "repo=jfillman/shop&ref=refs/heads/main&sha="+rpSHA)
	if len(h.mints) != 4 {
		t.Fatalf("nothing is cached across requests: %v", h.mints)
	}
}

func TestRefProtectionAuthorizationAndInput(t *testing.T) {
	s, _ := withRefProtection(t, map[string]bool{})
	if code, _, _ := getRP(t, s, "repo=jfillman/shop&ref=refs/heads/main&sha="+rpSHA); code != http.StatusForbidden {
		t.Fatalf("a caller with no refprotection grant is refused, got %d", code)
	}
	s, h := withRefProtection(t, refReader)
	for name, q := range map[string]string{
		"another owner":      "repo=evil/shop&ref=refs/heads/main&sha=" + rpSHA,
		"another repository": "repo=jfillman/other&ref=refs/heads/main&sha=" + rpSHA,
		"no repo":            "ref=refs/heads/main&sha=" + rpSHA,
		"short ref":          "repo=jfillman/shop&ref=main&sha=" + rpSHA,
		"bad sha":            "repo=jfillman/shop&ref=refs/heads/main&sha=abc",
	} {
		if code, out, _ := getRP(t, s, q); code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}
	if len(h.mints) != 0 || len(h.calls) != 0 {
		t.Fatalf("a refused request reaches neither the broker nor the host: %v %v", h.mints, h.calls)
	}
}

// GitHub answering with a failure is "unverifiable" (the guard refuses on anything but protected); the broker failing to
// mint a token is no answer at all: 502.
func TestRefProtectionHostFailureFailsClosed(t *testing.T) {
	s, h := withRefProtection(t, refReader)
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github-read-token" {
			io.WriteString(w, `{"token":"ghs-shop"}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	code, out, _ := getRP(t, s, "repo=jfillman/shop&ref=refs/heads/main&sha="+rpSHA)
	if code != http.StatusOK || out["verdict"] != "unverifiable" {
		t.Fatalf("%d %v", code, out)
	}
	h.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"no"}`)
	})
	code, out, _ = getRP(t, s, "repo=jfillman/shop&ref=refs/heads/main&sha="+rpSHA)
	if code != http.StatusBadGateway || out["verdict"] != nil {
		t.Fatalf("%d %v", code, out)
	}
}

func TestRefProtectionUnconfigured(t *testing.T) {
	s, done := newServer(t, refReader)
	defer done()
	if code, _, _ := getRP(t, s, "repo=jfillman/shop&ref=refs/heads/main&sha="+rpSHA); code != http.StatusServiceUnavailable {
		t.Fatalf("%d", code)
	}
}
