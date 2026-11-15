package refprotection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const sha = "1111111111111111111111111111111111111111"

// stub serves recorded GitHub response shapes by repo-relative path (query included). A path with no route is a test
// failure, so a call the code makes that the test did not expect cannot pass silently.
type resp struct {
	st   int
	body string
}
type stub struct {
	t      *testing.T
	routes map[string]resp
	calls  []string
}

func (s *stub) GetJSON(_ context.Context, _, _, path string) (int, []byte, error) {
	s.calls = append(s.calls, path)
	r, ok := s.routes[path]
	if !ok {
		s.t.Fatalf("unexpected GET %s\nhave: %v", path, keys(s.routes))
	}
	if r.st == 0 {
		return 0, nil, errors.New("connection reset")
	}
	return r.st, []byte(r.body), nil
}

func (s *stub) GraphQL(context.Context, string, string, string, map[string]any) (int, []byte, error) {
	s.t.Fatal("unexpected GraphQL")
	return 0, nil, nil
}

func keys(m map[string]resp) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func (s *stub) called(sub string) int {
	n := 0
	for _, c := range s.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// Recorded shapes. rules/branches: GET /repos/{o}/{r}/rules/branches/{branch}; the ruleset bodies are the shape
// GET /repos/{o}/{r}/rulesets/{id} returns to a reader that may see bypass_actors.
const (
	upgrade403 = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"403"}`
	notProt404 = `{"message":"Branch not protected","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"404"}`
	noAdmin403 = `{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"403"}`

	rulesMain = `[
 {"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},
 {"type":"non_fast_forward","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},
 {"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":true,"require_code_owner_review":false,"require_last_push_approval":false,"required_review_thread_resolution":false},"ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100}]`

	mainRulesetAdminAndApp = `{"id":100,"name":"main","target":"branch","source_type":"Repository","source":"o/app","enforcement":"active",
 "conditions":{"ref_name":{"exclude":[],"include":["~DEFAULT_BRANCH"]}},
 "rules":[{"type":"deletion"},{"type":"non_fast_forward"},{"type":"pull_request","parameters":{"required_approving_review_count":1}}],
 "bypass_actors":[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"},{"actor_id":123,"actor_type":"Integration","bypass_mode":"always"}],
 "current_user_can_bypass":"never"}`

	behind = `{"status":"behind","ahead_by":0,"behind_by":3,"total_commits":0,"commits":[]}`
)

func ok(b string) resp { return resp{200, b} }

func newStub(t *testing.T, extra map[string]resp) *stub {
	r := map[string]resp{
		"compare/main..." + sha + "?per_page=1": ok(behind),
	}
	for k, v := range extra {
		r[k] = v
	}
	return &stub{t: t, routes: r}
}

func check(t *testing.T, s *stub, ref string) *Configuration {
	t.Helper()
	c := &Checker{ProtectedBranches: []string{"main"}}
	v, err := c.Configuration(context.Background(), s, "o", "app", ref, sha)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return v
}

func want(t *testing.T, v *Configuration, verdict string, reasonHas ...string) {
	t.Helper()
	if v.State != verdict {
		t.Fatalf("verdict %q, want %q: %v", v.State, verdict, v.Reasons)
	}
	all := strings.Join(v.Reasons, " | ")
	for _, h := range reasonHas {
		if !strings.Contains(all, h) {
			t.Fatalf("reasons %q do not mention %q", all, h)
		}
	}
	if len(v.Reasons) == 0 {
		t.Fatal("a verdict carries reasons")
	}
}

func TestBranchProtectedByRuleset(t *testing.T) {
	s := newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(rulesMain),
		"rulesets/100":             ok(mainRulesetAdminAndApp),
		"branches/main/protection": {404, notProt404},
	})
	v := check(t, s, "refs/heads/main")
	want(t, v, ConfProtected, "requires a reviewed pull request")
	// Administrators and a GitHub App are the platform's own door: reported, not counted against the branch.
	if len(v.Bypass) != 2 || v.Bypass[0].Moot || v.Bypass[1].Moot {
		t.Fatalf("bypass actors are reported, not moot: %+v", v.Bypass)
	}
	if len(v.ProtectedBy) != 1 || v.ProtectedBy[0] != "ruleset 100 (Repository)" {
		t.Fatalf("protectedBy: %v", v.ProtectedBy)
	}
}

func TestBranchBypassThatMakesItMoot(t *testing.T) {
	for name, actors := range map[string]string{
		"write role":       `{"actor_id":4,"actor_type":"RepositoryRole","bypass_mode":"always"}`,
		"maintain role":    `{"actor_id":2,"actor_type":"RepositoryRole","bypass_mode":"always"}`,
		"role, PR bypass":  `{"actor_id":4,"actor_type":"RepositoryRole","bypass_mode":"pull_request"}`,
		"a team":           `{"actor_id":9,"actor_type":"Team","bypass_mode":"always"}`,
		"a deploy key":     `{"actor_id":null,"actor_type":"DeployKey","bypass_mode":"always"}`,
		"a custom role id": `{"actor_id":3145678,"actor_type":"RepositoryRole","bypass_mode":"always"}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(mainRulesetAdminAndApp, `"bypass_actors":[`, `"bypass_actors":[`+actors+`,`, 1)
			s := newStub(t, map[string]resp{
				"rules/branches/main?per_page=100&page=1": ok(rulesMain),
				"rulesets/100":             ok(body),
				"branches/main/protection": {404, notProt404},
			})
			v := check(t, s, "refs/heads/main")
			want(t, v, ConfUnprotected, "may bypass it", "anyone with push rights")
			moot := 0
			for _, b := range v.Bypass {
				if b.Moot {
					moot++
				}
			}
			if moot != 1 {
				t.Fatalf("exactly the pushing-class actor is moot: %+v", v.Bypass)
			}
		})
	}
}

// The recorded shape the host returns to a reader that may not see the bypass list: no bypass_actors key at all.
func TestBypassListNotShownIsUnverifiableNotNone(t *testing.T) {
	anon := `{"id":100,"name":"main","target":"branch","source_type":"Repository","source":"o/app","enforcement":"active","conditions":{"ref_name":{"exclude":[],"include":["~DEFAULT_BRANCH"]}},"rules":[{"type":"deletion"}]}`
	s := newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(rulesMain),
		"rulesets/100":             ok(anon),
		"branches/main/protection": {404, notProt404},
	})
	want(t, check(t, s, "refs/heads/main"), ConfUnverifiable, "did not show its bypass actors")
}

func TestBranchMissingRequirements(t *testing.T) {
	prOnly := `[{"type":"pull_request","parameters":{"required_approving_review_count":1},"ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100}]`
	zero := `[{"type":"pull_request","parameters":{"required_approving_review_count":0},"ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},
	 {"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},{"type":"non_fast_forward","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100}]`
	s := newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(prOnly), "rulesets/100": ok(mainRulesetAdminAndApp), "branches/main/protection": {404, notProt404},
	})
	v := check(t, s, "refs/heads/main")
	want(t, v, ConfUnprotected, "blocking force pushes", "blocking deletion")
	if strings.Contains(strings.Join(v.Missing, ","), "approving review") {
		t.Fatalf("the review is there: %v", v.Missing)
	}
	s = newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(zero), "rulesets/100": ok(mainRulesetAdminAndApp), "branches/main/protection": {404, notProt404},
	})
	want(t, check(t, s, "refs/heads/main"), ConfUnprotected, "at least one approving review")
}

func TestBranchNoProtectionAtAll(t *testing.T) {
	s := newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(`[]`), "branches/main/protection": {404, notProt404}})
	v := check(t, s, "refs/heads/main")
	want(t, v, ConfUnprotected, "missing pull requests")
	if v.State == ConfProtected || len(v.Missing) != 4 {
		t.Fatalf("%+v", v)
	}
}

const classicOK = `{"url":"https://api.github.com/repos/o/app/branches/main/protection",
 "required_pull_request_reviews":{"dismiss_stale_reviews":true,"require_code_owner_reviews":false,"required_approving_review_count":2,"bypass_pull_request_allowances":{"users":[],"teams":[],"apps":[]}},
 "enforce_admins":{"enabled":true},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false},"required_linear_history":{"enabled":false}}`

func TestBranchClassicProtection(t *testing.T) {
	s := newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(`[]`), "branches/main/protection": ok(classicOK)})
	v := check(t, s, "refs/heads/main")
	want(t, v, ConfProtected, ">=2 approving")
	if len(v.ProtectedBy) != 1 || v.ProtectedBy[0] != "branch protection" {
		t.Fatalf("%v", v.ProtectedBy)
	}
	// Force pushes allowed: not protected.
	s = newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(`[]`),
		"branches/main/protection": ok(strings.Replace(classicOK, `"allow_force_pushes":{"enabled":false}`, `"allow_force_pushes":{"enabled":true}`, 1))})
	want(t, check(t, s, "refs/heads/main"), ConfUnprotected, "blocking force pushes")
	// enforce_admins off: administrators bypass, reported and not moot.
	s = newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(`[]`),
		"branches/main/protection": ok(strings.Replace(classicOK, `"enforce_admins":{"enabled":true}`, `"enforce_admins":{"enabled":false}`, 1))})
	v = check(t, s, "refs/heads/main")
	want(t, v, ConfProtected)
	if len(v.Bypass) != 1 || v.Bypass[0].Moot || v.Bypass[0].ActorID != adminRole {
		t.Fatalf("admins are reported: %+v", v.Bypass)
	}
	// A named user allowed to bypass reviews is moot.
	s = newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(`[]`),
		"branches/main/protection": ok(strings.Replace(classicOK, `"users":[]`, `"users":[{"login":"dev"}]`, 1))})
	want(t, check(t, s, "refs/heads/main"), ConfUnprotected, "may bypass it")
}

// Rulesets and classic protection add up: a ruleset that blocks force pushes plus classic reviews is protected.
func TestBranchRulesetAndClassicCombine(t *testing.T) {
	nff := `[{"type":"non_fast_forward","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":7},{"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":7}]`
	rs := `{"id":7,"target":"branch","enforcement":"active","bypass_actors":[]}`
	s := newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(nff), "rulesets/7": ok(rs), "branches/main/protection": ok(classicOK)})
	want(t, check(t, s, "refs/heads/main"), ConfProtected)
}

func TestPlanNotAvailable(t *testing.T) {
	// A private repository on the free plan: rules read as empty, classic protection and the rulesets list both answer the upgrade message.
	s := newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(`[]`), "branches/main/protection": {403, upgrade403}, "rulesets?per_page=1": {403, upgrade403}})
	v := check(t, s, "refs/heads/main")
	want(t, v, ConfUnavailable, "no rulesets or branch protection", "review evidence does not need them")
	// The rules endpoint itself gated.
	s = newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": {403, upgrade403}, "branches/main/protection": {403, upgrade403}})
	want(t, check(t, s, "refs/heads/main"), ConfUnavailable)
	// Classic protection exists on the plan (404 not protected) while rulesets are gated: that is "unprotected", not "unavailable".
	s = newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(`[]`), "branches/main/protection": {404, notProt404}})
	want(t, check(t, s, "refs/heads/main"), ConfUnprotected)
	// A 404 that is not the upgrade message (a hidden repository, a typo) is never "unavailable".
	s = newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(`[]`), "branches/main/protection": {404, `{"message":"Not Found"}`}})
	c := &Checker{}
	if v, err := c.Configuration(context.Background(), s, "o", "app", "refs/heads/main", sha); err == nil {
		t.Fatalf("an unrecognised 404 is an error, got %+v", v)
	}
}

func TestClassicNotReadableByTheToken(t *testing.T) {
	s := newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(`[]`), "branches/main/protection": {403, noAdmin403}})
	want(t, check(t, s, "refs/heads/main"), ConfUnverifiable, "Administration: read")
}

func TestHostErrorsFailClosed(t *testing.T) {
	c := &Checker{ProtectedBranches: []string{"main"}}
	for name, routes := range map[string]map[string]resp{
		"rules 500":       {"rules/branches/main?per_page=100&page=1": {500, `{"message":"boom"}`}},
		"rules transport": {"rules/branches/main?per_page=100&page=1": {0, ""}},
		"rules 401":       {"rules/branches/main?per_page=100&page=1": {401, `{"message":"Bad credentials"}`}},
		"ruleset 502":     {"rules/branches/main?per_page=100&page=1": ok(rulesMain), "rulesets/100": {502, ""}, "branches/main/protection": {404, notProt404}},
		"compare 500": {"rules/branches/main?per_page=100&page=1": ok(rulesMain), "rulesets/100": ok(mainRulesetAdminAndApp), "branches/main/protection": {404, notProt404},
			"compare/main..." + sha + "?per_page=1": {500, ""}},
	} {
		t.Run(name, func(t *testing.T) {
			s := &stub{t: t, routes: routes}
			v, err := c.Configuration(context.Background(), s, "o", "app", "refs/heads/main", sha)
			if name == "ruleset 502" {
				// An unreadable ruleset is "could not decide", which is a verdict that refuses.
				if err != nil || v.State != ConfUnverifiable {
					t.Fatalf("%v %+v", err, v)
				}
				return
			}
			if err == nil {
				t.Fatalf("a host failure is an error, got %+v", v)
			}
		})
	}
}

func TestBranchCommitMustBeOnTheBranch(t *testing.T) {
	for _, status := range []string{"ahead", "diverged"} {
		s := newStub(t, map[string]resp{
			"rules/branches/main?per_page=100&page=1": ok(rulesMain), "rulesets/100": ok(mainRulesetAdminAndApp), "branches/main/protection": {404, notProt404},
			"compare/main..." + sha + "?per_page=1": ok(`{"status":"` + status + `"}`)})
		want(t, check(t, s, "refs/heads/main"), ConfUnprotected, "is not on branch main", status)
	}
}

func TestRulesPagedAndBounded(t *testing.T) {
	page := func(n int, withPR bool) string {
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, `{"type":"creation","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":1}`)
		}
		if withPR {
			parts[0] = `{"type":"pull_request","parameters":{"required_approving_review_count":1},"ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100}`
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	// The pull_request rule is on page 2: it is found.
	s := newStub(t, map[string]resp{
		"rules/branches/main?per_page=100&page=1": ok(page(100, false)),
		"rules/branches/main?per_page=100&page=2": ok(`[{"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":true},"ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},{"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},{"type":"non_fast_forward","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100}]`),
		"rulesets/100": ok(mainRulesetAdminAndApp), "branches/main/protection": {404, notProt404}})
	want(t, check(t, s, "refs/heads/main"), ConfProtected)
	// Never more than the bound, and a repository past it is not "protected".
	routes := map[string]resp{"branches/main/protection": {404, notProt404}}
	for p := 1; p <= maxRulePages; p++ {
		routes[fmt.Sprintf("rules/branches/main?per_page=100&page=%d", p)] = ok(page(100, false))
	}
	s = newStub(t, routes)
	want(t, check(t, s, "refs/heads/main"), ConfUnverifiable, "not read further")
	if n := s.called("rules/branches/"); n != maxRulePages {
		t.Fatalf("read %d pages, bound is %d", n, maxRulePages)
	}
}

// ---- tags. These are the shapes of a real repository that protects its tags with two rulesets (create+update, delete).

func tagRulesetJSON(id int, include, exclude string, enforcement string, rules ...string) string {
	var rs []string
	for _, r := range rules {
		rs = append(rs, `{"type":"`+r+`"}`)
	}
	return fmt.Sprintf(`{"id":%d,"name":"tags","target":"tag","source_type":"Repository","source":"o/app","enforcement":%q,
	 "conditions":{"ref_name":{"exclude":[%s],"include":[%s]}},"rules":[%s],"bypass_actors":[{"actor_id":123,"actor_type":"Integration","bypass_mode":"always"}]}`,
		id, enforcement, exclude, include, strings.Join(rs, ","))
}

const tagList = `[{"id":200,"name":"create","target":"tag","source_type":"Repository","source":"o/app","enforcement":"active"},
 {"id":201,"name":"delete","target":"tag","source_type":"Repository","source":"o/app","enforcement":"active"},
 {"id":300,"name":"main","target":"branch","source_type":"Repository","source":"o/app","enforcement":"active"}]`

func tagStub(t *testing.T, extra map[string]resp) *stub {
	routes := map[string]resp{
		"rulesets?includes_parents=true&per_page=100&page=1": ok(tagList),
		"rulesets/200": ok(tagRulesetJSON(200, `"refs/tags/v*"`, ``, "active", "creation", "update")),
		"rulesets/201": ok(tagRulesetJSON(201, `"refs/tags/v*"`, ``, "active", "deletion")),
		// the branch the tag's commit must be on
		"rules/branches/main?per_page=100&page=1": ok(rulesMain), "rulesets/100": ok(mainRulesetAdminAndApp), "branches/main/protection": {404, notProt404},
	}
	for k, v := range extra {
		routes[k] = v
	}
	return newStub(t, routes)
}

func TestTagProtected(t *testing.T) {
	s := tagStub(t, nil)
	v := check(t, s, "refs/tags/v1.2.3")
	want(t, v, ConfProtected, "creation, update and deletion", "protected branch main")
	if len(v.ProtectedBy) < 3 {
		t.Fatalf("what protects the tag is recorded: %v", v.ProtectedBy)
	}
}

func TestTagOnAnUnprotectedCommit(t *testing.T) {
	s := tagStub(t, map[string]resp{"compare/main..." + sha + "?per_page=1": ok(`{"status":"diverged"}`)})
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnprotected, "not on a protected branch", "diverged")
	// On a branch that is itself unprotected.
	s = tagStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(`[]`)})
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnprotected, "main is unprotected")
}

func TestTagRulesetGaps(t *testing.T) {
	// Only create+update: nothing restricts deletion.
	s := tagStub(t, map[string]resp{"rulesets/201": ok(tagRulesetJSON(201, `"refs/tags/v*"`, ``, "active", "non_fast_forward"))})
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnprotected, "restricting deletion")
	// A ruleset in evaluate mode does not enforce.
	s = tagStub(t, map[string]resp{"rulesets?includes_parents=true&per_page=100&page=1": ok(strings.Replace(tagList, `"id":201,"name":"delete","target":"tag","source_type":"Repository","source":"o/app","enforcement":"active"`, `"id":201,"name":"delete","target":"tag","source_type":"Repository","source":"o/app","enforcement":"evaluate"`, 1))})
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnprotected, "restricting deletion")
	// The pattern does not cover this tag, or excludes it.
	s = tagStub(t, map[string]resp{"rulesets/200": ok(tagRulesetJSON(200, `"refs/tags/release-*"`, ``, "active", "creation", "update"))})
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnprotected, "restricting creation")
	s = tagStub(t, map[string]resp{"rulesets/200": ok(tagRulesetJSON(200, `"~ALL"`, `"refs/tags/v1.2.*"`, "active", "creation", "update"))})
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnprotected, "restricting creation")
	// No tag rulesets at all.
	s = tagStub(t, map[string]resp{"rulesets?includes_parents=true&per_page=100&page=1": ok(`[]`)})
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnprotected, "restricting creation", "restricting update", "restricting deletion")
}

func TestTagBypassByPushRights(t *testing.T) {
	bad := strings.Replace(tagRulesetJSON(200, `"refs/tags/v*"`, ``, "active", "creation", "update"), `"bypass_actors":[`, `"bypass_actors":[{"actor_id":4,"actor_type":"RepositoryRole","bypass_mode":"always"},`, 1)
	s := tagStub(t, map[string]resp{"rulesets/200": ok(bad)})
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnprotected, "anyone with push rights could still create it")
}

func TestTagPlanAndConfig(t *testing.T) {
	s := &stub{t: t, routes: map[string]resp{"rulesets?includes_parents=true&per_page=100&page=1": {403, upgrade403}}}
	want(t, check(t, s, "refs/tags/v1.2.3"), ConfUnavailable, "review evidence does not need them")
	s = tagStub(t, nil)
	v, err := (&Checker{}).Configuration(context.Background(), s, "o", "app", "refs/tags/v1.2.3", sha)
	if err != nil {
		t.Fatal(err)
	}
	want(t, v, ConfUnprotected, "no protected branch is configured")
	s = &stub{t: t, routes: map[string]resp{"rulesets?includes_parents=true&per_page=100&page=1": {500, ""}}}
	if _, err := (&Checker{}).Configuration(context.Background(), s, "o", "app", "refs/tags/v1.2.3", sha); err == nil {
		t.Fatal("a host error is an error")
	}
}

func TestInvalidInputs(t *testing.T) {
	c := &Checker{}
	for _, tc := range [][2]string{
		{"main", sha}, {"refs/heads/", sha}, {"refs/pull/1/head", sha}, {"refs/heads/a b", sha}, {"refs/heads/main", "abc"}, {"refs/heads/main", strings.Repeat("A", 40)}, {"refs/heads/main", sha + "0"},
	} {
		_, err := c.Check(context.Background(), &stub{t: t}, "o", "app", tc[0], tc[1], nil)
		var inv Invalid
		if !errors.As(err, &inv) {
			t.Fatalf("%v: want Invalid, got %v", tc, err)
		}
	}
}

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pat, ref string
		want     bool
	}{
		{"refs/tags/v*", "refs/tags/v1.2.3", true}, {"refs/tags/v*", "refs/tags/x1", false}, {"refs/tags/v*", "refs/tags/v1/2", false},
		{"refs/tags/**", "refs/tags/a/b", true}, {"refs/tags/v1.2.3", "refs/tags/v1.2.3", true}, {"refs/tags/v1.2.3", "refs/tags/v1x2x3", false},
		{"refs/tags/v?", "refs/tags/v1", true},
	} {
		if got := globMatch(tc.pat, tc.ref); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v", tc.pat, tc.ref, got)
		}
	}
}

// Nothing is remembered between calls: a second check reads the host again and sees what changed.
func TestNothingIsCached(t *testing.T) {
	routes := map[string]resp{"rules/branches/main?per_page=100&page=1": ok(rulesMain), "rulesets/100": ok(mainRulesetAdminAndApp), "branches/main/protection": {404, notProt404},
		"compare/main..." + sha + "?per_page=1": ok(behind)}
	s := &stub{t: t, routes: routes}
	c := &Checker{}
	v1, _ := c.Configuration(context.Background(), s, "o", "app", "refs/heads/main", sha)
	routes["rules/branches/main?per_page=100&page=1"] = ok(`[]`)
	v2, _ := c.Configuration(context.Background(), s, "o", "app", "refs/heads/main", sha)
	if v1.State != ConfProtected || v2.State != ConfUnprotected {
		t.Fatalf("%s then %s", v1.State, v2.State)
	}
}

// ---- review parameters: an approval on a benign commit must not carry to a malicious later push.

func rulesWith(params string) string {
	return `[{"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},
	 {"type":"non_fast_forward","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},
	 {"type":"pull_request","parameters":{` + params + `},"ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100}]`
}

func branchWith(t *testing.T, rules string) *Configuration {
	s := newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(rules), "rulesets/100": ok(mainRulesetAdminAndApp), "branches/main/protection": {404, notProt404}})
	return check(t, s, "refs/heads/main")
}

func TestReviewParametersAreRequired(t *testing.T) {
	// approvals alone: a stale approval survives a later push
	v := branchWith(t, rulesWith(`"required_approving_review_count":1`))
	want(t, v, ConfUnprotected, "dismiss_stale_reviews_on_push", "require_last_push_approval")
	if len(v.Missing) != 1 {
		t.Fatalf("%v", v.Missing)
	}
	// explicit false for both
	want(t, branchWith(t, rulesWith(`"required_approving_review_count":2,"dismiss_stale_reviews_on_push":false,"require_last_push_approval":false`)), ConfUnprotected, "dismiss_stale_reviews_on_push")
	// either one is enough
	want(t, branchWith(t, rulesWith(`"required_approving_review_count":1,"dismiss_stale_reviews_on_push":true`)), ConfProtected)
	want(t, branchWith(t, rulesWith(`"required_approving_review_count":1,"require_last_push_approval":true`)), ConfProtected)
	// the stale protection without an approval is still not a review
	want(t, branchWith(t, rulesWith(`"required_approving_review_count":0,"dismiss_stale_reviews_on_push":true`)), ConfUnprotected, "at least one approving review")
}

func TestCodeOwnerAndThreadResolutionAreReportedNotRequired(t *testing.T) {
	v := branchWith(t, rulesWith(`"required_approving_review_count":1,"dismiss_stale_reviews_on_push":true,"require_code_owner_review":true,"required_review_thread_resolution":true`))
	want(t, v, ConfProtected)
	got := strings.Join(v.Notes, "|")
	if !strings.Contains(got, "require_code_owner_review: on") || !strings.Contains(got, "required_review_thread_resolution: on") {
		t.Fatalf("notes %v", v.Notes)
	}
	v = branchWith(t, rulesWith(`"required_approving_review_count":1,"dismiss_stale_reviews_on_push":true`))
	want(t, v, ConfProtected)
	if !strings.Contains(strings.Join(v.Notes, "|"), "require_code_owner_review: off") {
		t.Fatalf("off is reported too: %v", v.Notes)
	}
}

// The requirements add up across the rulesets that apply: approvals in one, stale dismissal in another.
func TestReviewParametersAcrossRulesets(t *testing.T) {
	rules := `[{"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},
	 {"type":"non_fast_forward","ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},
	 {"type":"pull_request","parameters":{"required_approving_review_count":1},"ruleset_source_type":"Repository","ruleset_source":"o/app","ruleset_id":100},
	 {"type":"pull_request","parameters":{"required_approving_review_count":0,"dismiss_stale_reviews_on_push":true},"ruleset_source_type":"Organization","ruleset_source":"o","ruleset_id":101}]`
	s := newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(rules), "rulesets/100": ok(mainRulesetAdminAndApp),
		"rulesets/101": ok(strings.Replace(mainRulesetAdminAndApp, `"id":100`, `"id":101`, 1)), "branches/main/protection": {404, notProt404}})
	v := check(t, s, "refs/heads/main")
	want(t, v, ConfProtected)
	if len(v.ProtectedBy) != 2 {
		t.Fatalf("both rulesets' bypass lists were read: %v", v.ProtectedBy)
	}
}

func TestClassicReviewParameters(t *testing.T) {
	noStale := strings.Replace(classicOK, `"dismiss_stale_reviews":true`, `"dismiss_stale_reviews":false`, 1)
	s := newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(`[]`), "branches/main/protection": ok(noStale)})
	want(t, check(t, s, "refs/heads/main"), ConfUnprotected, "dismiss_stale_reviews_on_push")
	lastPush := strings.Replace(noStale, `"require_code_owner_reviews":false`, `"require_code_owner_reviews":true,"require_last_push_approval":true`, 1)
	s = newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(`[]`), "branches/main/protection": ok(lastPush)})
	v := check(t, s, "refs/heads/main")
	want(t, v, ConfProtected)
	if !strings.Contains(strings.Join(v.Notes, "|"), "require_code_owner_review: on") {
		t.Fatalf("%v", v.Notes)
	}
}

// ---- refs: dot segments, empty segments, encoded slashes

func TestRefSegmentsAreRejected(t *testing.T) {
	c := &Checker{}
	for _, ref := range []string{
		"refs/heads/..", "refs/heads/.", "refs/heads/a/../main", "refs/heads/a/./b", "refs/heads/../x", "refs/heads/a//b", "refs/heads/a/", "refs/heads//a",
		"refs/tags/..", "refs/tags/v1/../v2", "refs/heads/a%2Fb", "refs/heads/a%2f..%2fb", "refs/tags/v1%2F2", "refs/heads/a\\b",
	} {
		_, err := c.Check(context.Background(), &stub{t: t}, "o", "app", ref, sha, nil)
		var inv Invalid
		if !errors.As(err, &inv) {
			t.Errorf("%q: want Invalid, got %v", ref, err)
		}
	}
	// a legitimate nested name still passes validation (it is not a protected branch here, so no evidence is read)
	if v, err := c.Check(context.Background(), &stub{t: t}, "o", "app", "refs/heads/release/1.x", sha, nil); err != nil || v.Verdict != Unverifiable {
		t.Fatalf("%v %+v", err, v)
	}
}

// ---- annotated tags: the commit is what Pipelines-as-Code records (head_commit.id); a tag object's sha is refused.

func TestTagObjectShaIsRefused(t *testing.T) {
	for _, st := range []int{404, 422} {
		s := tagStub(t, map[string]resp{"compare/main..." + sha + "?per_page=1": {st, `{"message":"No common ancestor between main and ` + sha + `"}`}})
		v := check(t, s, "refs/tags/v1.2.3")
		want(t, v, ConfUnprotected, "not a commit", "tag's own object")
	}
	// the all-zero sha (a push event with no head commit) is refused up front
	_, err := (&Checker{}).Check(context.Background(), &stub{t: t}, "o", "app", "refs/tags/v1.2.3", strings.Repeat("0", 40), nil)
	var inv Invalid
	if !errors.As(err, &inv) {
		t.Fatalf("zero sha: %v", err)
	}
}

// ---- an unreadable bypass list is a note, not a failure (the read token is not shown it; the verdict does not need it).

func TestBypassUnreadableIsANote(t *testing.T) {
	anon := `{"id":100,"target":"branch","enforcement":"active","rules":[{"type":"deletion"}]}`
	s := newStub(t, map[string]resp{"rules/branches/main?per_page=100&page=1": ok(rulesMain), "rulesets/100": ok(anon), "branches/main/protection": {404, notProt404}})
	v := check(t, s, "refs/heads/main")
	want(t, v, ConfUnverifiable, "did not show its bypass actors", "read-only token", "not needed for the verdict")
	for _, r := range v.Reasons {
		if strings.Contains(r, "refProtectionPermissions") || strings.Contains(r, "Administration: write") {
			t.Fatalf("the elevated permission path is gone, but a reason still points to it: %s", r)
		}
	}
}
