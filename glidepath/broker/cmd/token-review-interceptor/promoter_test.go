package main

import (
	"net/http"
	"testing"

	"github.com/glidepath/broker/token-review-interceptor/internal/githubapp"
)

func TestScopeFor(t *testing.T) {
	const sa = "system:serviceaccount:platform-catalog:glidepath-promoter"
	full := promoterConfig{Caller: sa, DispatchRepo: "riley-roooo/flight-promoter", App: &githubapp.Creds{}}
	cases := []struct {
		name     string
		cfg      promoterConfig
		caller   string
		req      promoterTokenRequest
		code     int
		promoter bool
		repo     string
		perms    map[string]string
	}{
		{"dispatch", full, sa, promoterTokenRequest{Purpose: "dispatch"}, 200, true, "flight-promoter", map[string]string{"actions": "write"}},
		{"dispatch ignores a requested repo", full, sa, promoterTokenRequest{Purpose: "dispatch", Owner: "jfillman", Repo: "gitops-x"}, 200, true, "flight-promoter", map[string]string{"actions": "write"}},
		{"release-pr", full, sa, promoterTokenRequest{Purpose: "release-pr", Owner: "jfillman", Repo: "gitops-boarding-api"}, 200, false, "gitops-boarding-api",
			map[string]string{"checks": "write", "contents": "read", "pull_requests": "read"}},
		{"release-pr refuses an app repo", full, sa, promoterTokenRequest{Purpose: "release-pr", Owner: "jfillman", Repo: "boarding-api"}, 403, false, "", nil},
		{"release-pr refuses a path in the repo", full, sa, promoterTokenRequest{Purpose: "release-pr", Owner: "jfillman", Repo: "gitops-x/../y"}, 403, false, "", nil},
		{"release-pr refuses a bad owner", full, sa, promoterTokenRequest{Purpose: "release-pr", Owner: "a/b", Repo: "gitops-x"}, 403, false, "", nil},
		{"another SA in the same namespace", full, "system:serviceaccount:platform-catalog:default", promoterTokenRequest{Purpose: "dispatch"}, 403, false, "", nil},
		{"an app pipeline SA", full, "system:serviceaccount:app-boarding-api-cicd:pipeline-runner", promoterTokenRequest{Purpose: "release-pr", Owner: "jfillman", Repo: "gitops-boarding-api"}, 403, false, "", nil},
		{"unknown purpose", full, sa, promoterTokenRequest{Purpose: "push"}, 400, false, "", nil},
		{"not configured", promoterConfig{}, sa, promoterTokenRequest{Purpose: "dispatch"}, 503, false, "", nil},
		{"no promoter key", promoterConfig{Caller: sa, DispatchRepo: "riley-roooo/flight-promoter"}, sa, promoterTokenRequest{Purpose: "dispatch"}, 503, false, "", nil},
		{"no promoter key still serves release-pr", promoterConfig{Caller: sa}, sa, promoterTokenRequest{Purpose: "release-pr", Owner: "jfillman", Repo: "gitops-a"}, 200, false, "gitops-a",
			map[string]string{"checks": "write", "contents": "read", "pull_requests": "read"}},
		{"bad dispatch repo", promoterConfig{Caller: sa, DispatchRepo: "flight-promoter", App: &githubapp.Creds{}}, sa, promoterTokenRequest{Purpose: "dispatch"}, 503, false, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scope, code, err := scopeFor(c.cfg, c.caller, c.req)
			if code != c.code {
				t.Fatalf("code %d, want %d (err %v)", code, c.code, err)
			}
			if c.code != http.StatusOK {
				if err == nil {
					t.Fatal("no error for a refusal")
				}
				return
			}
			if scope.UsePromoterApp != c.promoter || scope.Repo != c.repo {
				t.Fatalf("scope %+v", scope)
			}
			if len(scope.Permissions) != len(c.perms) {
				t.Fatalf("permissions %v, want %v", scope.Permissions, c.perms)
			}
			for k, v := range c.perms {
				if scope.Permissions[k] != v {
					t.Fatalf("permissions %v, want %v", scope.Permissions, c.perms)
				}
			}
		})
	}
}
