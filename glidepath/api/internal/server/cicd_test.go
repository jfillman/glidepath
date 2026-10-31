package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glidepath/api/internal/cicd"
	"github.com/glidepath/api/internal/github"
)

// fakeValidator answers as `glidepath validate --serve` does, from what the request says, and keeps the last request:
// a head with `cach:` has an unknown key; an agent's head that turns `sast: true` into `sast: false` loosens a gate.
type fakeValidator struct {
	mu   sync.Mutex
	last map[string]any
	srv  *httptest.Server
}

func newValidator(t *testing.T) *fakeValidator {
	v := &fakeValidator{}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		v.mu.Lock()
		v.last = req
		v.mu.Unlock()
		head, _ := req["head"].(string)
		base, _ := req["base"].(string)
		problems := []map[string]any{}
		if strings.Contains(head, "cach:") {
			problems = append(problems, map[string]any{"pointer": "/build/cach", "rule": "GP-SCHEMA-001", "severity": "error", "message": "unknown key", "hint": "cache?"})
		}
		if req["agent"] == true && strings.Contains(base, "sast: true") && strings.Contains(head, "sast: false") {
			problems = append(problems, map[string]any{"pointer": "/governance/sast", "rule": "GP-SCOPE-002", "severity": "error", "message": "loosened", "hint": "never the other way"})
		}
		changes := []map[string]any{}
		if _, hasBase := req["base"]; hasBase && head != base {
			changes = append(changes, map[string]any{"pointer": "/x", "change": "changed", "before": false, "after": true, "agentScope": "open"})
		}
		json.NewEncoder(w).Encode(map[string]any{"valid": len(problems) == 0, "problems": problems, "changes": changes, "contractVersion": 1})
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *fakeValidator) request() map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.last
}

// withCicd is newServer plus the validation route's dependencies: GitHub and the broker stubbed, a fake validator.
func withCicd(t *testing.T, allow map[string]bool) (*Server, *ghStub, *fakeValidator, *strings.Builder) {
	s, done := newServer(t, allow)
	t.Cleanup(done)
	gh := newGH(t)
	gh.files["jfillman/shop/cicd.yaml"] = shopCicd
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa-token\n"), 0o600)
	broker := &github.Broker{URL: gh.server.URL, TokenPath: tok, HTTP: gh.server.Client(), API: gh.server.URL}
	v := newValidator(t)
	s.Cicd = &cicd.Client{
		Owner:     "jfillman",
		GitHub:    func(app string) cicd.Source { return broker.Session(app) },
		Validator: &cicd.Validator{URL: v.srv.URL, HTTP: v.srv.Client()},
	}
	logs := &strings.Builder{}
	s.Log = slog.New(slog.NewJSONHandler(logs, nil))
	return s, gh, v, logs
}

func postCicd(t *testing.T, s *Server, body string, hdr map[string]string) (int, map[string]any, string) {
	req := httptest.NewRequest(http.MethodPost, "/v1/apps/shop/cicd/validations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t-dev")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	raw, _ := io.ReadAll(rec.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return rec.Code, out, string(raw)
}

var validator = map[string]bool{"dev validate cicd app-shop-cicd": true}

func TestValidateCicdPatch(t *testing.T) {
	s, gh, v, logs := withCicd(t, validator)
	code, out, body := postCicd(t, s, `{"patch": {"build": {"cache": {"enabled": true}}}}`, nil)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	resulting, _ := out["resulting"].(string)
	if !strings.Contains(resulting, "enabled: true") || !strings.Contains(resulting, "tier: flight") {
		t.Fatalf("the patch was not applied to the base:\n%s", resulting)
	}
	base := out["base"].(map[string]any)
	if base["present"] != true || base["sha"] != devSHA || base["ref"] != "HEAD" || base["repo"] != "jfillman/shop" {
		t.Fatalf("base %v", base)
	}
	req := v.request()
	if req["agent"] != false || req["base"] != shopCicd || req["head"] != resulting {
		t.Fatalf("the validator was asked %v", req)
	}
	if out["valid"] != true || out["summary"] != "valid; 1 change(s): /x false -> true" {
		t.Fatalf("valid %v summary %q", out["valid"], out["summary"])
	}
	if !strings.Contains(strings.Join(gh.mints, ","), "jfillman/shop") {
		t.Fatalf("no read token was minted for the app's repo: %v", gh.mints)
	}
	audit := logs.String()
	for _, want := range []string{`"op":"validateCicd"`, `"tier":"T0"`, `"valid":true`, `"changes":1`, `"base":"abc1234"`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit line lacks %s: %s", want, audit)
		}
	}
}

func TestValidateCicdContentAsAgentAtRef(t *testing.T) {
	s, gh, v, _ := withCicd(t, validator)
	gh.files["jfillman/shop/cicd.yaml"] = "governance:\n  sast: true\n"
	code, out, body := postCicd(t, s, `{"content": "governance:\n  sast: false\n", "agent": true, "baseRef": "abc1234"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if out["valid"] != false || out["resulting"] != "governance:\n  sast: false\n" {
		t.Fatalf("%s", body)
	}
	problems := out["problems"].([]any)
	if len(problems) != 1 || problems[0].(map[string]any)["rule"] != "GP-SCOPE-002" {
		t.Fatalf("problems %v", problems)
	}
	if base := out["base"].(map[string]any); base["sha"] != devSHA || base["ref"] != "abc1234" {
		t.Fatalf("base %v", base)
	}
	if req := v.request(); req["agent"] != true || req["base"] != "governance:\n  sast: true\n" {
		t.Fatalf("the validator was asked %v", req)
	}
	if !strings.HasPrefix(out["summary"].(string), "1 error(s): GP-SCOPE-002; 1 change(s)") {
		t.Fatalf("summary %q", out["summary"])
	}
}

func TestValidateCicdNoFileYet(t *testing.T) {
	s, gh, v, _ := withCicd(t, validator)
	delete(gh.files, "jfillman/shop/cicd.yaml")
	code, out, body := postCicd(t, s, `{"patch": {"apiVersion": "platform/v1", "build": {"agent": "nodejs-22"}}}`, nil)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if base := out["base"].(map[string]any); base["present"] != false || base["sha"] != devSHA {
		t.Fatalf("base %v", base)
	}
	if out["resulting"] != "apiVersion: platform/v1\nbuild:\n    agent: nodejs-22\n" {
		t.Fatalf("resulting %q", out["resulting"])
	}
	if req := v.request(); req["base"] != nil {
		t.Fatalf("a missing base must reach the validator as null, got %v", req["base"])
	}
}

func TestValidateCicdRefusals(t *testing.T) {
	s, _, _, _ := withCicd(t, validator)
	cases := []struct {
		name, body, token string
		status            int
		rule, msg         string
	}{
		{"neither patch nor content", `{"agent": true}`, "t-dev", 400, "GP-API-400", "exactly one of patch"},
		{"both", `{"patch": {}, "content": "x: 1\n"}`, "t-dev", 400, "GP-API-400", "exactly one of patch"},
		{"patch not an object", `{"patch": [1, 2]}`, "t-dev", 400, "GP-API-400", "JSON object"},
		{"patch not JSON", `{"patch": {"a": }`, "t-dev", 400, "GP-API-400", ""},
		{"bad ref", `{"patch": {}, "baseRef": "a..b"}`, "t-dev", 400, "GP-API-400", "baseRef must be"},
		{"unknown ref", `{"patch": {}, "baseRef": "nosuch"}`, "t-dev", 400, "GP-API-400", "is not a ref of jfillman/shop"},
		{"not allowed", `{"patch": {}}`, "t-nobody", 403, "GP-AUTHZ-001", ""},
	}
	for _, c := range cases {
		code, out, body := postCicd(t, s, c.body, map[string]string{"Authorization": "Bearer " + c.token})
		if code != c.status || out["rule"] != c.rule || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
}

func TestValidateCicdValidatorDown(t *testing.T) {
	s, _, _, _ := withCicd(t, validator)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	s.Cicd.Validator = &cicd.Validator{URL: dead.URL, HTTP: http.DefaultClient}
	code, out, body := postCicd(t, s, `{"patch": {}}`, nil)
	if code != http.StatusBadGateway || out["rule"] != "GP-API-502" || !strings.Contains(body, "validator") {
		t.Fatalf("%d %s", code, body)
	}
}
