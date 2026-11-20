package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/glidepath/broker/token-review-interceptor/internal/githubapp"
)

func goodPin() agentPinRequest {
	return agentPinRequest{Owner: "jfillman", Repo: "gate-api", Name: "gate-reader", Path: "agents/gate-reader.yaml",
		Commit: strings.Repeat("a", 40), Group: "group:default/team-platform", Description: "reads runs"}
}

func TestAppOfNamespace(t *testing.T) {
	for ns, want := range map[string]string{"app-gate-api-cicd": "gate-api", "infra-skyport-broker-cicd": "skyport-broker", "platform-system": "", "app-x-prod": ""} {
		got, ok := appOfNamespace(ns)
		if got != want || ok != (want != "") {
			t.Errorf("%s: got %q %v", ns, got, ok)
		}
	}
}

func TestCheckPinRequest(t *testing.T) {
	if err := checkPinRequest(goodPin()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*agentPinRequest){
		"bad name":   func(r *agentPinRequest) { r.Name = "Bad_Name" },
		"other path": func(r *agentPinRequest) { r.Path = "agents/other.yaml" },
		"traversal":  func(r *agentPinRequest) { r.Path = "../agents/gate-reader.yaml" },
		"short sha":  func(r *agentPinRequest) { r.Commit = "abc" },
		"owner":      func(r *agentPinRequest) { r.Group = "team-platform" },
		"newline":    func(r *agentPinRequest) { r.Description = "a\nb" },
		"repo":       func(r *agentPinRequest) { r.Repo = "a/b" },
	}
	for name, mut := range cases {
		r := goodPin()
		mut(&r)
		if checkPinRequest(r) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r := goodPin()
	r.Remove, r.Group = true, ""
	if err := checkPinRequest(r); err != nil {
		t.Errorf("remove without a group: %v", err)
	}
}

func TestPinFileIsTheAgentXR(t *testing.T) {
	f := pinFile("gate-api", goodPin())
	for _, want := range []string{"kind: Agent", "name: gate-reader", "namespace: app-gate-api-cicd", "app: gate-api",
		"owner: group:default/team-platform", `description: "reads runs"`, "repo: jfillman/gate-api", "path: agents/gate-reader.yaml",
		"commit: " + strings.Repeat("a", 40)} {
		if !strings.Contains(f, want) {
			t.Errorf("missing %q in\n%s", want, f)
		}
	}
}

func TestOpenPinPR(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			_, _ = w.Write([]byte(`{"object":{"sha":"base"}}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git/refs"):
			w.WriteHeader(201)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(404)
		case r.Method == "PUT":
			var put map[string]string
			_ = json.Unmarshal(body, &put)
			if put["branch"] != "agent-release/gate-api-gate-reader-aaaaaaa" || put["content"] == "" {
				t.Errorf("bad put %v", put)
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"commit":{"sha":"headsha"}}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"number":23,"html_url":"https://github.com/jfillman/t/pull/23"}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	defer githubapp.SetAPIBaseForTest(srv.URL)()
	out, err := openPinPR(ghClient{token: "t"}, "jfillman/t", "gate-api", goodPin())
	if err != nil {
		t.Fatal(err)
	}
	if out.PR != 23 || out.Head != "headsha" || out.File != "tenants/gate-api/xr-requests/gate-reader.yaml" {
		t.Errorf("got %+v (calls %v)", out, calls)
	}
}

func TestParseServiceAccountUsername(t *testing.T) {
	cases := []struct {
		username, ns, sa string
		ok               bool
	}{
		{"system:serviceaccount:app-gate-api-cicd:pipeline-runner", "app-gate-api-cicd", "pipeline-runner", true},
		{"system:serviceaccount:app-gate-api-cicd", "", "", false},
		{"system:serviceaccount:app-gate-api-cicd:", "", "", false},
		{"system:serviceaccount::pipeline-runner", "", "", false},
		{"system:serviceaccount:a:b:c", "", "", false},
		{"system:serviceaccount:a:b:", "", "", false},
		{"system:node:worker-1", "", "", false},
		{"alice", "", "", false},
		{"", "", "", false},
		{"system:serviceaccounts:app-gate-api-cicd:pipeline-runner", "", "", false},
		{"prefix system:serviceaccount:app-gate-api-cicd:pipeline-runner", "", "", false},
		{"system:serviceaccount:app-gate-api-cicd:pipeline-runner\n", "", "", false},
		{"system:serviceaccount:App-X-cicd:pipeline-runner", "", "", false},
		{"system:serviceaccount:app-gate-api-cicd:pipeline..runner", "", "", false},
		{"system:serviceaccount:app-gate-api-cicd:.pipeline-runner", "", "", false},
		{"system:serviceaccount:app-gate-api-cicd:pipeline-runner.", "", "", false},
		{"system:serviceaccount:app.gate-cicd:pipeline-runner", "", "", false},
		{"system:serviceaccount:app-gate-api-cicd:pipeline_runner", "", "", false},
	}
	for _, c := range cases {
		ns, sa, ok := parseServiceAccountUsername(c.username)
		if ns != c.ns || sa != c.sa || ok != c.ok {
			t.Errorf("%q: got %q %q %v", c.username, ns, sa, ok)
		}
	}
}

func TestCheckPinCaller(t *testing.T) {
	allowed := map[string]bool{"pipeline-runner": true, "clearance-gate": true}
	cases := []struct {
		name, username string
		allowed        map[string]bool
		status         int
		app            string
		msgHas         string
	}{
		{"pipeline-runner allowed", "system:serviceaccount:app-gate-api-cicd:pipeline-runner", allowed, 0, "gate-api", ""},
		{"clearance-gate allowed", "system:serviceaccount:app-gate-api-cicd:clearance-gate", allowed, 0, "gate-api", ""},
		{"infra namespace allowed", "system:serviceaccount:infra-skyport-broker-cicd:clearance-gate", allowed, 0, "skyport-broker", ""},
		{"other SA in the right namespace", "system:serviceaccount:app-gate-api-cicd:default", allowed, http.StatusForbidden, "", `ServiceAccount "default"`},
		{"look-alike SA name", "system:serviceaccount:app-gate-api-cicd:pipeline-runner-2", allowed, http.StatusForbidden, "", "may not pin agents"},
		{"allowed SA in the wrong namespace", "system:serviceaccount:platform-system:pipeline-runner", allowed, http.StatusForbidden, "", "not an app's -cicd namespace"},
		{"allowed SA in an app prod namespace", "system:serviceaccount:app-gate-api-prod:clearance-gate", allowed, http.StatusForbidden, "", "not an app's -cicd namespace"},
		{"malformed: too few parts", "system:serviceaccount:app-gate-api-cicd", allowed, http.StatusUnauthorized, "", "not a ServiceAccount identity"},
		{"malformed: too many parts", "system:serviceaccount:app-gate-api-cicd:pipeline-runner:x", allowed, http.StatusUnauthorized, "", "not a ServiceAccount identity"},
		{"malformed: a user", "jfillman", allowed, http.StatusUnauthorized, "", "not a ServiceAccount identity"},
		{"malformed: empty SA", "system:serviceaccount:app-gate-api-cicd:", allowed, http.StatusUnauthorized, "", "not a ServiceAccount identity"},
		{"malformed: empty dot component in SA", "system:serviceaccount:app-gate-api-cicd:pipeline..runner", allowed, http.StatusUnauthorized, "", "not a ServiceAccount identity"},
		{"empty allowlist", "system:serviceaccount:app-gate-api-cicd:pipeline-runner", map[string]bool{}, http.StatusServiceUnavailable, "", "not configured"},
	}
	for _, c := range cases {
		ns, app, status, err := checkPinCaller(c.username, c.allowed)
		if status != c.status {
			t.Errorf("%s: status %d, want %d (err %v)", c.name, status, c.status, err)
			continue
		}
		if c.status == 0 {
			if err != nil || app != c.app || ns == "" {
				t.Errorf("%s: ns %q app %q err %v", c.name, ns, app, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.msgHas) {
			t.Errorf("%s: err %v, want it to contain %q", c.name, err, c.msgHas)
		}
	}
}

func TestLoadAgentPinServiceAccounts(t *testing.T) {
	t.Run("unset defaults to the W-8 overlap list", func(t *testing.T) {
		os.Unsetenv("AGENT_PIN_SERVICE_ACCOUNTS")
		got := loadAgentPinServiceAccounts()
		if len(got) != 2 || !got["pipeline-runner"] || !got["clearance-gate"] {
			t.Errorf("got %v", got)
		}
	})
	t.Run("set replaces the default", func(t *testing.T) {
		t.Setenv("AGENT_PIN_SERVICE_ACCOUNTS", " clearance-gate , ,")
		got := loadAgentPinServiceAccounts()
		if len(got) != 1 || !got["clearance-gate"] {
			t.Errorf("got %v", got)
		}
	})
	t.Run("set but empty allows no one", func(t *testing.T) {
		t.Setenv("AGENT_PIN_SERVICE_ACCOUNTS", "")
		if got := loadAgentPinServiceAccounts(); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
}
