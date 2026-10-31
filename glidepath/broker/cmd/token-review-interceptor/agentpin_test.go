package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
