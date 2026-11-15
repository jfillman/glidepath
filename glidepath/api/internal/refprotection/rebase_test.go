package refprotection

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// rebaseWorld: main is root sh(1) (file base=b0), optionally a base commit sh(2) that changes `baseEdits` after the pull request
// forked, then the pull request #4's two commits (f1=p1, f2=p2, forked from sh(1)) landed by a rebase merge as sh(42), sh(43)
// (committed by the merger, not GitHub's web-flow: nothing signs them). landed1/landed2 are what each landed commit edits.
func rebaseWorld(t *testing.T, baseEdits, landed1, landed2 map[string]string) (*world, string) {
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root", edits: map[string]string{"base": "b0"}})
	parent := sh(1)
	if baseEdits != nil {
		w.land(&wCommit{sha: sh(2), author: "carol", committer: "carol", edits: baseEdits})
		parent = sh(2)
	}
	p1 := &wCommit{sha: sh(40), author: "gina", committer: "gina", parents: []string{sh(1)}, edits: map[string]string{"f1": "p1"}}
	p2 := &wCommit{sha: sh(41), author: "gina", committer: "gina", parents: []string{sh(40)}, edits: map[string]string{"f2": "p2"}}
	for _, c := range []*wCommit{p1, p2} {
		c.aname, c.cname = c.author, c.committer
		w.commits[c.sha] = c
		w.side[c.sha] = true
	}
	w.land(&wCommit{sha: sh(42), author: "gina", committer: "mallory", parents: []string{parent}, edits: landed1},
		&wCommit{sha: sh(43), author: "gina", committer: "mallory", parents: []string{sh(42)}, edits: landed2})
	pr := w.pr(&wPR{n: 4, opener: "gina", merger: "mallory", mergeSha: sh(43), commits: []string{sh(40), sh(41)}})
	pr.reviews = approved("bob", pr.head)
	w.associate(sh(42), 4)
	w.associate(sh(43), 4)
	return w, parent
}

func checkRebase(t *testing.T, w *world) *Verdict {
	t.Helper()
	return mustCheck(t, checker(), w, "refs/heads/main", sh(43), nil)
}

// A true rebase landing: the same files with the same resulting blobs as the reviewed head, over the same base or over a base that
// changed other files.
func TestARebaseLandingOfTheReviewedContentIsAccepted(t *testing.T) {
	for name, base := range map[string]map[string]string{"the base did not move": nil, "the base changed another file": {"f9": "x9"}} {
		t.Run(name, func(t *testing.T) {
			w, _ := rebaseWorld(t, base, map[string]string{"f1": "p1"}, map[string]string{"f2": "p2"})
			v := checkRebase(t, w)
			needVerdict(t, v, Protected)
			if len(v.Evidence.PullRequests) != 1 || v.Evidence.PullRequests[0].Number != 4 {
				t.Fatalf("%+v", v.Evidence)
			}
			// reads added for this rebase-merged pull request: the run (1) and two compares (1 page each)
			if w.count("commits?sha=") != 1 || w.count("compare/"+sh(1)+"...") != 2 && w.count("compare/"+sh(2)+"...") != 2 {
				t.Fatalf("%v", w.calls)
			}
		})
	}
}

func TestAHandMadeLinearCommitAfterAnApprovedPullRequestIsRefused(t *testing.T) {
	// the approved pull request changed f1 and f2; what landed also carries a file nobody reviewed
	w, _ := rebaseWorld(t, nil, map[string]string{"f1": "p1"}, map[string]string{"f2": "p2", "evil": "e"})
	needVerdict(t, checkRebase(t, w), Unreviewed, rebaseRefusal)
	// a changed blob of a reviewed file
	w, _ = rebaseWorld(t, nil, map[string]string{"f1": "p1-evil"}, map[string]string{"f2": "p2"})
	needVerdict(t, checkRebase(t, w), Unreviewed, rebaseRefusal)
	// a reviewed file missing from the landing
	w, _ = rebaseWorld(t, nil, map[string]string{"f1": "p1"}, map[string]string{})
	needVerdict(t, checkRebase(t, w), Unreviewed, rebaseRefusal)
	// and the second commit of the run is refused with the first (neither is that pull request's landing)
	v := checkRebase(t, w)
	if v.Evidence.Commits[0].Reviewed {
		t.Fatalf("%+v", v.Evidence.Commits)
	}
}

func TestARebaseOverABaseThatTouchedTheSameFileIsRefusedWithTheReason(t *testing.T) {
	// the base changed f1 after the fork; the rebase merged both, so f1's blob is neither the reviewed one nor the base's
	w, _ := rebaseWorld(t, map[string]string{"f1": "x1"}, map[string]string{"f1": "p1+x1"}, map[string]string{"f2": "p2"})
	v := checkRebase(t, w)
	needVerdict(t, v, Unreviewed, rebaseRefusal)
	if !strings.Contains(strings.Join(v.Missing, " "), "f1") {
		t.Fatalf("the first differing file is named: %v", v.Missing)
	}
}

func TestARebaseThatLandedFewerCommitsThanThePullRequestHasIsRefused(t *testing.T) {
	// GitHub drops commits that were empty to begin with: here the pull request counts 3 and only 2 landed, so the run of 3
	// reaches back over the base commit, whose changes are not the pull request's
	w, _ := rebaseWorld(t, map[string]string{"f9": "x9"}, map[string]string{"f1": "p1"}, map[string]string{"f2": "p2"})
	w.prs[4].count = 3
	needVerdict(t, checkRebase(t, w), Unreviewed, rebaseRefusal)
}

func TestARebaseWithMoreThan300FilesIsUnverifiable(t *testing.T) {
	big := map[string]string{}
	for i := 0; i < 301; i++ {
		big[fmt.Sprintf("gen/f%03d", i)] = "g"
	}
	w, _ := rebaseWorld(t, nil, big, map[string]string{"f2": "p2"})
	w.commits[sh(40)].edits = big
	needVerdict(t, checkRebase(t, w), Unverifiable, "has more than 300 entries")
	// just under the cap is read in full
	small := map[string]string{}
	for i := 0; i < 250; i++ {
		small[fmt.Sprintf("gen/f%03d", i)] = "g"
	}
	w, _ = rebaseWorld(t, nil, small, map[string]string{"f2": "p2"})
	w.commits[sh(40)].edits = small
	needVerdict(t, checkRebase(t, w), Protected)
}

func TestASquashAndAGitHubMergeNeedNoContentComparison(t *testing.T) {
	// web-flow with a verified signature already says GitHub made the commit from the head: no run or compare is read for it
	w := newWorld(t)
	w.land(&wCommit{sha: sh(1), author: "root", committer: "root"})
	w.squash(1, sh(2), "alice", "bob")
	needVerdict(t, mustCheck(t, checker(), w, "refs/heads/main", sh(2), nil), Protected)
	if w.count("commits?sha=") != 0 || w.count("compare/"+sh(2)) != 0 {
		t.Fatalf("%v", w.calls)
	}
}

// The branch history GitHub lists is by date: a run is only a run if each commit's first parent is the next one listed.
func TestARunThatIsNotAChainIsRefused(t *testing.T) {
	w, _ := rebaseWorld(t, nil, map[string]string{"f1": "p1"}, map[string]string{"f2": "p2"})
	// the listing puts an unrelated commit between the two landed ones
	w.commits[sh(77)] = &wCommit{sha: sh(77), author: "x", committer: "x", parents: []string{sh(1)}}
	w.override["commits"] = resp{200, "[" + w.commits[sh(43)].json() + "," + w.commits[sh(77)].json() + "," + w.commits[sh(42)].json() + "]"}
	needVerdict(t, checkRebase(t, w), Unreviewed, "not the tip of a rebase landing")
	// a merge commit inside the run is not a rebase either
	w, _ = rebaseWorld(t, nil, map[string]string{"f1": "p1"}, map[string]string{"f2": "p2"})
	w.commits[sh(42)].parents = []string{sh(1), sh(77)}
	w.commits[sh(77)] = &wCommit{sha: sh(77), author: "x", committer: "x", parents: []string{sh(1)}}
	needVerdict(t, checkRebase(t, w), Unreviewed, "not the tip of a rebase landing")
}

// GitHub's own compare shape (testdata/real_compare_files.json, recorded from a public repository): the file entries carry the
// resulting blob sha, the status and the file name, which is what the content comparison keys on.
func TestRecordedCompareFilesShape(t *testing.T) {
	b, err := os.ReadFile("testdata/real_compare_files.json")
	if err != nil {
		t.Fatal(err)
	}
	e := &engine{rd: fixedReader(b), owner: "cli", repo: "cli"}
	got, err := e.compareFiles(context.Background(), "a", "b")
	if err != nil || len(got) == 0 {
		t.Fatalf("%v %v", got, err)
	}
	for name, desc := range got {
		parts := strings.Split(desc, "|")
		if name == "" || len(parts) != 3 || !shaRE.MatchString(parts[1]) || parts[0] == "" {
			t.Fatalf("%q: %q", name, desc)
		}
	}
}

type fixedReader []byte

func (f fixedReader) GetJSON(context.Context, string, string, string) (int, []byte, error) {
	return 200, []byte(f), nil
}
