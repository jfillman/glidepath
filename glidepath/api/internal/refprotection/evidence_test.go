package refprotection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/glidepath/api/internal/github"
)

func checker() *Checker {
	return &Checker{ProtectedBranches: []string{"main"}, BotLogins: []string{"pipeline[bot]"}}
}

func needVerdict(t *testing.T, v *Verdict, verdict string, reasonHas ...string) {
	t.Helper()
	if v.Verdict != verdict {
		t.Fatalf("verdict %q, want %q: reasons %v missing %v", v.Verdict, verdict, v.Reasons, v.Missing)
	}
	all := strings.Join(append(append([]string{}, v.Reasons...), v.Missing...), " | ")
	for _, h := range reasonHas {
		if !strings.Contains(all, h) {
			t.Fatalf("answer %q does not mention %q", all, h)
		}
	}
	if len(v.Reasons) == 0 {
		t.Fatal("a verdict carries a reason")
	}
}

// a first release of one squash-merged, approved commit.
func TestFirstReleaseOneReviewedCommit(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(10, sh(2), "alice", "bob")
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil)
	needVerdict(t, v, Protected, "review evidence: 1 commit in 1 pull request", "approved by someone other than their authors on the final head", short(sh(2))+" alone")
	ev := v.Evidence
	if ev.Mode != "first" || len(ev.Commits) != 1 || !ev.Commits[0].Reviewed || ev.Commits[0].PullRequest != 10 {
		t.Fatalf("%+v", ev)
	}
	if j(ev.Openers) != `["alice"]` || j(ev.Mergers) != `["alice"]` || j(ev.Approvers) != `["bob"]` {
		t.Fatalf("openers %v mergers %v approvers %v", ev.Openers, ev.Mergers, ev.Approvers)
	}
	if len(ev.Accepted) != 0 || ev.Accepted == nil {
		t.Fatalf("the baseline list is present and empty: %#v", ev.Accepted)
	}
	// the older commit sha(1) was never looked at: only the commit itself is in a first release's range
	if w.count("commits/"+sh(1)) != 0 {
		t.Fatal("a first release checks the commit alone")
	}
	if v.Configuration == nil {
		t.Fatal("the informational configuration is part of the answer")
	}
}

// merge commit, squash and rebase merge in one range, from a verified release.
func TestCleanRangeMergeSquashRebase(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	// the verified release: a squash PR #1
	w.squash(1, sh(2), "alice", "bob")
	// PR #2: a merge commit. Its own commits a2, b2 land on main, then the merge commit M2 (two parents).
	a2, b2 := w.prCommit(sh(20), "carol"), w.prCommit(sh(21), "carol")
	w.land(&wCommit{sha: a2.sha, author: "carol", committer: "carol", parents: []string{sh(2)}}, &wCommit{sha: b2.sha, author: "carol", committer: "carol"})
	w.land(&wCommit{sha: sh(22), author: "dave", committer: "web-flow", cname: "GitHub", aname: "dave", parents: []string{b2.sha, sh(2)}})
	p2 := w.pr(&wPR{n: 2, opener: "carol", merger: "dave", mergeSha: sh(22), commits: []string{a2.sha, b2.sha}})
	p2.reviews = approved("erin", p2.head)
	w.associate(a2.sha, 2)
	w.associate(b2.sha, 2)
	w.associate(sh(22), 2)
	// PR #3: a squash
	w.squash(3, sh(30), "frank", "bob")
	// PR #4: a rebase merge. Its commits p1, p2 land as r1, r2 (new shas, linear); the merge_commit_sha is the tip.
	o1, o2 := w.prCommit(sh(40), "gina"), w.prCommit(sh(41), "gina")
	w.land(&wCommit{sha: sh(42), author: "gina", committer: "gina"}, &wCommit{sha: sh(43), author: "gina", committer: "gina"})
	p4 := w.pr(&wPR{n: 4, opener: "gina", merger: "gina", mergeSha: sh(43), commits: []string{o1.sha, o2.sha}})
	p4.reviews = approved("bob", p4.head)
	w.associate(sh(42), 4)
	w.associate(sh(43), 4)

	cands := []Candidate{{Sha: sh(2), Release: "release-tracking-r1", Env: "prod"}}
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(43), cands)
	needVerdict(t, v, Protected, "6 commits in 3 pull requests", "range "+short(sh(2))+".."+short(sh(43)))
	ev := v.Evidence
	if ev.Mode != "range" || ev.From != sh(2) || ev.FromRelease != "gitops-app#900" || len(ev.Commits) != 6 {
		t.Fatalf("%+v", ev)
	}
	if j(ev.Openers) != `["carol","frank","gina"]` || j(ev.Mergers) != `["dave","frank","gina"]` || j(ev.Approvers) != `["bob","erin"]` {
		t.Fatalf("openers %v mergers %v approvers %v", ev.Openers, ev.Mergers, ev.Approvers)
	}
	if len(ev.PullRequests) != 3 || ev.PullRequests[0].Number != 2 || len(ev.PullRequests[0].Commits) != 2 {
		t.Fatalf("%+v", ev.PullRequests)
	}
	// every commit of the range names its pull request
	want := map[string]int{a2.sha: 2, b2.sha: 2, sh(22): 2, sh(30): 3, sh(42): 4, sh(43): 4}
	for _, c := range ev.Commits {
		if n, ok := want[c.Sha]; ok && c.PullRequest != n {
			t.Fatalf("%s is in #%d, want #%d", c.Sha, c.PullRequest, n)
		}
	}
}

// a commit nobody opened a pull request for: a direct push, a force push, an administrator's bypass.
func TestDirectPushIsUnreviewedAndNamed(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	w.land(&wCommit{sha: sh(3), author: "mallory", committer: "mallory"})
	w.squash(4, sh(4), "alice", "bob")
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(4), []Candidate{{Sha: sh(2), Release: "r"}})
	needVerdict(t, v, Unreviewed, "1 of 2 commits", short(sh(3))+": no pull request: pushed to main directly")
	if len(v.Evidence.Unreviewed) != 1 || v.Evidence.Unreviewed[0].Sha != sh(3) {
		t.Fatalf("%+v", v.Evidence.Unreviewed)
	}
	for _, m := range v.Missing {
		if strings.Contains(m, short(sh(4))+":") {
			t.Fatalf("a reviewed commit is not listed as missing: %v", v.Missing)
		}
	}
}

// reviewed on a side branch, then main was fast-forwarded to it: the pull request merged into the side branch, not main.
func TestPullRequestMergedIntoSideBranchThenFastForwarded(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.land(&wCommit{sha: sh(2), author: "carol", committer: "carol"})
	w.pr(&wPR{n: 7, opener: "carol", base: "release/next", mergeSha: sh(2), commits: []string{sh(2)}}).reviews = approved("bob", sh(2))
	w.associate(sh(2), 7)
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil)
	needVerdict(t, v, Unreviewed, "#7 (merged into release/next)")
	// the same pull request merged into ANOTHER REPOSITORY's main (a fork) does not count either
	w2 := newWorld(t)
	w2.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w2.land(&wCommit{sha: sh(2), author: "carol", committer: "carol"})
	w2.pr(&wPR{n: 8, opener: "carol", baseRepo: "carol/app", mergeSha: sh(2), commits: []string{sh(2)}}).reviews = approved("bob", sh(2))
	w2.associate(sh(2), 8)
	needVerdict(t, mustCheck(t, checker(), w2, "refs/heads/main", sh(2), nil), Unreviewed, "#8 (merged into carol/app:main)")
	// an association whose pull request, read in full, was not merged into main is refused as well
	w3 := newWorld(t)
	w3.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w3.land(&wCommit{sha: sh(2), author: "carol", committer: "carol"})
	w3.pr(&wPR{n: 9, opener: "carol", mergeSha: sh(2), commits: []string{sh(2)}}).reviews = approved("bob", sh(2))
	w3.associate(sh(2), 9)
	w3.override["pulls/9"] = resp{200, strings.Replace(w3.prs[9].json(), `"merged_at":"2026-11-10T10:00:00Z"`, `"merged_at":null`, 1)}
	needVerdict(t, mustCheck(t, checker(), w3, "refs/heads/main", sh(2), nil), Unreviewed, "#9 (not merged into main)")
}

// a pull request that is open or closed unmerged does not cover a commit even when GitHub associates it.
func TestUnmergedPullRequestDoesNotCount(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.land(&wCommit{sha: sh(2), author: "carol", committer: "carol"})
	p := w.pr(&wPR{n: 5, opener: "carol", unmerged: true, mergeSha: sh(2), commits: []string{sh(2)}})
	p.reviews = approved("bob", sh(2))
	w.associate(sh(2), 5)
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil), Unreviewed, "#5 (not merged)")
}

func reviewCase(t *testing.T, mutate func(p *wPR, w *world)) *Verdict {
	t.Helper()
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	p := w.squash(10, sh(2), "alice", "bob")
	mutate(p, w)
	return mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil)
}

func TestApprovalFollowedByAPushIsStale(t *testing.T) {
	v := reviewCase(t, func(p *wPR, w *world) {
		// bob approved the first commit; carol pushed another afterwards; nobody approved the new head
		first, second := p.commits[0], sh(900)
		w.prCommit(second, "alice")
		p.commits = []string{first, second}
		p.head = second
		p.reviews = approved("bob", first)
	})
	needVerdict(t, v, Unreviewed, "approved only on an older head (bob on "+short(sh(100010))+")", "pushed to after the approval")
}

func TestApprovalOnAnOlderHeadIsIgnoredWhenAnotherApprovedTheFinalHead(t *testing.T) {
	v := reviewCase(t, func(p *wPR, w *world) {
		first, second := p.commits[0], sh(900)
		w.prCommit(second, "alice")
		p.commits = []string{first, second}
		p.head = second
		p.reviews = []wReview{{user: "bob", state: "APPROVED", commit: first}, {user: "erin", state: "APPROVED", commit: second}}
	})
	needVerdict(t, v, Protected)
	if j(v.Evidence.Approvers) != `["erin"]` {
		t.Fatalf("only the approval on the final head counts: %v", v.Evidence.Approvers)
	}
}

func TestApprovalByTheOpenersSecondAccountListedAsCommitAuthor(t *testing.T) {
	v := reviewCase(t, func(p *wPR, w *world) {
		// alice opened it; the commits were authored by "alice-alt" and "alice-alt" approved it
		w.commits[p.commits[0]].author = "alice-alt"
		p.reviews = approved("alice-alt", p.head)
	})
	needVerdict(t, v, Unreviewed, "approved only by alice-alt, who is the opener or an author or committer")
}

func TestApprovalByTheOpenerOrACommitterDoesNotCount(t *testing.T) {
	v := reviewCase(t, func(p *wPR, w *world) { p.reviews = approved("alice", p.head) })
	needVerdict(t, v, Unreviewed, "approved only by alice")
	v = reviewCase(t, func(p *wPR, w *world) {
		w.commits[p.commits[0]].committer = "zed" // a committer who is not the author
		p.reviews = approved("zed", p.head)
	})
	needVerdict(t, v, Unreviewed, "approved only by zed")
	// login comparison ignores case
	v = reviewCase(t, func(p *wPR, w *world) { p.reviews = approved("ALICE", p.head) })
	needVerdict(t, v, Unreviewed, "approved only by ALICE")
}

func TestWebFlowCommitterSaysNothingAboutWhoWroteIt(t *testing.T) {
	v := reviewCase(t, func(p *wPR, w *world) {
		w.commits[p.commits[0]].committer = "web-flow"
		w.commits[p.commits[0]].cname = "GitHub"
		p.reviews = approved("bob", p.head)
	})
	needVerdict(t, v, Protected)
	if j(v.Evidence.PullRequests[0].Authors) != `["alice"]` {
		t.Fatalf("%v", v.Evidence.PullRequests[0].Authors)
	}
}

func TestDismissedAndWithdrawnApprovals(t *testing.T) {
	v := reviewCase(t, func(p *wPR, w *world) { p.reviews = []wReview{{user: "bob", state: "DISMISSED", commit: p.head}} })
	needVerdict(t, v, Unreviewed, "the approval of bob was dismissed")
	// approved, then changes requested by the same reviewer: not an approval any more
	v = reviewCase(t, func(p *wPR, w *world) {
		p.reviews = []wReview{{user: "bob", state: "APPROVED", commit: p.head}, {user: "bob", state: "CHANGES_REQUESTED", commit: p.head}}
	})
	needVerdict(t, v, Unreviewed, "changes requested by bob")
	// a later plain comment does not withdraw an approval
	v = reviewCase(t, func(p *wPR, w *world) {
		p.reviews = []wReview{{user: "bob", state: "APPROVED", commit: p.head}, {user: "bob", state: "COMMENTED", commit: p.head}, {user: "pat", state: "PENDING", commit: p.head}}
	})
	needVerdict(t, v, Protected)
	// no review at all
	v = reviewCase(t, func(p *wPR, w *world) { p.reviews = nil })
	needVerdict(t, v, Unreviewed, "no approving review")
}

func TestBotApprovalsDoNotCount(t *testing.T) {
	for name, rv := range map[string]wReview{
		"type Bot":   {user: "helper", typ: "Bot", state: "APPROVED"},
		"bot suffix": {user: "reviewer[bot]", typ: "User", state: "APPROVED"},
		"configured": {user: "Pipeline-App", typ: "User", state: "APPROVED"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
			p := w.squash(10, sh(2), "alice", "bob")
			rv.commit = p.head
			p.reviews = []wReview{rv}
			c := checker()
			c.BotLogins = []string{"pipeline-app"}
			v := mustCheck(t, c, w, "refs/heads/main", sh(2), nil)
			needVerdict(t, v, Unreviewed, "approved only by a bot ("+rv.user+")")
		})
	}
	// ... but a person's approval beside a bot's does
	v := reviewCase(t, func(p *wPR, w *world) {
		p.reviews = []wReview{{user: "helper[bot]", typ: "Bot", state: "APPROVED", commit: p.head}, {user: "bob", state: "APPROVED", commit: p.head}}
	})
	needVerdict(t, v, Protected)
}

// commits whose author GitHub could not link to an account: the approver must not be the merger either.
func TestUnlinkedAuthorExcludesTheMergerAsApprover(t *testing.T) {
	setup := func(approver string) *Verdict {
		return reviewCase(t, func(p *wPR, w *world) {
			p.merger = "bob"
			c := w.commits[p.commits[0]]
			c.author, c.committer, c.aname, c.cname = "", "", "Some Body", "Some Body" // unlinked
			p.reviews = approved(approver, p.head)
		})
	}
	needVerdict(t, setup("bob"), Unreviewed, "approved only by bob", "or the merger (some commits' authors are unknown to GitHub)")
	v := setup("erin")
	needVerdict(t, v, Protected)
	if v.Evidence.PullRequests[0].Unlinked != 1 {
		t.Fatalf("the unlinked commit is counted: %+v", v.Evidence.PullRequests[0])
	}
	// with every author linked, the person who approves and then merges is fine (self-merge after approval)
	v = reviewCase(t, func(p *wPR, w *world) { p.merger = "bob"; p.reviews = approved("bob", p.head) })
	needVerdict(t, v, Protected)
	if j(v.Evidence.Mergers) != `["bob"]` || j(v.Evidence.Approvers) != `["bob"]` {
		t.Fatalf("%v %v", v.Evidence.Mergers, v.Evidence.Approvers)
	}
}

func TestPullRequestFromAFork(t *testing.T) {
	v := reviewCase(t, func(p *wPR, w *world) { p.headRepo = "alice/app" })
	needVerdict(t, v, Protected)
}

func TestCommitCountMismatchAndMissingHeadAreRefused(t *testing.T) {
	v := reviewCase(t, func(p *wPR, w *world) { p.count = 5 })
	needVerdict(t, v, Unreviewed, "GitHub lists 1 commits but the pull request has 5")
	v = reviewCase(t, func(p *wPR, w *world) { p.head = sh(999) })
	needVerdict(t, v, Unreviewed, "its final head "+short(sh(999))+" is not among its commits")
}

// GitHub associates a commit with a pull request that does not contain it.
func TestAssociatedButNotContainedCommit(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(10, sh(2), "alice", "bob")
	// a direct push after the squash, associated (wrongly or by a stray ref) with #10
	w.land(&wCommit{sha: sh(3), author: "mallory", committer: "mallory"})
	w.associate(sh(3), 10)
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(3), []Candidate{{Sha: sh(2), Release: "r"}})
	needVerdict(t, v, Unreviewed, short(sh(3))+": GitHub associates it with #10, which does not contain it")
}

// a rebase merge lands N commits; an extra commit just before them is not one of them.
func TestRebaseRunIsBoundedByTheCommitCount(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	w.land(&wCommit{sha: sh(50), author: "mallory", committer: "mallory"}) // direct push
	o1 := w.prCommit(sh(40), "gina")
	w.land(&wCommit{sha: sh(42), author: "gina", committer: "gina"})
	p := w.pr(&wPR{n: 4, opener: "gina", mergeSha: sh(42), commits: []string{o1.sha}}) // one commit: only the tip landed
	p.reviews = approved("bob", p.head)
	w.associate(sh(42), 4)
	w.associate(sh(50), 4) // a stray association
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(42), []Candidate{{Sha: sh(2), Release: "r"}})
	needVerdict(t, v, Unreviewed, short(sh(50))+": GitHub associates it with #4")
	if len(v.Evidence.Unreviewed) != 1 {
		t.Fatalf("only the stray commit: %+v", v.Evidence.Unreviewed)
	}
}

// ---- the baseline: where the range starts

func rangeWorld(t *testing.T) *world {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	w.squash(2, sh(3), "alice", "bob")
	w.squash(3, sh(4), "alice", "bob")
	return w
}

func TestNearestVerifiedReleaseStartsTheRange(t *testing.T) {
	w := rangeWorld(t)
	cands := []Candidate{{Sha: sh(3), Release: "new"}, {Sha: sh(2), Release: "old"}} // newest first
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(4), cands)
	needVerdict(t, v, Protected, "1 commit in 1 pull request")
	if v.Evidence.From != sh(3) || v.Evidence.FromRelease != "gitops-app#901" {
		t.Fatalf("%+v", v.Evidence)
	}
}

// A forged "last release" that is not an ancestor never shrinks the range, and never passes it.
func TestForgedLastReleaseNotAnAncestorIsRefused(t *testing.T) {
	w := rangeWorld(t)
	// a commit that exists but is on no branch of ours
	w.prCommit(sh(77), "mallory")
	w.override["compare/"+sh(77)+"..."+sh(4)] = resp{200, `{"status":"diverged","total_commits":3,"ahead_by":3,"commits":[]}`}
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(4), []Candidate{{Sha: sh(77), Release: "forged"}})
	needVerdict(t, v, Unverifiable, "cannot establish which commits this release carries", "gitops history shows earlier releases", "is diverged of")
	if v.Evidence != nil && len(v.Evidence.Commits) > 0 {
		t.Fatal("no evidence is claimed for a range that could not be established")
	}
	// a sha GitHub does not know
	v = mustCheck(t, checker(), w, "refs/heads/main", sh(4), []Candidate{{Sha: sh(88), Release: "forged"}})
	needVerdict(t, v, Unverifiable, "is not a commit GitHub knows")
}

// A forged release naming an ancestor that was pushed directly: the baseline must itself have review evidence.
func TestForgedLastReleaseThatIsItselfUnreviewedIsRefused(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(2, sh(3), "alice", "bob")
	w.land(&wCommit{sha: sh(4), author: "mallory", committer: "mallory"}) // direct push
	w.squash(5, sh(5), "alice", "bob")
	forged := Candidate{Sha: sh(4), Release: "forged"}
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(5), []Candidate{forged})
	needVerdict(t, v, Unverifiable, "itself has no review evidence", "no pull request")
	// ... and with a second, honest candidate behind it, the honest one is used and the direct push is in the range
	v = mustCheck(t, checker(), w, "refs/heads/main", sh(5), []Candidate{forged, {Sha: sh(3), Release: "honest"}})
	needVerdict(t, v, Unreviewed, short(sh(4))+": no pull request")
	if v.Evidence.From != sh(3) || !strings.Contains(strings.Join(v.Evidence.Notes, " "), "itself has no review evidence") {
		t.Fatalf("%+v", v.Evidence)
	}
}

// A release of a commit at or before a verified release (a rollback, a repeat): the commit alone, still checked.
func TestCommitAtOrBeforeAVerifiedReleaseIsCheckedAlone(t *testing.T) {
	w := rangeWorld(t)
	for _, c := range []Candidate{{Sha: sh(4), Release: "same"}, {Sha: sh(4), Release: "later"}} {
		v := mustCheck(t, checker(), w, "refs/heads/main", sh(3), []Candidate{c})
		needVerdict(t, v, Protected, "alone")
		if v.Evidence.Mode != "first" || len(v.Evidence.Commits) != 1 {
			t.Fatalf("%+v", v.Evidence)
		}
	}
	// ... and a commit that is not reviewed is still refused
	w.land(&wCommit{sha: sh(5), author: "mallory", committer: "mallory"})
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(5), []Candidate{{Sha: sh(5), Release: "same"}}), Unreviewed, short(sh(5)))
}

// ---- first release and refs

func TestFirstReleaseWithoutAnyVerifiedRelease(t *testing.T) {
	w := rangeWorld(t)
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(4), nil)
	needVerdict(t, v, Protected, "1 commit in 1 pull request")
	if v.Evidence.Mode != "first" || v.Evidence.From != "" || !strings.Contains(strings.Join(v.Evidence.Notes, " "), "first release") {
		t.Fatalf("%+v", v.Evidence)
	}
	// the commit alone: older commits' pull requests were never read
	if w.count("pulls/2") != 0 || w.count("pulls/1") != 0 {
		t.Fatalf("%v", w.calls)
	}
}

func TestSideBranchIsNotEvaluated(t *testing.T) {
	w := newWorld(t)
	v := mustCheck(t, checker(), w, "refs/heads/feature/x", sh(4), nil)
	needVerdict(t, v, Unverifiable, "not one of the protected branches")
	if v.MainLike || len(w.calls) != 0 {
		t.Fatalf("a side branch reads nothing: %v", w.calls)
	}
}

func TestCommitNotOnTheProtectedBranch(t *testing.T) {
	w := rangeWorld(t)
	w.prCommit(sh(77), "mallory")
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(77), nil)
	needVerdict(t, v, Unreviewed, "is not on branch main", "diverged")
}

func TestTagResolvesToTheProtectedBranchHoldingItsCommit(t *testing.T) {
	w := rangeWorld(t)
	c := &Checker{ProtectedBranches: []string{"main"}, TagRefPattern: regexp.MustCompile(`^refs/tags/v[0-9.]+$`)}
	v := mustCheck(t, c, w, "refs/tags/v1.0.0", sh(4), nil)
	needVerdict(t, v, Protected)
	if v.Kind != "tag" || !v.MainLike || v.Branch != "main" {
		t.Fatalf("%+v", v)
	}
	w.prCommit(sh(77), "mallory")
	v = mustCheck(t, c, w, "refs/tags/v1.0.1", sh(77), nil)
	needVerdict(t, v, Unreviewed, "not on a protected branch")
	if v, err := (&Checker{}).Check(context.Background(), w, "o", "app", "refs/tags/v1.0.0", sh(4), nil); err != nil || v.Verdict != Unverifiable {
		t.Fatalf("no protected branch configured: %v %+v", err, v)
	}
	// MainLike follows the tag pattern
	if mustCheck(t, c, w, "refs/tags/nightly", sh(4), nil).MainLike {
		t.Fatal("nightly is not a release tag")
	}
}

// ---- bounds, pagination, failures

func TestMissingLinesAreBounded(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	for i := 0; i < 30; i++ {
		w.land(&wCommit{sha: sh(1000 + i), author: "mallory", committer: "mallory"})
	}
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(1029), []Candidate{{Sha: sh(2), Release: "r"}})
	needVerdict(t, v, Unreviewed, "30 of 30 commits")
	if len(v.Missing) != maxMissingLines+1 || !strings.Contains(v.Missing[maxMissingLines], "and 10 more commits") {
		t.Fatalf("%d lines: %v", len(v.Missing), v.Missing[len(v.Missing)-1])
	}
	if len(v.Evidence.Unreviewed) != 30 {
		t.Fatal("the full list stays in the evidence")
	}
}

func longHistory(w *world, n int) {
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	for i := 0; i < n; i++ {
		w.land(&wCommit{sha: sh(5000 + i), author: "mallory", committer: "mallory"})
	}
}

func TestRangePaginationBoundaries(t *testing.T) {
	for _, n := range []int{99, 100, 101, 250} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			w := newWorld(t)
			longHistory(w, n)
			v := mustCheck(t, checker(), w, "refs/heads/main", sh(5000+n-1), []Candidate{{Sha: sh(2), Release: "r"}})
			needVerdict(t, v, Unreviewed, fmt.Sprintf("%d of %d commits", n, n))
			if len(v.Evidence.Commits) != n {
				t.Fatalf("read %d of %d commits", len(v.Evidence.Commits), n)
			}
		})
	}
	// 251 commits: more than one check verifies
	w := newWorld(t)
	longHistory(w, 251)
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(5000+250), []Candidate{{Sha: sh(2), Release: "r"}})
	needVerdict(t, v, Unverifiable, "more than 250 commits")
	if w.count("commits/") != 2 { // only the baseline's own commit was looked up (its pulls and itself); no commit of the range was
		t.Fatalf("a range that is too long reads no commit of it: %d", w.count("commits/"))
	}
}

func TestEvidenceCallBudget(t *testing.T) {
	// 250 commits, each in its own reviewed pull request: ~4 reads each, far past the budget
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	for i := 0; i < 250; i++ {
		w.squash(100+i, sh(6000+i), "alice", "bob")
	}
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(6249), []Candidate{{Sha: sh(2), Release: "r"}})
	needVerdict(t, v, Unverifiable, fmt.Sprintf("more than %d GitHub reads", maxEvidenceCalls))
	if len(w.calls) > maxEvidenceCalls+60 { // the evidence stops at the budget; the informational reads are few
		t.Fatalf("%d calls", len(w.calls))
	}
}

func TestPullListAndReviewPaginationBounds(t *testing.T) {
	// 300 associated pull requests: the list is not read in full, so nothing is concluded from it
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.land(&wCommit{sha: sh(2), author: "carol", committer: "carol"})
	for i := 0; i < 300; i++ {
		w.pr(&wPR{n: 100 + i, opener: "carol", base: "other", mergeSha: sh(2), commits: []string{sh(2)}})
		w.associate(sh(2), 100+i)
	}
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil), Unverifiable, "has more than 300 entries")
	// 100 pull requests: exactly one full page, then an empty one, all read
	w = newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.land(&wCommit{sha: sh(2), author: "carol", committer: "carol"})
	for i := 0; i < 100; i++ {
		w.pr(&wPR{n: 100 + i, opener: "carol", base: "other", mergeSha: sh(2), commits: []string{sh(2)}})
		w.associate(sh(2), 100+i)
	}
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil), Unreviewed, "no pull request merged into main contains it")
	if w.count("commits/"+sh(2)+"/pulls") != 2 {
		t.Fatalf("a full first page asks for the second: %v", w.calls)
	}
	// 500 reviews: all five pages read, an approval on page 5 counts; 501st page is not asked for
	w = newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	p := w.squash(10, sh(2), "alice", "bob")
	p.reviews = nil
	for i := 0; i < 499; i++ {
		p.reviews = append(p.reviews, wReview{user: fmt.Sprintf("c%d", i), state: "COMMENTED", commit: p.head})
	}
	p.reviews = append(p.reviews, wReview{user: "bob", state: "APPROVED", commit: p.head})
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil), Unverifiable, "GET pulls/10/reviews has more than 500 entries")
	p.reviews = p.reviews[:len(p.reviews)-2] // 498 entries, approval gone: 5 pages, last one short
	p.reviews = append(p.reviews, wReview{user: "bob", state: "APPROVED", commit: p.head})
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil), Protected)
}

func TestHostFailuresFailClosed(t *testing.T) {
	cands := []Candidate{{Sha: sh(3), Release: "r"}}
	cases := map[string]struct {
		path string
		r    resp
		err  error
		want string // "" = an error is returned
	}{
		"pull 500":           {path: "pulls/3", r: resp{500, `{"message":"boom"}`}, want: Unverifiable + "|GitHub answered 500 to GET pulls/3"},
		"pull 404":           {path: "pulls/3", r: resp{404, `{"message":"Not Found"}`}, want: Unverifiable + "|GitHub answered 404"},
		"associations 404":   {path: "commits/" + sh(4) + "/pulls", r: resp{404, `{"message":"No commit found"}`}, want: Unverifiable + "|GitHub answered 404 to GET commits/"},
		"associations 403":   {path: "commits/" + sh(4) + "/pulls", r: resp{403, `{"message":"Resource not accessible by integration"}`}, want: Unverifiable + "|Resource not accessible"},
		"commits garbage":    {path: "pulls/3/commits", r: resp{200, `{"not":"a list"}`}, want: Unverifiable + "|GET pulls/3/commits could not be read"},
		"reviews 500":        {path: "pulls/3/reviews", r: resp{500, ``}, want: Unverifiable + "|GitHub answered 500 to GET pulls/3/reviews"},
		"compare 500":        {path: "compare/" + sh(3) + "..." + sh(4), r: resp{500, ``}, want: Unverifiable + "|GitHub answered 500"},
		"branch compare 500": {path: "compare/main..." + sh(4), r: resp{500, ``}, want: Unverifiable + "|GitHub answered 500 to the compare of main"},
		"transport":          {path: "pulls/3", err: errors.New("connection reset"), want: ""},
		"rate limited":       {path: "pulls/3", err: github.RateLimited{Wait: 0}, want: ""},
		"rate limited list":  {path: "commits/" + sh(4) + "/pulls", err: github.RateLimited{Wait: 0}, want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := rangeWorld(t)
			if tc.err != nil {
				w.errs[tc.path] = tc.err
			} else {
				w.override[tc.path] = tc.r
			}
			w.setReleases(cands)
			v, err := checker().Check(context.Background(), w, "o", "app", "refs/heads/main", sh(4), withHistory(w))
			if tc.want == "" {
				if err == nil {
					t.Fatalf("a failed read is an error, got %+v", v)
				}
				var rl github.RateLimited
				if name == "rate limited" && !errors.As(err, &rl) {
					t.Fatalf("%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			verdict, has, _ := strings.Cut(tc.want, "|")
			needVerdict(t, v, verdict, has)
		})
	}
}

func TestNothingIsCachedBetweenChecks(t *testing.T) {
	w := rangeWorld(t)
	c := checker()
	v1 := mustCheck(t, c, w, "refs/heads/main", sh(4), nil)
	n := len(w.calls)
	// the pull request is dismissed between the two checks: the second check sees it
	w.prs[3].reviews = []wReview{{user: "bob", state: "DISMISSED", commit: w.prs[3].head}}
	v2 := mustCheck(t, c, w, "refs/heads/main", sh(4), nil)
	if v1.Verdict != Protected || v2.Verdict != Unreviewed || len(w.calls) < 2*n-5 {
		t.Fatalf("%s then %s, %d then %d calls", v1.Verdict, v2.Verdict, n, len(w.calls))
	}
}

func TestEvidenceCallsAreReported(t *testing.T) {
	w := rangeWorld(t)
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(4), nil)
	// ref compare + gitops closed pull requests + commit's pulls + pull + its commits + its reviews + the approver's permission
	// + the merge commit itself (it is not in a range list) = 8 reads for a lone squashed commit
	if v.Evidence.Calls != 8 {
		t.Fatalf("calls %d: %v", v.Evidence.Calls, w.calls)
	}
}

// The informational configuration never decides: a failure to read it leaves the verdict alone.
func TestConfigurationNeverDecides(t *testing.T) {
	w := rangeWorld(t)
	w.override["rules/branches/main"] = resp{500, `{"message":"boom"}`}
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(4), nil)
	needVerdict(t, v, Protected)
	if v.Configuration == nil || v.Configuration.State != ConfUnverifiable || !strings.Contains(strings.Join(v.Configuration.Reasons, " "), "informational only") {
		t.Fatalf("%+v", v.Configuration)
	}
	// and an unprotected configuration does not turn a reviewed range into a refusal
	w2 := rangeWorld(t)
	v = mustCheck(t, checker(), w2, "refs/heads/main", sh(4), nil)
	needVerdict(t, v, Protected)
	if v.Configuration.State != ConfUnprotected {
		t.Fatalf("a ruleset-free repository reads as unprotected: %+v", v.Configuration)
	}
}

// ---- recorded GitHub shapes (testdata/real_*.json, from a public repository: pulls, commits, reviews, associations, compare)

type recordedHost struct {
	t *testing.T
	// permission403: the permission read answers GitHub's recorded 403 (the token may not read it) instead of "write"
	permission403 bool
	// dropState removes the recorded reviews of that state (to show the verdict rests on them)
	dropState string
}

func (r recordedHost) GetJSON(_ context.Context, _, _, path string) (int, []byte, error) {
	read := func(f string) (int, []byte, error) {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			r.t.Fatal(err)
		}
		return 200, b, nil
	}
	p, _, _ := strings.Cut(path, "?")
	switch {
	case strings.HasPrefix(p, "compare/trunk..."):
		return 200, []byte(`{"status":"behind"}`), nil
	case strings.HasPrefix(p, "commits/") && strings.HasSuffix(p, "/pulls"):
		return read("real_commit_pulls.json")
	case p == "pulls/15041":
		return read("real_pull.json")
	case p == "pulls/15041/commits":
		return read("real_pull_commits.json")
	case p == "pulls/15041/reviews":
		st, b, err := read("real_pull_reviews.json")
		if r.dropState == "" {
			return st, b, err
		}
		var all []map[string]any
		if err := json.Unmarshal(b, &all); err != nil {
			r.t.Fatal(err)
		}
		kept := []map[string]any{}
		for _, rv := range all {
			if rv["state"] != r.dropState {
				kept = append(kept, rv)
			}
		}
		out, _ := json.Marshal(kept)
		return 200, out, nil
	case strings.HasPrefix(p, "commits/") && !strings.HasSuffix(p, "/pulls"):
		return read("real_commit.json")
	case strings.HasPrefix(p, "collaborators/") && strings.HasSuffix(p, "/permission"):
		if r.permission403 {
			_, b, _ := read("real_permission_403.json")
			return 403, b, nil
		}
		return 200, []byte(`{"permission":"write","role_name":"write"}`), nil
	case strings.HasPrefix(p, "rules/") || strings.HasPrefix(p, "rulesets"):
		return 200, []byte(`[]`), nil
	case strings.HasPrefix(p, "branches/"):
		return 404, []byte(notProt404), nil
	}
	r.t.Fatalf("unexpected GET %s", path)
	return 0, nil, nil
}

func TestRecordedShapes(t *testing.T) {
	c := &Checker{ProtectedBranches: []string{"trunk"}}
	v, err := c.Check(context.Background(), recordedHost{t: t}, "cli", "cli", "refs/heads/trunk", "e1110093f516b3e595cae3bef8a20c7bf39c2d14", nil)
	if err != nil {
		t.Fatal(err)
	}
	needVerdict(t, v, Protected, "1 commit in 1 pull request")
	pe := v.Evidence.PullRequests[0]
	// the recorded merge commit is a real GitHub-made merge commit: two parents, committer web-flow, verified signature
	var rc ghCommit
	b0, _ := os.ReadFile("testdata/real_commit.json")
	if err := json.Unmarshal(b0, &rc); err != nil || rc.Committer.Login != "web-flow" || !rc.Commit.Verification.Verified || len(rc.Parents) != 2 {
		t.Fatalf("the recorded shape: %v %+v", err, rc)
	}
	if pe.Number != 15041 || pe.Opener != "BagToad" || pe.Merger != "BagToad" || j(pe.Approvers) != `["tidy-dev"]` {
		t.Fatalf("%+v", pe)
	}
	// the Copilot reviewer (type Bot, [bot] suffix) only commented: without the human's approval nothing counts
	b, _ := os.ReadFile("testdata/real_pull_reviews.json")
	if !strings.Contains(string(b), "copilot-pull-request-reviewer[bot]") {
		t.Fatal("the recorded reviews are the real ones")
	}
	v, err = c.Check(context.Background(), recordedHost{t: t, dropState: "APPROVED"}, "cli", "cli", "refs/heads/trunk", "e1110093f516b3e595cae3bef8a20c7bf39c2d14", nil)
	if err != nil {
		t.Fatal(err)
	}
	needVerdict(t, v, Unreviewed, "no approving review")
}

// The recorded 403 (what GitHub answered a token that may not read collaborator permissions) makes the check use the review's
// author_association; the real reviewer there is a CONTRIBUTOR, which does not count as write access: refused, and it says so.
func TestRecordedPermissionRefusalFallsBackToAssociation(t *testing.T) {
	c := &Checker{ProtectedBranches: []string{"trunk"}}
	v, err := c.Check(context.Background(), recordedHost{t: t, permission403: true}, "cli", "cli", "refs/heads/trunk", "e1110093f516b3e595cae3bef8a20c7bf39c2d14", nil)
	if err != nil {
		t.Fatal(err)
	}
	needVerdict(t, v, Unreviewed, "approved only by an account without write access", "tidy-dev (association:CONTRIBUTOR)")
}
