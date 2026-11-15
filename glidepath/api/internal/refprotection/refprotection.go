// Package refprotection answers one question for GET /v1/apps/{app}/ref-protection (docs/admin/api.md): was every commit
// this run releases REVIEWED, that is, did it arrive on the protected branch through a pull request that someone other
// than its author approved?
//
// It is the second layer behind the admission policy's name check (cicd-run-shape decides on the ref's NAME; anyone with
// push rights can push a v* tag or an unprotected main). The privileged Pipelines call it as their first task (catalog
// verify-ref-protection) and refuse to run unless the verdict is "protected".
//
// The verdict rests on evidence read from GitHub's authenticated facts (pull requests, their commits and reviews), never
// on the host's settings: a ruleset's bypass list is shown only to a token that may administer the repository, which the
// platform does not hold (pipeline trust T3b, docs/admin/security.md "Ref protection"). The rulesets and branch
// protection are still read and reported, best effort, as an informational "configuration" section.
//
//   - Range (range.go): the commits after the last VERIFIED release of this app (a release record whose refProtection
//     says review evidence was verified) up to the commit; with no such release, the commit alone. The baseline is
//     never trusted to skip commits: it must be an ancestor of the commit, and must itself carry review evidence.
//   - Pull request: each commit must belong to a pull request MERGED INTO THE PROTECTED BRANCH of THIS repository (the
//     commit is its merge commit, one of its commits, or one of the commits a rebase merge landed from it).
//   - Review: that pull request needs an approving review, on its final head, not dismissed, from an account that is not
//     a bot, not its opener and not the author or committer of any of its commits.
//
// Verdicts: protected (every commit has the evidence), unreviewed (some commit does not: they are listed), unverifiable
// (GitHub did not answer, the range is longer than MaxRangeCommits, or the range cannot be established). Only protected
// lets a run start. A host that fails to answer at the transport level (or rate limits past the bounded retries) is an
// error, and the caller refuses on it too.
//
// Nothing is cached across requests: every call reads the host again, with the one read-only token the Session minted
// for this request (contents, pull requests and metadata: read). Every list is paged and bounded, and one check makes
// at most maxEvidenceCalls evidence reads.
package refprotection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Reader is one bounded GET of a repo-relative path (github.Session.GetJSON).
type Reader interface {
	GetJSON(ctx context.Context, owner, repo, path string) (int, []byte, error)
}

// Limits a check never exceeds. Beyond one the answer is "unverifiable" with the reason, never "protected".
const (
	MaxRangeCommits  = 250 // commits between the last verified release and this one
	maxEvidenceCalls = 400 // GitHub reads for the evidence of one check (the informational configuration reads are bounded separately)
	maxListPages     = 3   // x100: commit-to-pull-request associations, pull request commits
	maxReviewPages   = 5   // x100 reviews of one pull request
	maxCandidates    = 5   // verified releases considered as the baseline
	maxMissingLines  = 20  // lines of "missing" in the answer (the full list is evidence.unreviewed)
)

// Verdicts.
const (
	Protected    = "protected"
	Unreviewed   = "unreviewed"
	Unverifiable = "unverifiable"
)

// VerifiedMarker is what the guard's summary (and so a release record's refProtection field) contains when review
// evidence was verified. A release record without it is not a baseline.
const VerifiedMarker = "review evidence:"

// Invalid is a request that cannot be answered: a malformed ref or sha, or a repository that is not the app's own.
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

var (
	branchRefRE = regexp.MustCompile(`^refs/heads/[A-Za-z0-9._/-]{1,200}$`)
	tagRefRE    = regexp.MustCompile(`^refs/tags/[A-Za-z0-9._/-]{1,200}$`)
	shaRE       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	upgradeRE   = regexp.MustCompile(`(?i)upgrade to github|upgrade your plan|not available for|available (for|on|to) .*(plan|public)`)
)

// Candidate is a release that claims to have been verified: the commit it released and where the claim came from. It is
// a CLAIM (release records are written by the pipeline's ServiceAccount until pipeline trust T4): the check verifies it.
type Candidate struct {
	Sha     string `json:"sha"`
	Release string `json:"release,omitempty"` // the release record's name
	Env     string `json:"env,omitempty"`
	At      string `json:"at,omitempty"`
}

// Verdict is the answer.
type Verdict struct {
	Verdict string `json:"verdict"`
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	Sha     string `json:"sha"`
	Kind    string `json:"kind"` // branch | tag
	// MainLike: the ref is one a privileged or main-image run may start from (a configured protected branch, or a tag
	// matching the release tag pattern). A build of any other ref (a side branch) writes no main image, so the build
	// task passes it; a privileged Pipeline ignores this and needs "protected".
	MainLike bool `json:"mainLike"`
	// Branch is the protected branch the evidence was read against (a tag's: the first protected branch holding its commit).
	Branch  string   `json:"branch,omitempty"`
	Reasons []string `json:"reasons"`
	// Missing lists what a developer must fix (one line per commit without evidence, at most maxMissingLines).
	Missing  []string  `json:"missing,omitempty"`
	Evidence *Evidence `json:"evidence,omitempty"`
	// Configuration is what the host's rulesets and branch protection say. Informational: it never decides the verdict.
	Configuration *Configuration `json:"configuration,omitempty"`
}

// Evidence is what the verdict rests on, and what the Clearance gate's proposer reads (openers, mergers, approvers, the
// commits).
type Evidence struct {
	Branch string `json:"branch"`
	// Mode: "range" (from the last verified release) or "first" (this commit alone: no usable earlier verified release).
	Mode         string           `json:"mode"`
	From         string           `json:"from,omitempty"` // the baseline commit (exclusive), mode range
	FromRelease  string           `json:"fromRelease,omitempty"`
	To           string           `json:"to"` // inclusive
	Commits      []CommitEvidence `json:"commits"`
	PullRequests []PullEvidence   `json:"pullRequests"`
	// Openers, Mergers and Approvers are the distinct GitHub accounts across every pull request in the range, sorted.
	Openers   []string `json:"openers"`
	Mergers   []string `json:"mergers"`
	Approvers []string `json:"approvers"`
	// Unreviewed names every commit without evidence and why.
	Unreviewed []UnreviewedCommit `json:"unreviewed,omitempty"`
	// Accepted is where an owner-declared baseline will list commits accepted without review, with who accepted them. Always
	// empty in this version: there is no override.
	Accepted []AcceptedCommit `json:"accepted"`
	Notes    []string         `json:"notes,omitempty"`
	// Calls is how many GitHub reads the evidence took.
	Calls int `json:"calls"`
}

// CommitEvidence is one commit of the range.
type CommitEvidence struct {
	Sha         string `json:"sha"`
	PullRequest int    `json:"pullRequest,omitempty"`
	Reviewed    bool   `json:"reviewed"`
	Reason      string `json:"reason,omitempty"`
}

// PullEvidence is one merged pull request that brought commits of the range into the protected branch.
type PullEvidence struct {
	Number      int      `json:"number"`
	URL         string   `json:"url"`
	Opener      string   `json:"opener"`
	Merger      string   `json:"merger"`
	MergedAt    string   `json:"mergedAt"`
	MergeCommit string   `json:"mergeCommit"`
	Head        string   `json:"head"` // the final head: the commit approvals must be on
	Commits     []string `json:"commits"`
	// Authors are the GitHub accounts that authored or committed its commits.
	Authors []string `json:"authors"`
	// Unlinked counts commits whose author or committer GitHub could not link to an account.
	Unlinked  int      `json:"unlinked,omitempty"`
	Approvers []string `json:"approvers"` // accounts whose approval counts
	Reviewed  bool     `json:"reviewed"`
	Missing   string   `json:"missing,omitempty"`
}

// UnreviewedCommit is a commit without review evidence.
type UnreviewedCommit struct {
	Sha    string `json:"sha"`
	Reason string `json:"reason"`
}

// AcceptedCommit is a commit an owner accepted without review (reserved: nothing fills it yet).
type AcceptedCommit struct {
	Sha        string `json:"sha"`
	AcceptedBy string `json:"acceptedBy"`
	Reason     string `json:"reason"`
	At         string `json:"at,omitempty"`
}

// unverifiable is a reason the evidence cannot be established; Check turns it into the verdict.
type unverifiable struct{ why string }

func (u *unverifiable) Error() string { return u.why }

func (v *Verdict) finish(verdict string, reasons ...string) *Verdict {
	v.Verdict = verdict
	v.Reasons = append(v.Reasons, reasons...)
	return v
}

// Check answers for owner/repo at ref and sha. rd is the request's own reader. candidates are the releases that claim
// to have been verified (newest first); they are claims, checked here.
func (c *Checker) Check(ctx context.Context, rd Reader, owner, repo, ref, sha string, candidates []Candidate) (*Verdict, error) {
	v := &Verdict{Repo: owner + "/" + repo, Ref: ref, Sha: sha}
	if !shaRE.MatchString(sha) {
		return nil, Invalid{"sha must be a full 40-character lowercase commit sha"}
	}
	if !cleanRef(ref) {
		return nil, Invalid{"ref must be a full ref without empty, . or .. segments or encoded characters: refs/heads/<branch> or refs/tags/<tag>"}
	}
	if strings.Trim(sha, "0") == "" {
		return nil, Invalid{"sha is the all-zero sha: the run recorded no commit"}
	}
	e := &engine{c: c, rd: rd, owner: owner, repo: repo, prs: map[int]*prState{}}
	switch {
	case branchRefRE.MatchString(ref):
		v.Kind = "branch"
		b := strings.TrimPrefix(ref, "refs/heads/")
		for _, p := range c.ProtectedBranches {
			v.MainLike = v.MainLike || p == b
		}
		if !v.MainLike {
			// A side branch writes no main image and a privileged run never starts from one (admission refuses its name);
			// review evidence is established only against a configured protected branch.
			return v.finish(Unverifiable, fmt.Sprintf("%s is not one of the protected branches (%s): review evidence is established only against those", ref, strings.Join(c.ProtectedBranches, ", "))), nil
		}
		v.Branch = b
	case tagRefRE.MatchString(ref):
		v.Kind = "tag"
		v.MainLike = c.TagRefPattern == nil || c.TagRefPattern.MatchString(ref)
	default:
		return nil, Invalid{"ref must be a full ref: refs/heads/<branch> or refs/tags/<tag>"}
	}
	err := c.decide(ctx, e, v, sha, candidates)
	var u *unverifiable
	if errors.As(err, &u) {
		v.Verdict, v.Reasons, v.Missing = "", nil, nil
		v.finish(Unverifiable, u.why)
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if v.Evidence != nil {
		v.Evidence.Calls = e.calls
	}
	if v.Branch != "" {
		// Informational: what the host's settings say. Never decides, never fails the check.
		cfg, cerr := c.Configuration(ctx, rd, owner, repo, ref, sha)
		switch {
		case cerr == nil:
			v.Configuration = cfg
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			v.Configuration = &Configuration{State: ConfUnverifiable, Reasons: []string{"the host's settings could not be read (informational only): " + cerr.Error()}}
		}
	}
	return v, nil
}

// decide fills v. It returns *unverifiable for "cannot establish", any other error for a failed read.
func (c *Checker) decide(ctx context.Context, e *engine, v *Verdict, sha string, candidates []Candidate) error {
	// The protected branch the commit must be on.
	if v.Kind == "tag" {
		if len(c.ProtectedBranches) == 0 {
			return &unverifiable{"no protected branch is configured for this platform (cicdRunAdmission.protectedBranches), so a tag's commit cannot be shown to have been reviewed"}
		}
		var notes []string
		for _, b := range c.ProtectedBranches {
			ok, why, err := e.onBranch(ctx, b, sha)
			if err != nil {
				return err
			}
			if ok {
				v.Branch = b
				break
			}
			notes = append(notes, b+": "+why)
		}
		if v.Branch == "" {
			v.finish(Unreviewed, fmt.Sprintf("tagged commit %s is not on a protected branch (%s): it did not arrive through a reviewed pull request", short(sha), strings.Join(notes, "; ")))
			v.Missing = []string{short(sha) + ": not on any protected branch"}
			return nil
		}
	} else {
		ok, why, err := e.onBranch(ctx, v.Branch, sha)
		if err != nil {
			return err
		}
		if !ok {
			v.finish(Unreviewed, fmt.Sprintf("commit %s is not on branch %s (%s): it did not arrive through a reviewed pull request into it", short(sha), v.Branch, why))
			v.Missing = []string{short(sha) + ": not on " + v.Branch}
			return nil
		}
	}
	rng, err := e.resolveRange(ctx, v.Branch, sha, candidates)
	if err != nil {
		return err
	}
	ev := &Evidence{Branch: v.Branch, Mode: rng.mode, From: rng.from, FromRelease: rng.fromRelease, To: sha, Accepted: []AcceptedCommit{}, Notes: rng.notes}
	v.Evidence = ev
	idx := map[string]int{}
	for i, rc := range rng.commits {
		idx[rc.sha] = i
	}
	for _, rc := range rng.commits {
		ce, err := e.commitEvidence(ctx, v.Branch, rc.sha, idx, rng.commits)
		if err != nil {
			return err
		}
		ev.Commits = append(ev.Commits, ce)
		if !ce.Reviewed {
			ev.Unreviewed = append(ev.Unreviewed, UnreviewedCommit{Sha: ce.Sha, Reason: ce.Reason})
		}
	}
	e.summarize(ev)
	where := fmt.Sprintf("%s, %s", v.Branch, rangeText(ev))
	if len(ev.Unreviewed) > 0 {
		v.finish(Unreviewed, fmt.Sprintf("review evidence is incomplete: %d of %d commits have no approved pull request (%s)", len(ev.Unreviewed), len(ev.Commits), where))
		for i, u := range ev.Unreviewed {
			if i == maxMissingLines {
				v.Missing = append(v.Missing, fmt.Sprintf("... and %d more commits (evidence.unreviewed lists all)", len(ev.Unreviewed)-maxMissingLines))
				break
			}
			v.Missing = append(v.Missing, fmt.Sprintf("%s: %s", short(u.Sha), u.Reason))
		}
		return nil
	}
	n := len(ev.PullRequests)
	v.finish(Protected, fmt.Sprintf("%s %d commit%s in %d pull request%s, all approved by someone other than their authors on the final head (%s)", VerifiedMarker, len(ev.Commits), plural(len(ev.Commits)), n, plural(n), where))
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func rangeText(ev *Evidence) string {
	if ev.Mode == "range" {
		return fmt.Sprintf("range %s..%s", short(ev.From), short(ev.To))
	}
	return fmt.Sprintf("%s alone: no earlier verified release", short(ev.To))
}

// summarize fills the distinct opener, merger and approver lists from the pull requests that cover the range.
func (e *engine) summarize(ev *Evidence) {
	op, mg, ap := map[string]bool{}, map[string]bool{}, map[string]bool{}
	seen := map[int]bool{}
	for _, ce := range ev.Commits {
		if ce.PullRequest == 0 || seen[ce.PullRequest] {
			continue
		}
		seen[ce.PullRequest] = true
		pe := e.prs[ce.PullRequest].PullEvidence
		ev.PullRequests = append(ev.PullRequests, pe)
		op[pe.Opener], mg[pe.Merger] = true, true
		for _, a := range pe.Approvers {
			ap[a] = true
		}
	}
	sort.Slice(ev.PullRequests, func(i, j int) bool { return ev.PullRequests[i].Number < ev.PullRequests[j].Number })
	ev.Openers, ev.Mergers, ev.Approvers = keysOf(op), keysOf(mg), keysOf(ap)
	if ev.PullRequests == nil {
		ev.PullRequests = []PullEvidence{}
	}
}

func keysOf(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// cleanRef: no empty, "." or ".." segment (a path that climbs, or a name the host would normalise to another ref).
func cleanRef(ref string) bool {
	rest := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	if rest == ref || rest == "" {
		return false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return !strings.ContainsAny(ref, "%\\")
}

func escapeSegs(p string) string {
	parts := strings.Split(p, "/")
	for i, x := range parts {
		parts[i] = url.PathEscape(x)
	}
	return strings.Join(parts, "/")
}

// hostMessage is a GitHub error body's message, or "".
func hostMessage(body []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	json.Unmarshal(body, &m)
	return m.Message
}

func upgradeAnswer(status int, body []byte) bool {
	return (status == http.StatusForbidden || status == http.StatusNotFound) && upgradeRE.MatchString(hostMessage(body))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
