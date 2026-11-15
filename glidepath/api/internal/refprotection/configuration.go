// Configuration: what the git host reports about a ref's protection (rulesets, classic branch protection, bypass
// lists). Since pipeline trust T3b this is INFORMATIONAL: it never decides the verdict (that rests on review evidence,
// evidence.go). It is read best effort with the read token the platform already mints, so a bypass list GitHub shows
// only to a token with Administration: write is reported as "not shown", not as a failure, and a read that fails is
// reported as such. Nothing here is cached.
package refprotection

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Bounds on the informational configuration reads: a repository with more than this is not read further; the note says so.
const (
	maxRulePages    = 5  // x100 rules for one branch
	maxRulesetPages = 3  // x100 rulesets
	maxRulesetReads = 25 // rulesets fetched one by one (tags, and bypass lists)
)

// The configuration's own states (not the verdict's).
const (
	ConfProtected    = "protected"
	ConfUnprotected  = "unprotected"
	ConfUnavailable  = "unavailable"
	ConfUnverifiable = "unverifiable"
)

// Bypass is one actor a ruleset or protection lets past its rules.
type Bypass struct {
	Source    string `json:"source"`    // "ruleset 123 (Repository)" or "branch protection"
	ActorType string `json:"actorType"` // RepositoryRole, Team, Integration, OrganizationAdmin, DeployKey, User
	ActorID   int64  `json:"actorId,omitempty"`
	Mode      string `json:"mode,omitempty"` // always | pull_request (rulesets)
	Moot      bool   `json:"moot"`           // true: this actor class makes the protection moot for "anyone with push rights"
	Note      string `json:"note,omitempty"`
}

// Configuration is what the host's own settings say about the ref, for a person to read (never the verdict).
type Configuration struct {
	// State: protected (the settings would require reviewed pull requests, block force pushes and deletion, with no
	// push-rights bypass), unprotected, unavailable (the host's plan has no rulesets or branch protection), or
	// unverifiable (the read token is not shown enough, or the host did not answer).
	State       string   `json:"state"`
	Reasons     []string `json:"reasons"`
	Missing     []string `json:"missing,omitempty"`
	Bypass      []Bypass `json:"bypass,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	ProtectedBy []string `json:"protectedBy,omitempty"`

	repo, ref, sha, kind string
}

// Checker reads the host through Reader and decides.
type Checker struct {
	ProtectedBranches []string // the branches a release may come from (the admission policy's protectedBranches)
	// TagRefPattern is the admission policy's tagRefPattern, matched against the full ref. nil: every tag is main-like.
	TagRefPattern *regexp.Regexp
	// BotLogins are accounts (the pipeline's own GitHub App logins) whose approval never counts, besides every account
	// the host types Bot or names with a [bot] suffix.
	BotLogins []string
}

// Configuration reads the host's settings for owner/repo at ref and sha (already validated by Check).
func (c *Checker) Configuration(ctx context.Context, rd Reader, owner, repo, ref, sha string) (*Configuration, error) {
	v := &Configuration{repo: owner + "/" + repo, ref: ref, sha: sha}
	switch {
	case branchRefRE.MatchString(ref):
		v.kind = "branch"
		return c.branch(ctx, rd, owner, repo, strings.TrimPrefix(ref, "refs/heads/"), sha, v)
	case tagRefRE.MatchString(ref):
		v.kind = "tag"
		return c.tag(ctx, rd, owner, repo, strings.TrimPrefix(ref, "refs/tags/"), sha, v)
	}
	return nil, Invalid{"ref must be a full ref: refs/heads/<branch> or refs/tags/<tag>"}
}

func (v *Configuration) finish(state string, reasons ...string) *Configuration {
	v.State = state
	v.Reasons = append(v.Reasons, reasons...)
	return v
}

// ---- branch

// branchFacts is what one branch's protection shows, before the verdict.
type branchFacts struct {
	approvals    int  // the most approving reviews any source requires
	staleDismiss bool // dismiss_stale_reviews_on_push (rulesets) / dismiss_stale_reviews (classic)
	lastPush     bool // require_last_push_approval
	codeOwner    bool // reported only
	threads      bool // reported only
	noForcePush  bool
	noDelete     bool
	protectedBy  []string
	bypass       []Bypass
	unavailable  bool     // the host has no rulesets / branch protection for this repository
	unverifiable []string // why the host did not show enough
}

func (c *Checker) branch(ctx context.Context, rd Reader, owner, repo, branch, sha string, v *Configuration) (*Configuration, error) {
	f, err := c.branchFacts(ctx, rd, owner, repo, branch)
	if err != nil {
		return nil, err
	}
	v.Bypass = f.bypass
	v.ProtectedBy = f.protectedBy
	if len(f.unverifiable) > 0 {
		return v.finish(ConfUnverifiable, f.unverifiable...), nil
	}
	if f.unavailable && len(f.protectedBy) == 0 {
		return v.finish(ConfUnavailable, fmt.Sprintf("%s/%s: the git host offers no rulesets or branch protection for this repository on its plan (it said so) (informational: review evidence does not need them)", owner, repo)), nil
	}
	if f.approvals < 1 {
		v.Missing = append(v.Missing, "pull requests with at least one approving review (required_approving_review_count >= 1)")
	}
	if !f.staleDismiss && !f.lastPush {
		// Without one of these an approval given to a harmless commit still counts after a malicious one is pushed.
		v.Missing = append(v.Missing, "dismiss_stale_reviews_on_push or require_last_push_approval (an approval must not survive a later push)")
	}
	v.Notes = append(v.Notes, onOff("require_code_owner_review", f.codeOwner), onOff("required_review_thread_resolution", f.threads))
	if !f.noForcePush {
		v.Missing = append(v.Missing, "blocking force pushes")
	}
	if !f.noDelete {
		v.Missing = append(v.Missing, "blocking deletion")
	}
	moot := mootBypass(f.bypass)
	if len(v.Missing) > 0 {
		return v.finish(ConfUnprotected, fmt.Sprintf("branch %s is not protected as required: missing %s", branch, strings.Join(v.Missing, ", "))), nil
	}
	if len(moot) > 0 {
		return v.finish(ConfUnprotected, fmt.Sprintf("branch %s is protected, but %s may bypass it, so anyone with push rights could still land an unreviewed commit", branch, strings.Join(moot, ", "))), nil
	}
	// The commit must be on the branch: a sha that is not an ancestor of the protected head did not arrive by it.
	ok, why, err := reachable(ctx, rd, owner, repo, branch, sha)
	if err != nil {
		return nil, err
	}
	if !ok {
		return v.finish(ConfUnprotected, fmt.Sprintf("commit %s is not on branch %s (%s)", short(sha), branch, why)), nil
	}
	return v.finish(ConfProtected, fmt.Sprintf("branch %s requires a reviewed pull request (>=%d approving), blocks force pushes and deletion, and no push-rights actor class may bypass it; %s is on it", branch, f.approvals, short(sha))), nil
}

func onOff(name string, on bool) string {
	if on {
		return name + ": on"
	}
	return name + ": off"
}

func mootBypass(b []Bypass) []string {
	var out []string
	for _, x := range b {
		if x.Moot {
			out = append(out, fmt.Sprintf("%s %s%s (%s)", x.ActorType, idStr(x.ActorID), modeStr(x.Mode), x.Source))
		}
	}
	return out
}
func idStr(id int64) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprint(id)
}
func modeStr(m string) string {
	if m == "" {
		return ""
	}
	return " [" + m + "]"
}

type ruleEntry struct {
	Type       string `json:"type"`
	Parameters struct {
		Approvals    int  `json:"required_approving_review_count"`
		StaleDismiss bool `json:"dismiss_stale_reviews_on_push"`
		LastPush     bool `json:"require_last_push_approval"`
		CodeOwner    bool `json:"require_code_owner_review"`
		Threads      bool `json:"required_review_thread_resolution"`
	} `json:"parameters"`
	RulesetID         int64  `json:"ruleset_id"`
	RulesetSourceType string `json:"ruleset_source_type"`
}

func (c *Checker) branchFacts(ctx context.Context, rd Reader, owner, repo, branch string) (*branchFacts, error) {
	f := &branchFacts{}
	// Rulesets: the rules that apply to this branch (active ones only; the host evaluates conditions and inheritance).
	var rules []ruleEntry
	rulesUnavailable := false
	for page := 1; page <= maxRulePages; page++ {
		st, body, err := rd.GetJSON(ctx, owner, repo, fmt.Sprintf("rules/branches/%s?per_page=100&page=%d", escapeSegs(branch), page))
		if err != nil {
			return nil, err
		}
		if upgradeAnswer(st, body) {
			rulesUnavailable = true
			break
		}
		if st != http.StatusOK {
			return nil, fmt.Errorf("GET rules/branches/%s failed with %d: %s", branch, st, hostMessage(body))
		}
		var got []ruleEntry
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, fmt.Errorf("GET rules/branches/%s: %w", branch, err)
		}
		rules = append(rules, got...)
		if len(got) < 100 {
			break
		}
		if page == maxRulePages {
			f.unverifiable = append(f.unverifiable, fmt.Sprintf("branch %s has more than %d rules: not read further", branch, maxRulePages*100))
		}
	}
	// Which rulesets supply each requirement, so their bypass lists are read (and only theirs).
	contributing := map[int64]string{}
	for _, r := range rules {
		switch r.Type {
		case "pull_request":
			if r.Parameters.Approvals > f.approvals {
				f.approvals = r.Parameters.Approvals
			}
			f.staleDismiss = f.staleDismiss || r.Parameters.StaleDismiss
			f.lastPush = f.lastPush || r.Parameters.LastPush
			f.codeOwner = f.codeOwner || r.Parameters.CodeOwner
			f.threads = f.threads || r.Parameters.Threads
			// Every ruleset that supplies a review requirement is one whose bypass list matters.
			contributing[r.RulesetID] = r.RulesetSourceType
		case "non_fast_forward":
			f.noForcePush = true
			contributing[r.RulesetID] = r.RulesetSourceType
		case "deletion":
			f.noDelete = true
			contributing[r.RulesetID] = r.RulesetSourceType
		}
	}
	ids := make([]int64, 0, len(contributing))
	for id := range contributing {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > maxRulesetReads {
		f.unverifiable = append(f.unverifiable, fmt.Sprintf("more than %d rulesets apply to branch %s: not read further", maxRulesetReads, branch))
		ids = ids[:maxRulesetReads]
	}
	for _, id := range ids {
		src := fmt.Sprintf("ruleset %d (%s)", id, contributing[id])
		by, why, err := rulesetBypass(ctx, rd, owner, repo, id, src)
		if err != nil {
			return nil, err
		}
		if why != "" {
			f.unverifiable = append(f.unverifiable, why)
			continue
		}
		f.bypass = append(f.bypass, by...)
		f.protectedBy = append(f.protectedBy, src)
	}
	// Classic branch protection (Administration: read on the token; without it the answer below says so).
	st, body, err := rd.GetJSON(ctx, owner, repo, "branches/"+escapeSegs(branch)+"/protection")
	if err != nil {
		return nil, err
	}
	classicUnavailable := false
	switch {
	case st == http.StatusOK:
		c := classicFacts(body, branch)
		if c.err != "" {
			f.unverifiable = append(f.unverifiable, c.err)
		} else {
			if c.approvals > f.approvals {
				f.approvals = c.approvals
			}
			f.staleDismiss = f.staleDismiss || c.staleDismiss
			f.lastPush = f.lastPush || c.lastPush
			f.codeOwner = f.codeOwner || c.codeOwner
			f.threads = f.threads || c.threads
			f.noForcePush = f.noForcePush || c.noForcePush
			f.noDelete = f.noDelete || c.noDelete
			f.bypass = append(f.bypass, c.bypass...)
			f.protectedBy = append(f.protectedBy, "branch protection")
		}
	case upgradeAnswer(st, body):
		classicUnavailable = true
	case st == http.StatusNotFound && strings.Contains(strings.ToLower(hostMessage(body)), "branch not protected"):
		// no classic protection: rulesets alone decide
	case st == http.StatusForbidden && strings.Contains(strings.ToLower(hostMessage(body)), "not accessible"):
		// The read token has no Administration: read: classic protection is not shown. Rulesets still decide, but a
		// branch that only classic protection guards cannot be verified.
		if f.approvals < 1 || !f.noForcePush || !f.noDelete {
			f.unverifiable = append(f.unverifiable, fmt.Sprintf("classic branch protection on %s cannot be read: the platform's read token does not hold the Administration: read permission (informational only: docs/admin/security.md, Ref protection)", branch))
		}
	default:
		return nil, fmt.Errorf("GET branches/%s/protection failed with %d: %s", branch, st, hostMessage(body))
	}
	// "Not on the plan" is believed only when nothing protects the branch and the host says so for BOTH mechanisms:
	// classic protection answered its upgrade message, and so did rulesets (the rules read, or a probe of the rulesets
	// list: some plans answer an empty rules list for a repository whose rulesets are gated). A host that answers
	// "no protection" for either is a host where it is available, and the branch is simply unprotected.
	if len(f.protectedBy) == 0 && len(f.unverifiable) == 0 && classicUnavailable {
		gated := rulesUnavailable
		if !gated {
			st, body, err := rd.GetJSON(ctx, owner, repo, "rulesets?per_page=1")
			if err != nil {
				return nil, err
			}
			gated = upgradeAnswer(st, body)
		}
		f.unavailable = gated
	}
	return f, nil
}

type classic struct {
	staleDismiss, lastPush bool
	codeOwner, threads     bool
	approvals              int
	noForcePush, noDelete  bool
	bypass                 []Bypass
	err                    string
}

func classicFacts(body []byte, branch string) classic {
	var p struct {
		Reviews *struct {
			Approvals    int  `json:"required_approving_review_count"`
			StaleDismiss bool `json:"dismiss_stale_reviews"`
			LastPush     bool `json:"require_last_push_approval"`
			CodeOwner    bool `json:"require_code_owner_reviews"`
			Allow        *struct {
				Users []json.RawMessage `json:"users"`
				Teams []json.RawMessage `json:"teams"`
				Apps  []json.RawMessage `json:"apps"`
			} `json:"bypass_pull_request_allowances"`
		} `json:"required_pull_request_reviews"`
		Conversations *struct {
			Enabled bool `json:"enabled"`
		} `json:"required_conversation_resolution"`
		EnforceAdmins *struct {
			Enabled bool `json:"enabled"`
		} `json:"enforce_admins"`
		AllowForcePushes *struct {
			Enabled bool `json:"enabled"`
		} `json:"allow_force_pushes"`
		AllowDeletions *struct {
			Enabled bool `json:"enabled"`
		} `json:"allow_deletions"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return classic{err: fmt.Sprintf("branch protection of %s could not be read: %v", branch, err)}
	}
	out := classic{}
	if p.Reviews != nil {
		out.approvals = p.Reviews.Approvals
		out.staleDismiss, out.lastPush, out.codeOwner = p.Reviews.StaleDismiss, p.Reviews.LastPush, p.Reviews.CodeOwner
		if a := p.Reviews.Allow; a != nil {
			for range a.Users {
				out.bypass = append(out.bypass, Bypass{Source: "branch protection", ActorType: "User", Moot: true, Note: "may bypass pull request reviews"})
			}
			for range a.Teams {
				out.bypass = append(out.bypass, Bypass{Source: "branch protection", ActorType: "Team", Moot: true, Note: "may bypass pull request reviews"})
			}
			for range a.Apps {
				out.bypass = append(out.bypass, Bypass{Source: "branch protection", ActorType: "Integration", Note: "may bypass pull request reviews (a GitHub App)"})
			}
		}
	}
	out.threads = p.Conversations != nil && p.Conversations.Enabled
	// An absent flag is the host's default: force pushes and deletions are off. Only an explicit enabled: true allows them.
	out.noForcePush = p.AllowForcePushes == nil || !p.AllowForcePushes.Enabled
	out.noDelete = p.AllowDeletions == nil || !p.AllowDeletions.Enabled
	if p.EnforceAdmins == nil || !p.EnforceAdmins.Enabled {
		out.bypass = append(out.bypass, Bypass{Source: "branch protection", ActorType: "RepositoryRole", ActorID: adminRole, Note: "administrators are not held to the rules (enforce_admins is off)"})
	}
	return out
}

// adminRole is the repository role id of an administrator, in a ruleset's bypass list.
const adminRole = 5

// rulesetBypass reads one ruleset and returns its bypass actors. why is non-empty when the ruleset cannot be decided
// from (not active, unreadable, or its bypass list not shown to this token).
func rulesetBypass(ctx context.Context, rd Reader, owner, repo string, id int64, src string) ([]Bypass, string, error) {
	st, body, err := rd.GetJSON(ctx, owner, repo, fmt.Sprintf("rulesets/%d", id))
	if err != nil {
		return nil, "", err
	}
	if st != http.StatusOK {
		return nil, fmt.Sprintf("%s could not be read (%d): its bypass list is unknown", src, st), nil
	}
	return parseBypass(body, src)
}

func parseBypass(body []byte, src string) ([]Bypass, string, error) {
	var rs struct {
		Enforcement  string `json:"enforcement"`
		BypassActors *[]struct {
			ActorID    *int64 `json:"actor_id"`
			ActorType  string `json:"actor_type"`
			BypassMode string `json:"bypass_mode"`
		} `json:"bypass_actors"`
	}
	if err := json.Unmarshal(body, &rs); err != nil {
		return nil, "", fmt.Errorf("%s: %w", src, err)
	}
	if rs.Enforcement != "active" {
		return nil, fmt.Sprintf("%s is %q, not active", src, rs.Enforcement), nil
	}
	if rs.BypassActors == nil {
		// The host leaves bypass_actors out for a reader that may not see it (GitHub returns it only to a token with write
		// access to the ruleset). Guessing "none" would be the unsafe answer; the platform does not hold that permission
		// on purpose (review evidence, not bypass lists, decides: docs/admin/security.md), so this is a note.
		return nil, fmt.Sprintf("%s: the host did not show its bypass actors to this read-only token, so who may bypass it is not known (not needed for the verdict)", src), nil
	}
	var out []Bypass
	for _, a := range *rs.BypassActors {
		b := Bypass{Source: src, ActorType: a.ActorType, Mode: a.BypassMode}
		if a.ActorID != nil {
			b.ActorID = *a.ActorID
		}
		switch a.ActorType {
		case "OrganizationAdmin", "EnterpriseOwner":
			b.Note = "organization administrators"
		case "Integration":
			b.Note = "a GitHub App"
		case "RepositoryRole":
			if b.ActorID == adminRole {
				b.Note = "repository administrators"
			} else {
				b.Moot = true
				b.Note = "a role below administrator: anyone holding it can push"
			}
		default: // Team, DeployKey, anything new: who it covers is not shown, so it counts
			b.Moot = true
			b.Note = "who this covers is not shown: assumed to include people with push rights"
		}
		out = append(out, b)
	}
	return out, "", nil
}

// reachable reports whether sha is on branch (an ancestor of its head, or its head).
func reachable(ctx context.Context, rd Reader, owner, repo, branch, sha string) (bool, string, error) {
	st, body, err := rd.GetJSON(ctx, owner, repo, "compare/"+escapeSegs(branch)+"..."+sha+"?per_page=1")
	if err != nil {
		return false, "", err
	}
	switch st {
	case http.StatusOK:
		var c struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &c); err != nil {
			return false, "", fmt.Errorf("compare %s...%s: %w", branch, short(sha), err)
		}
		if c.Status == "behind" || c.Status == "identical" {
			return true, "", nil
		}
		return false, "the host reports it " + c.Status + " of the branch", nil
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return false, "the host does not know that sha as a commit of this repository: it is not a commit (an annotated tag's own object sha is refused: the run must record the commit it points at) or the branch is missing", nil
	}
	return false, "", fmt.Errorf("GET compare %s...%s failed with %d: %s", branch, short(sha), st, hostMessage(body))
}

// ---- tag

type rulesetSummary struct {
	ID          int64  `json:"id"`
	Target      string `json:"target"`
	Enforcement string `json:"enforcement"`
	SourceType  string `json:"source_type"`
}

type tagRuleset struct {
	ID          int64  `json:"id"`
	Target      string `json:"target"`
	Enforcement string `json:"enforcement"`
	Conditions  struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules []struct {
		Type string `json:"type"`
	} `json:"rules"`
}

func (c *Checker) tag(ctx context.Context, rd Reader, owner, repo, tag, sha string, v *Configuration) (*Configuration, error) {
	full := "refs/tags/" + tag
	var sums []rulesetSummary
	for page := 1; page <= maxRulesetPages; page++ {
		st, body, err := rd.GetJSON(ctx, owner, repo, fmt.Sprintf("rulesets?includes_parents=true&per_page=100&page=%d", page))
		if err != nil {
			return nil, err
		}
		if upgradeAnswer(st, body) {
			return v.finish(ConfUnavailable, fmt.Sprintf("%s/%s: the git host offers no rulesets for this repository on its plan (it said so) (informational: review evidence does not need them)", owner, repo)), nil
		}
		if st != http.StatusOK {
			return nil, fmt.Errorf("GET rulesets failed with %d: %s", st, hostMessage(body))
		}
		var got []rulesetSummary
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, fmt.Errorf("GET rulesets: %w", err)
		}
		sums = append(sums, got...)
		if len(got) < 100 {
			break
		}
		if page == maxRulesetPages {
			return v.finish(ConfUnverifiable, fmt.Sprintf("more than %d rulesets: the list is not read further", maxRulesetPages*100)), nil
		}
	}
	have := map[string]bool{}
	var unverifiable []string
	reads := 0
	for _, s := range sums {
		if s.Target != "tag" || s.Enforcement != "active" {
			continue
		}
		if reads++; reads > maxRulesetReads {
			unverifiable = append(unverifiable, fmt.Sprintf("more than %d tag rulesets: not read further", maxRulesetReads))
			break
		}
		src := fmt.Sprintf("ruleset %d (%s)", s.ID, s.SourceType)
		st, body, err := rd.GetJSON(ctx, owner, repo, fmt.Sprintf("rulesets/%d", s.ID))
		if err != nil {
			return nil, err
		}
		if st != http.StatusOK {
			unverifiable = append(unverifiable, fmt.Sprintf("%s could not be read (%d)", src, st))
			continue
		}
		var rs tagRuleset
		if err := json.Unmarshal(body, &rs); err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		if !matchesRef(full, rs.Conditions.RefName.Include, rs.Conditions.RefName.Exclude) {
			continue
		}
		by, why, err := parseBypass(body, src)
		if err != nil {
			return nil, err
		}
		if why != "" {
			unverifiable = append(unverifiable, why)
			continue
		}
		v.Bypass = append(v.Bypass, by...)
		v.ProtectedBy = append(v.ProtectedBy, src)
		for _, r := range rs.Rules {
			have[r.Type] = true
		}
	}
	if len(unverifiable) > 0 {
		return v.finish(ConfUnverifiable, unverifiable...), nil
	}
	for _, need := range []string{"creation", "update", "deletion"} {
		if !have[need] {
			v.Missing = append(v.Missing, "tag ruleset restricting "+need)
		}
	}
	if len(v.Missing) > 0 {
		return v.finish(ConfUnprotected, fmt.Sprintf("tag %s is not protected as required: missing %s (an active tag ruleset matching %s)", tag, strings.Join(v.Missing, ", "), full)), nil
	}
	if moot := mootBypass(v.Bypass); len(moot) > 0 {
		return v.finish(ConfUnprotected, fmt.Sprintf("tag %s is covered by tag rulesets, but %s may bypass them, so anyone with push rights could still create it", tag, strings.Join(moot, ", "))), nil
	}
	// The tagged commit must have come through a protected branch.
	if len(c.ProtectedBranches) == 0 {
		return v.finish(ConfUnprotected, "no protected branch is configured for this platform (cicdRunAdmission.protectedBranches), so a tag's commit cannot be shown to have been reviewed"), nil
	}
	var notes []string
	for _, b := range c.ProtectedBranches {
		ok, why, err := reachable(ctx, rd, owner, repo, b, sha)
		if err != nil {
			return nil, err
		}
		if !ok {
			notes = append(notes, fmt.Sprintf("%s: %s", b, why))
			continue
		}
		bv, err := c.branch(ctx, rd, owner, repo, b, sha, &Configuration{repo: v.repo, ref: "refs/heads/" + b, sha: sha, kind: "branch"})
		if err != nil {
			return nil, err
		}
		if bv.State != ConfProtected {
			notes = append(notes, fmt.Sprintf("%s is %s: %s", b, bv.State, strings.Join(bv.Reasons, "; ")))
			continue
		}
		v.ProtectedBy = append(v.ProtectedBy, "reachable from protected branch "+b)
		v.Bypass = append(v.Bypass, bv.Bypass...)
		return v.finish(ConfProtected, fmt.Sprintf("tag %s is covered by active tag rulesets for creation, update and deletion with no push-rights bypass, and %s is on protected branch %s", tag, short(sha), b)), nil
	}
	return v.finish(ConfUnprotected, fmt.Sprintf("tagged commit %s is not on a protected branch: %s", short(sha), strings.Join(notes, "; "))), nil
}

// matchesRef is a ruleset's ref_name condition: some include pattern matches and no exclude does. ~ALL matches every
// ref; ~DEFAULT_BRANCH never matches a tag. A glob's * stays inside a path segment, ** crosses them.
func matchesRef(ref string, include, exclude []string) bool {
	m := func(pats []string) bool {
		for _, p := range pats {
			if p == "~ALL" || globMatch(p, ref) {
				return true
			}
		}
		return false
	}
	return m(include) && !m(exclude)
}

func globMatch(pat, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch ch := pat[i]; ch {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}
