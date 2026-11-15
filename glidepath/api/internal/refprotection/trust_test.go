package refprotection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/glidepath/api/internal/github"
)

// ---- approvers need push access (GraphQL authorCanPushToRepository)

func TestApproverWithoutPushAccessDoesNotCount(t *testing.T) {
	for _, perm := range []string{"none", "read", "triage"} {
		t.Run(perm, func(t *testing.T) {
			v := reviewCase(t, func(p *wPR, w *world) {
				w.perms["rando"] = perm
				p.reviews = approved("rando", p.head)
			})
			needVerdict(t, v, Unreviewed, "approved only by an account without push access to the repository (rando)", "GitHub records an approval from anyone who can see a pull request")
		})
	}
	// ... an account that may push counts, and the fact is recorded in the evidence
	for _, perm := range []string{"write", "maintain", "admin"} {
		v := reviewCase(t, func(p *wPR, w *world) {
			w.perms["bob"] = perm
			p.reviews = approved("bob", p.head)
		})
		needVerdict(t, v, Protected)
		if got := v.Evidence.PullRequests[0].ApproverCanPush["bob"]; !got {
			t.Fatalf("canPush not recorded: %+v", v.Evidence.PullRequests[0])
		}
	}
	// the outsider's approval does not stand in for an insider's
	v := reviewCase(t, func(p *wPR, w *world) {
		w.perms["rando"] = "none"
		p.reviews = []wReview{{user: "rando", state: "APPROVED", commit: p.head}, {user: "bob", state: "APPROVED", commit: p.head}}
	})
	needVerdict(t, v, Protected)
	if j(v.Evidence.PullRequests[0].Approvers) != `["bob"]` || v.Evidence.PullRequests[0].ApproverCanPush["rando"] {
		t.Fatalf("%+v", v.Evidence.PullRequests[0])
	}
}

func TestReviewsAreReadOncePerPullRequestAndNoRESTPermissionIsRead(t *testing.T) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	w.squash(2, sh(3), "carol", "bob")
	mustCheck(t, checker(), w, "refs/heads/main", sh(3), []Candidate{{Sha: sh(2)}})
	if w.gqlCalls != 2 || w.count("collaborators/") != 0 || w.count("/reviews") != 0 {
		t.Fatalf("graphql %d: %v", w.gqlCalls, w.calls)
	}
}

func TestGraphQLFailuresFailClosed(t *testing.T) {
	cases := map[string]struct {
		set  func(w *world)
		want string // "" = an error is returned
	}{
		"500": {func(w *world) { w.gql = &resp{500, `{"message":"boom"}`} }, Unverifiable + "|GitHub answered 500 to the reviews of #1"},
		"200 with errors": {func(w *world) {
			w.gql = &resp{200, `{"data":null,"errors":[{"message":"Resource not accessible by integration"}]}`}
		}, Unverifiable + "|GraphQL answered an error|not accessible"},
		"errors beside data": {func(w *world) {
			w.gql = &resp{200, `{"data":{"repository":{"pullRequest":null}},"errors":[{"message":"x"}]}`}
		}, Unverifiable + "|GraphQL answered an error"},
		"no pull request": {func(w *world) { w.gql = &resp{200, `{"data":{"repository":{"pullRequest":null}}}`} }, Unverifiable + "|shows no pull request #1"},
		"garbage":         {func(w *world) { w.gql = &resp{200, `not json`} }, Unverifiable + "|could not be read"},
		"no push answer": {func(w *world) {
			w.gql = &resp{200, `{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":false},"nodes":[{"state":"APPROVED","author":{"login":"bob","__typename":"User"},"commit":{"oid":"x"}}]}}}}}`}
		}, Unverifiable + "|did not say whether bob may push"},
		"transport":    {func(w *world) { w.gqlErr = errors.New("connection reset") }, ""},
		"rate limited": {func(w *world) { w.gqlErr = github.RateLimited{} }, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
			w.squash(1, sh(2), "alice", "bob")
			tc.set(w)
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
			if v.Evidence != nil {
				t.Fatalf("no partial evidence: %+v", v.Evidence)
			}
		})
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
		"no pull requests at all":                func() {},
		"unmerged release":                       func() { g := w.release(sh(2)); g.mergedAt = "" },
		"merged but changes no release manifest": func() { g := w.release(sh(2)); g.files = []string{"README.md"} },
		"a release of another environment":       func() { w.histEnv = "prod"; w.release(sh(2)) },
	} {
		t.Run(name, func(t *testing.T) {
			w.gitops, w.histEnv = nil, ""
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

// Retitling or re-describing release pull requests hides nothing: the history is decided from what they changed, and a release is
// identified by what was merged. (Anyone with write or triage rights on the gitops repository can edit a title, even after a merge.)
func TestRetitlingReleasePullRequestsHidesNoHistory(t *testing.T) {
	w := baselineWorld(t)
	g := w.release(sh(2))
	base := mustCheck2(t, w, sh(5))
	needVerdict(t, base, Unreviewed, short(sh(3))+": no pull request") // the direct push after the real release
	for _, title := range []string{"Bump chart", "", "Release: other to dev @ x", "docs"} {
		g.title = title
		v := mustCheck2(t, w, sh(5))
		if v.Verdict != base.Verdict || v.Evidence == nil || v.Evidence.From != sh(2) || v.Evidence.Mode != "range" {
			t.Fatalf("title %q changed the answer: %+v", title, v)
		}
	}
	// ... and when the retitled pull request cannot be corroborated, the history still shows a release: unverifiable, never a first release
	g.title = "Bump chart"
	g.message = "Bump chart"
	v := mustCheck2(t, w, sh(5))
	needVerdict(t, v, Unverifiable, "cannot establish which commits this release carries")
	if v.Evidence != nil {
		t.Fatalf("%+v", v.Evidence)
	}
}

// A release pull request is identified by what was merged, each fact read at the merge.
func TestHistoryThatShowsReleasesButNoBaselineIsUnverifiable(t *testing.T) {
	cases := map[string]func(g *gRelease){
		"opened by someone else":                  func(g *gRelease) { g.login = "mallory" },
		"no trailers":                             func(g *gRelease) { g.message = "Release: promote app to dev" },
		"another repository":                      func(g *gRelease) { g.message = strings.ReplaceAll(g.message, "github.com/o/app", "github.com/o/other") },
		"the manifest as merged sets another tag": func(g *gRelease) { g.tag = "sha-deadbee" },
		"two files":                               func(g *gRelease) { g.files = append(g.files, "hangar-dev/dev/values.yaml") },
		"a file besides the manifest":             func(g *gRelease) { g.files = append(g.files, "README.md") },
		"two commits":                             func(g *gRelease) { g.commits = 2 },
		"its one commit is not its head":          func(g *gRelease) { g.prHead = sh(1234) },
		"the commit is a person's":                func(g *gRelease) { g.commitLogin = "mallory" },
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
			if v.Evidence != nil {
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

// Forging a baseline: take an open pull request the platform's account opened, push a replacement commit naming a reviewed commit R
// above an unreviewed U, and merge it. The opener is still the account, but a person's commit is not the pipeline's.
func TestAReplacementCommitByAPersonIsNotARelease(t *testing.T) {
	w := baselineWorld(t)
	w.release(sh(2))
	forged := w.release(sh(4)) // R, above the unreviewed U (sh(3))
	forged.commitLogin = "mallory"
	v := mustCheck2(t, w, sh(5))
	if v.Evidence == nil || v.Evidence.From != sh(2) {
		t.Fatalf("the forged release does not start the range: %+v", v)
	}
	needVerdict(t, v, Unreviewed, short(sh(3))+": no pull request")
	// the replacement's trailer names R but its manifest as merged does not
	forged.commitLogin, forged.tag = "", "sha-"+short(sh(3))
	v = mustCheck2(t, w, sh(5))
	if v.Evidence == nil || v.Evidence.From != sh(2) {
		t.Fatalf("%+v", v)
	}
}

func mustCheck2(t *testing.T, w *world, to string) *Verdict {
	t.Helper()
	v, err := checker().Check(context.Background(), w, "o", "app", "refs/heads/main", to, withHistory(w))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The baseline is a release of the SAME environment (and cluster) the run is releasing to; with no single target the strictest
// reading applies: the oldest of the environments' newest releases.
func TestTheBaselineIsARereleaseOfTheSameEnvironment(t *testing.T) {
	w := baselineWorld(t)
	w.release(sh(2)).env = "prod"
	w.gitops[0].files = []string{"hangar-dev/prod/release.yaml"}
	dev := w.release(sh(4)) // a newer release, to dev
	_ = dev
	for env, from := range map[string]string{"prod": sh(2), "dev": sh(4), "": sh(2)} {
		t.Run("env="+env, func(t *testing.T) {
			w.histEnv = env
			v := mustCheck2(t, w, sh(5))
			if v.Evidence == nil || v.Evidence.From != from {
				t.Fatalf("env %q: from %v, want %s: %v %v", env, v.Evidence, short(from), v.Reasons, v.Missing)
			}
		})
	}
	// an environment with no release: a first release (its history shows none); another cluster is another target
	w.histEnv = "staging"
	if v := mustCheck2(t, w, sh(5)); v.Evidence.Mode != "first" {
		t.Fatalf("%+v", v.Evidence)
	}
	w.histEnv, w.histCluster = "dev", "hangar-prod"
	if v := mustCheck2(t, w, sh(5)); v.Evidence.Mode != "first" {
		t.Fatalf("%+v", v.Evidence)
	}
}

func TestUnreadableGitopsHistoryIsUnverifiable(t *testing.T) {
	for name, set := range map[string]func(w *world){
		"list 500":     func(w *world) { w.override["gitops:pulls"] = resp{500, `{"message":"boom"}`} },
		"list 404":     func(w *world) { w.override["gitops:pulls"] = resp{404, `{"message":"Not Found"}`} },
		"list garbage": func(w *world) { w.override["gitops:pulls"] = resp{200, `{"not":"a list"}`} },
		"files":        func(w *world) { w.override["gitops:pulls/900/files"] = resp{403, `{"message":"no"}`} },
		"pull":         func(w *world) { w.override["gitops:pulls/900"] = resp{500, ``} },
		"commits":      func(w *world) { w.override["gitops:pulls/900/commits"] = resp{500, ``} },
		"contents":     func(w *world) { w.override["gitops:contents/hangar-dev/dev/release.yaml"] = resp{500, ``} },
	} {
		t.Run(name, func(t *testing.T) {
			w := baselineWorld(t)
			w.release(sh(2))
			set(w)
			v := mustCheck2(t, w, sh(5))
			needVerdict(t, v, Unverifiable)
			if v.Evidence != nil {
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

func TestGitopsHistoryWindow(t *testing.T) {
	// the release is behind 20 merged pull requests that change other files: within the scan window, found
	w := baselineWorld(t)
	w.release(sh(2))
	for i := 0; i < 20; i++ {
		g := w.release(sh(2))
		g.files = []string{fmt.Sprintf("docs/%d.md", i)}
	}
	v := mustCheck2(t, w, sh(5))
	if v.Evidence == nil || v.Evidence.From != sh(2) {
		t.Fatalf("%+v %v", v.Evidence, v.Reasons)
	}
	// ... behind more merged pull requests than the window reads: the history is not read in full, never "a first release"
	w = baselineWorld(t)
	w.release(sh(2))
	for i := 0; i < 40; i++ {
		g := w.release(sh(2))
		g.files = []string{fmt.Sprintf("docs/%d.md", i)}
	}
	needVerdict(t, mustCheck2(t, w, sh(5)), Unverifiable, "not read in full")
	// more than 100 closed pull requests are paged
	w = baselineWorld(t)
	for i := 0; i < 120; i++ {
		g := w.release(sh(2))
		g.mergedAt = ""
	}
	w.release(sh(2))
	v = mustCheck2(t, w, sh(5))
	if v.Evidence == nil || v.Evidence.From != sh(2) {
		t.Fatalf("%+v %v", v.Evidence, v.Reasons)
	}
}

// ---- a release pull request that was changed after it was opened is not a baseline

func TestAChangedReleasePullRequestIsHistoryButNotABaseline(t *testing.T) {
	for name, events := range map[string][]string{
		"force pushed to its head": {"head_ref_force_pushed"},
		"reopened":                 {"reopened"},
		"head branch restored":     {"head_ref_deleted", "head_ref_restored"},
		"a commit added":           {"committed"},
	} {
		t.Run(name, func(t *testing.T) {
			w := baselineWorld(t)
			w.release(sh(2)).events = events
			v := mustCheck2(t, w, sh(5))
			needVerdict(t, v, Unverifiable, "cannot establish which commits this release carries", "its timeline shows")
			if v.Evidence != nil {
				t.Fatalf("never a first release: %+v", v.Evidence)
			}
		})
	}
	// events a normal release has (labels, the head branch deleted after the merge, a force push of the BASE) change nothing
	w := baselineWorld(t)
	w.release(sh(2)).events = []string{"labeled", "base_ref_force_pushed", "merged", "head_ref_deleted", "closed"}
	if v := mustCheck2(t, w, sh(5)); v.Evidence == nil || v.Evidence.From != sh(2) {
		t.Fatalf("%+v", v)
	}
	// an older untouched release is still a baseline behind a changed one
	w = baselineWorld(t)
	w.release(sh(2))
	w.release(sh(4)).events = []string{"head_ref_force_pushed"}
	if v := mustCheck2(t, w, sh(5)); v.Evidence == nil || v.Evidence.From != sh(2) {
		t.Fatalf("%+v", v)
	}
}

func TestTimelineReadFailuresAreUnverifiable(t *testing.T) {
	for name, set := range map[string]func(w *world){
		"500":      func(w *world) { w.override["gitops:issues/900/timeline"] = resp{500, ``} },
		"garbage":  func(w *world) { w.override["gitops:issues/900/timeline"] = resp{200, `{"x":1}`} },
		"too long": func(w *world) { w.gitops[0].manyEvents = 300 },
	} {
		t.Run(name, func(t *testing.T) {
			w := baselineWorld(t)
			w.release(sh(2))
			set(w)
			v := mustCheck2(t, w, sh(5))
			needVerdict(t, v, Unverifiable)
			if v.Evidence != nil {
				t.Fatalf("%+v", v.Evidence)
			}
		})
	}
}

// GitHub's own timelines (testdata): a force-pushed pull request shows head_ref_force_pushed beside one committed event, a clean
// one just the one commit.
func TestRecordedTimelines(t *testing.T) {
	for file, want := range map[string]string{"real_timeline_force_pushed.json": "head_ref_force_pushed", "real_timeline_clean.json": ""} {
		b, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		e := &engine{rd: fixedReader(b), owner: "cli", repo: "cli"}
		why, err := e.timelineWhy(context.Background(), "cli", 1)
		if err != nil || (want == "") != (why == "") || !strings.Contains(why, want) {
			t.Fatalf("%s: %q %v", file, why, err)
		}
	}
}
