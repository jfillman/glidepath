package refprotection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ---- approvers need write access

func TestApproverWithoutWriteAccessDoesNotCount(t *testing.T) {
	for perm, want := range map[string]string{"none": "rando (none)", "read": "rando (read)", "triage": "rando (triage)"} {
		t.Run(perm, func(t *testing.T) {
			v := reviewCase(t, func(p *wPR, w *world) {
				w.perms["rando"] = perm
				p.reviews = approved("rando", p.head)
			})
			needVerdict(t, v, Unreviewed, "approved only by an account without write access to the repository ("+want+")", "GitHub records an approval from anyone who can see a pull request")
		})
	}
	// ... a write, maintain or admin collaborator counts, and the permission is recorded in the evidence
	for _, perm := range []string{"write", "maintain", "admin"} {
		v := reviewCase(t, func(p *wPR, w *world) {
			w.perms["bob"] = perm
			p.reviews = approved("bob", p.head)
		})
		needVerdict(t, v, Protected)
		if got := v.Evidence.PullRequests[0].ApproverPermissions["bob"]; got != perm {
			t.Fatalf("permission recorded %q, want %q", got, perm)
		}
	}
	// the outsider's approval does not stand in for an insider's: with both, the insider's counts
	v := reviewCase(t, func(p *wPR, w *world) {
		w.perms["rando"] = "none"
		p.reviews = []wReview{{user: "rando", state: "APPROVED", commit: p.head}, {user: "bob", state: "APPROVED", commit: p.head}}
	})
	needVerdict(t, v, Protected)
	if j(v.Evidence.PullRequests[0].Approvers) != `["bob"]` {
		t.Fatalf("%v", v.Evidence.PullRequests[0].Approvers)
	}
}

func TestPermissionIsReadOncePerApprover(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	w.squash(2, sh(3), "carol", "bob")
	mustCheck(t, checker(), w, "refs/heads/main", sh(3), []Candidate{{Sha: sh(2)}})
	if n := w.count("collaborators/bob/permission"); n != 1 {
		t.Fatalf("one read for one approver, got %d", n)
	}
}

func TestPermissionReadFailuresFailClosed(t *testing.T) {
	cases := map[string]struct {
		r    resp
		err  error
		want string
	}{
		"500":             {r: resp{500, `{"message":"boom"}`}, want: Unverifiable + "|GitHub answered 500 to the permission of bob"},
		"another 403":     {r: resp{403, `{"message":"Bad credentials"}`}, want: Unverifiable + "|GitHub answered 403 to the permission of bob"},
		"no permission":   {r: resp{200, `{"user":{"login":"bob"}}`}, want: Unverifiable + "|is empty"},
		"unknown account": {r: resp{404, `{"message":"Not Found"}`}, want: Unreviewed + "|approved only by an account without write access to the repository (bob (none))"},
		"transport":       {err: errors.New("connection reset")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
			w.squash(1, sh(2), "alice", "bob")
			if tc.err != nil {
				w.errs["collaborators/bob/permission"] = tc.err
			} else {
				w.permsAs["bob"] = tc.r
			}
			w.setReleases(nil)
			v, err := checker().Check(context.Background(), w, "o", "app", "refs/heads/main", sh(2), withHistory(w))
			if tc.want == "" {
				if err == nil {
					t.Fatalf("a failed read is an error, got %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			verdict, has, _ := strings.Cut(tc.want, "|")
			for _, h := range strings.Split(has, "|") {
				needVerdict(t, v, verdict, h)
			}
		})
	}
}

// When GitHub does not let the token read permissions, the review's own author_association is the fallback, weaker, and said so.
func TestPermissionFallsBackToAssociationAndSaysSo(t *testing.T) {
	for assoc, ok := range map[string]bool{"OWNER": true, "COLLABORATOR": true, "MEMBER": true, "CONTRIBUTOR": false, "NONE": false, "FIRST_TIME_CONTRIBUTOR": false} {
		for _, msg := range []string{"Resource not accessible by integration", "Must have push access to view collaborator permission."} {
			t.Run(assoc+"/"+msg[:5], func(t *testing.T) {
				v := reviewCase(t, func(p *wPR, w *world) {
					w.permsAs["bob"] = resp{403, `{"message":"` + msg + `"}`}
					p.reviews = []wReview{{user: "bob", state: "APPROVED", commit: p.head, assoc: assoc}}
				})
				if ok {
					needVerdict(t, v, Protected)
					if v.Evidence.PullRequests[0].ApproverPermissions["bob"] != "association:"+assoc || len(v.Evidence.Notes) == 0 || !strings.Contains(strings.Join(v.Evidence.Notes, " "), "author_association") {
						t.Fatalf("%+v", v.Evidence)
					}
				} else {
					needVerdict(t, v, Unreviewed, "bob (association:"+assoc+")")
				}
			})
		}
	}
}

// ---- merge commits are GitHub's

// mergeWorld: a pull request #20 (head sh(21), approved by bob) whose merge commit sh(30) has the given committer and signature.
func mergeWorld(t *testing.T, parents []string, committer string, unverified bool) *world {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.prCommit(sh(21), "alice")
	w.land(&wCommit{sha: sh(30), author: "alice", committer: committer, parents: parents, unverified: unverified})
	p := w.pr(&wPR{n: 20, opener: "alice", merger: "mallory", mergeSha: sh(30), commits: []string{sh(21)}})
	p.reviews = approved("bob", p.head)
	w.associate(sh(30), 20)
	return w
}

func TestAMergeCommitPushedByHandIsRefused(t *testing.T) {
	// two parents (base tip, approved head), committed by a person: carries whatever its author put in
	w := mergeWorld(t, []string{sh(1), sh(21)}, "mallory", false)
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(30), nil), Unreviewed, "whose merge commit was not made by GitHub (committer web-flow with a verified signature)", "pushed by hand")
	// a web-flow committer is not enough without GitHub's verified signature
	w = mergeWorld(t, []string{sh(1), sh(21)}, "web-flow", true)
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(30), nil), Unreviewed, "whose merge commit was not made by GitHub")
	// one parent that is the approved head: reviewed code with something on top
	w = mergeWorld(t, []string{sh(21)}, "mallory", false)
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(30), nil), Unreviewed, "whose merge commit was not made by GitHub")
}

func TestAMergeCommitGitHubMadeIsAccepted(t *testing.T) {
	w := mergeWorld(t, []string{sh(1), sh(21)}, "web-flow", false)
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(30), nil)
	needVerdict(t, v, Protected)
	// a lone merge commit is read once (it is not in a range list)
	if w.count("commits/"+sh(30)) != 2 { // its pulls, and itself
		t.Fatalf("%v", w.calls)
	}
	// in a range, the compare list carries committer and signature: no extra read
	w2 := newWorld(t)
	w2.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w2.squash(1, sh(2), "alice", "bob")
	w2.prCommit(sh(21), "alice")
	w2.land(&wCommit{sha: sh(30), author: "alice", committer: "web-flow", parents: []string{sh(2), sh(21)}})
	p := w2.pr(&wPR{n: 20, opener: "alice", merger: "alice", mergeSha: sh(30), commits: []string{sh(21)}})
	p.reviews = approved("bob", p.head)
	w2.associate(sh(30), 20)
	needVerdict(t, mustCheck(t, checker(), w2, "refs/heads/main", sh(30), []Candidate{{Sha: sh(2)}}), Protected)
	if w2.count("commits/"+sh(30)+"?") != 0 || w2.count("commits/"+sh(30)) != 1 {
		t.Fatalf("%v", w2.calls)
	}
}

func TestARebaseShapedTipNeedsTheReviewedContentAndTheApprovedHeadPushedAsIsIsAccepted(t *testing.T) {
	// A commit with ONE parent that is not one of the pull request's commits looks like the tip of a rebase merge, whoever
	// committed it: it is accepted only if what landed IS the reviewed content (rebase_test.go).
	w := mergeWorld(t, []string{sh(1)}, "mallory", false)
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(30), nil), Protected) // the same content as the reviewed head: nothing differs
	w = mergeWorld(t, []string{sh(1)}, "mallory", false)
	w.commits[sh(30)].edits = map[string]string{"evil": "e"} // content nobody reviewed
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(30), nil), Unreviewed, "landed content differs from its reviewed head")
	// the approved head itself landed on the branch by a push: exactly what was reviewed
	w = newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.land(&wCommit{sha: sh(21), author: "alice", committer: "alice"})
	p := w.pr(&wPR{n: 20, opener: "alice", merger: "alice", mergeSha: sh(21), commits: []string{sh(21)}})
	p.reviews = approved("bob", p.head)
	w.associate(sh(21), 20)
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(21), nil), Protected)
}

// ---- the baseline comes from the gitops repository

// goodWorld is releases B(sh(2)) real, then a direct push U(sh(3)), a reviewed squash R(sh(4)) and a reviewed tip(sh(5)).
func baselineWorld(t *testing.T) *world {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	w.land(&wCommit{sha: sh(3), author: "mallory", committer: "mallory"}) // U: never reviewed
	w.squash(4, sh(4), "alice", "bob")                                    // R
	w.squash(5, sh(5), "alice", "bob")
	return w
}

// A release record can no longer pick the baseline: only the gitops history does. The unreviewed commit before a
// reviewed one that nothing real released is found, whatever a record says.
func TestForgedForwardBaselineCannotHideAnUnreviewedCommit(t *testing.T) {
	w := baselineWorld(t)
	v := mustCheck(t, checker(), w, "refs/heads/main", sh(5), []Candidate{{Sha: sh(2), Release: "real"}})
	needVerdict(t, v, Unreviewed, short(sh(3))+": no pull request")
	if v.Evidence.From != sh(2) || len(v.Evidence.Commits) != 3 {
		t.Fatalf("the range starts at the real release: %+v", v.Evidence)
	}
	// the same answer however often it is asked: no record is read, so deleting every one changes nothing
	for i := 0; i < 2; i++ {
		v2 := mustCheck(t, checker(), w, "refs/heads/main", sh(5), []Candidate{{Sha: sh(2), Release: "real"}})
		if v2.Verdict != v.Verdict || v2.Evidence.From != v.Evidence.From {
			t.Fatalf("%+v", v2)
		}
	}
	// once U is really released and reviewed away (a real release of R), the range is R..tip
	v = mustCheck(t, checker(), w, "refs/heads/main", sh(5), []Candidate{{Sha: sh(4), Release: "real R"}, {Sha: sh(2)}})
	needVerdict(t, v, Protected)
	if v.Evidence.From != sh(4) {
		t.Fatalf("%+v", v.Evidence)
	}
}

func TestNoReleaseInTheGitopsHistoryIsAFirstRelease(t *testing.T) {
	w := baselineWorld(t)
	for name, mut := range map[string]func(){
		"no pull requests at all": func() {},
		"unmerged release":        func() { g := w.release(sh(2)); g.mergedAt = "" },
		"not a release":           func() { g := w.release(sh(2)); g.title = "Bump chart" },
	} {
		t.Run(name, func(t *testing.T) {
			w.gitops = nil
			mut()
			v, err := checker().Check(context.Background(), w, "o", "app", "refs/heads/main", sh(5), withHistory(w))
			if err != nil {
				t.Fatal(err)
			}
			needVerdict(t, v, Protected)
			if v.Evidence.Mode != "first" || !strings.Contains(strings.Join(v.Evidence.Notes, " "), "first release") {
				t.Fatalf("%+v", v.Evidence)
			}
		})
	}
	// a repository with no release history to read (the gitops repository itself)
	v, err := checker().Check(context.Background(), w, "o", "app", "refs/heads/main", sh(5), nil)
	if err != nil || v.Evidence.Mode != "first" {
		t.Fatalf("%v %+v", err, v)
	}
}

// If the gitops history shows an earlier release the answer is never "a first release".
func TestHistoryThatShowsReleasesButNoBaselineIsUnverifiable(t *testing.T) {
	cases := map[string]func(g *gRelease){
		"opened by someone else":               func(g *gRelease) { g.login = "mallory" },
		"no trailers":                          func(g *gRelease) { g.message = "Release: promote app to dev" },
		"another repository":                   func(g *gRelease) { g.message = strings.ReplaceAll(g.message, "github.com/o/app", "github.com/o/other") },
		"the manifest's tag is not the commit": func(g *gRelease) { g.patch = "@@\n-    tag: sha-1\n+    tag: sha-deadbee" },
		"two files":                            func(g *gRelease) { g.files = []string{"a/dev/release.yaml", "a/dev/values.yaml"} },
		"not a manifest":                       func(g *gRelease) { g.files = []string{"README.md"} },
		"two commits":                          func(g *gRelease) { g.commits = 2 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			w := baselineWorld(t)
			mut(w.release(sh(2)))
			v, err := checker().Check(context.Background(), w, "o", "app", "refs/heads/main", sh(5), withHistory(w))
			if err != nil {
				t.Fatal(err)
			}
			needVerdict(t, v, Unverifiable)
			if v.Evidence != nil && v.Evidence.Mode == "first" {
				t.Fatalf("never a first release: %+v", v.Evidence)
			}
		})
	}
	// ... but a corroborated older release is still found behind a release that is not
	w := baselineWorld(t)
	w.release(sh(2))
	w.release(sh(4)).login = "mallory"
	v := mustCheck2(t, w, sh(5))
	if v.Evidence == nil || v.Evidence.From != sh(2) {
		t.Fatalf("the corroborated release starts the range: %+v", v)
	}
	needVerdict(t, v, Unreviewed, short(sh(3))+": no pull request") // the direct push after it is in the range
}

func mustCheck2(t *testing.T, w *world, to string) *Verdict {
	v, err := checker().Check(context.Background(), w, "o", "app", "refs/heads/main", to, withHistory(w))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUnreadableGitopsHistoryIsUnverifiable(t *testing.T) {
	for name, set := range map[string]func(w *world){
		"500":     func(w *world) { w.override["gitops:pulls"] = resp{500, `{"message":"boom"}`} },
		"404":     func(w *world) { w.override["gitops:pulls"] = resp{404, `{"message":"Not Found"}`} },
		"garbage": func(w *world) { w.override["gitops:pulls"] = resp{200, `{"not":"a list"}`} },
		"commits": func(w *world) { w.override["gitops:pulls/900/commits"] = resp{500, ``} },
		"files":   func(w *world) { w.override["gitops:pulls/900/files"] = resp{403, `{"message":"no"}`} },
	} {
		t.Run(name, func(t *testing.T) {
			w := baselineWorld(t)
			w.release(sh(2))
			set(w)
			v := mustCheck2(t, w, sh(5))
			needVerdict(t, v, Unverifiable)
			if v.Evidence != nil && len(v.Evidence.Commits) > 0 {
				t.Fatalf("no evidence for a range that was not established: %+v", v.Evidence)
			}
		})
	}
	w := baselineWorld(t)
	w.errs["gitops:pulls"] = errors.New("connection reset")
	if _, err := checker().Check(context.Background(), w, "o", "app", "refs/heads/main", sh(5), withHistory(w)); err == nil {
		t.Fatal("a transport failure is an error")
	}
}

func TestGitopsHistoryPaging(t *testing.T) {
	// the release is on page 2 behind 120 other closed pull requests
	w := baselineWorld(t)
	for i := 0; i < 120; i++ {
		g := w.release(sh(2))
		g.title = fmt.Sprintf("Bump %d", i)
	}
	w.release(sh(2))
	v := mustCheck2(t, w, sh(5))
	if v.Evidence == nil || v.Evidence.From != sh(2) {
		t.Fatalf("%+v %v", v.Evidence, v.Reasons)
	}
	// 300 closed pull requests and none is a release of this app: the history is not read in full, never "a first release"
	w = baselineWorld(t)
	for i := 0; i < 300; i++ {
		w.release(sh(2)).title = fmt.Sprintf("Bump %d", i)
	}
	needVerdict(t, mustCheck2(t, w, sh(5)), Unverifiable, "not read in full")
}
