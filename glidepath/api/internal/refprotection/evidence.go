package refprotection

// The evidence engine: the range of commits, the merged pull request of each, and that pull request's review evidence.
// Every fact is a GitHub-authenticated field (a pull request's opener and merger, its commits' linked accounts, a review's
// reviewer, state and commit_id), never text a commit or a pull request carries. See the package doc and
// docs/admin/security.md "Ref protection" for the rules and the reason for each.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type engine struct {
	c           *Checker
	rd          Reader
	owner, repo string
	hist        *History
	calls       int
	prs         map[int]*prState  // pull requests read in THIS check, by number (never kept across requests)
	perms       map[string]string // reviewer login (lower case) -> repository permission read in THIS check
	infos       map[string]*commitInfo
	assocUsed   bool // a reviewer's write access was taken from author_association, not read
}

// commitInfo is what the merge-commit rule needs of one commit.
type commitInfo struct {
	committer string
	verified  bool
	parents   []string
}

type prState struct {
	PullEvidence
	commitSet map[string]bool
	count     int // the pull request's own commit count
	// rebaseRun (once per pull request): the commits a rebase merge landed from it, when what landed is its reviewed content
	run            map[string]bool
	runDone, runOK bool
	runWhy         string
}

// do is one budgeted read. A status is returned, not judged.
func (e *engine) do(ctx context.Context, path string) (int, []byte, error) {
	if e.calls >= maxEvidenceCalls {
		return 0, nil, &unverifiable{fmt.Sprintf("the evidence needs more than %d GitHub reads: the range or its pull requests are too large to verify in one check", maxEvidenceCalls)}
	}
	e.calls++
	return e.rd.GetJSON(ctx, e.owner, e.repo, path)
}

// ok is a read that must be 200; any other status makes the evidence unverifiable (never "reviewed").
func (e *engine) ok(ctx context.Context, path string) ([]byte, error) {
	st, body, err := e.do(ctx, path)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		p, _, _ := strings.Cut(path, "?")
		return nil, &unverifiable{fmt.Sprintf("GitHub answered %d to GET %s: %s", st, p, hostMessage(body))}
	}
	return body, nil
}

// paged reads path in pages of 100 (at most maxPages); each returns how many entries its page held.
func (e *engine) paged(ctx context.Context, path string, maxPages int, each func([]byte) (int, error)) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		body, err := e.ok(ctx, fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page))
		if err != nil {
			return err
		}
		n, err := each(body)
		if err != nil {
			return &unverifiable{fmt.Sprintf("GitHub's answer to GET %s could not be read: %v", strings.SplitN(path, "?", 2)[0], err)}
		}
		if n < 100 {
			return nil
		}
		if page == maxPages {
			return &unverifiable{fmt.Sprintf("GET %s has more than %d entries: not read further", strings.SplitN(path, "?", 2)[0], maxPages*100)}
		}
	}
	return nil
}

// ---- GitHub shapes

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ghCommit struct {
	SHA       string  `json:"sha"`
	Author    *ghUser `json:"author"`
	Committer *ghUser `json:"committer"`
	Parents   []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Commit struct {
		Author struct {
			Name string `json:"name"`
		} `json:"author"`
		Committer struct {
			Name string `json:"name"`
		} `json:"committer"`
		Verification struct {
			Verified bool `json:"verified"`
		} `json:"verification"`
	} `json:"commit"`
}

type ghRepoRef struct {
	FullName string `json:"full_name"`
}

type ghPull struct {
	Number         int     `json:"number"`
	HTMLURL        string  `json:"html_url"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Commits        int     `json:"commits"`
	User           *ghUser `json:"user"`
	MergedBy       *ghUser `json:"merged_by"`
	Head           struct {
		SHA  string     `json:"sha"`
		Repo *ghRepoRef `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string     `json:"ref"`
		Repo *ghRepoRef `json:"repo"`
	} `json:"base"`
}

type ghReview struct {
	User     *ghUser `json:"user"`
	State    string  `json:"state"`
	CommitID string  `json:"commit_id"`
	// AuthorAssociation is GitHub's word for the reviewer's relationship to the repository (OWNER, MEMBER, COLLABORATOR,
	// CONTRIBUTOR, NONE ...). It is only a fallback for write access when the permission read is not allowed.
	AuthorAssociation string `json:"author_association"`
}

// ---- ancestry and range

// onBranch: is sha an ancestor of (or the head of) branch? compare branch...sha says "behind" or "identical".
func (e *engine) onBranch(ctx context.Context, branch, sha string) (bool, string, error) {
	st, body, err := e.do(ctx, "compare/"+escapeSegs(branch)+"..."+sha+"?per_page=1")
	if err != nil {
		return false, "", err
	}
	switch st {
	case http.StatusOK:
		var c struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &c); err != nil {
			return false, "", &unverifiable{fmt.Sprintf("GitHub's compare of %s...%s could not be read: %v", branch, short(sha), err)}
		}
		if c.Status == "behind" || c.Status == "identical" {
			return true, "", nil
		}
		return false, "the host reports it " + c.Status + " of the branch", nil
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return false, "", &unverifiable{"the host does not know " + short(sha) + " as a commit of this repository: it is not a commit (an annotated tag's own object sha is refused: the run must record the commit it points at) or the branch is missing"}
	}
	return false, "", &unverifiable{fmt.Sprintf("GitHub answered %d to the compare of %s...%s: %s", st, branch, short(sha), hostMessage(body))}
}

type rangeCommit struct {
	sha       string
	parents   []string
	committer string
	verified  bool
	known     bool // read from GitHub's compare (a lone commit carries only its sha)
}

type commitRange struct {
	mode        string
	from        string
	fromRelease string
	commits     []rangeCommit // oldest first, to inclusive
	notes       []string
}

// resolveRange decides which commits must carry evidence. The baseline is the newest release the app's GITOPS repository
// corroborates (history.go) that is an ancestor of the commit per GitHub's compare and whose own commit has review evidence.
// Release records (ConfigMaps) are not an input: the pipeline's ServiceAccount can write and delete them, so a forged one could
// otherwise shorten the range. Neither the gitops history nor GitHub's ancestry can be edited by it. If the gitops history
// shows an earlier release but none can start the range, the answer is unverifiable, never a first release; a first release
// (the commit alone) is only when the history shows none.
func (e *engine) resolveRange(ctx context.Context, branch, to string) (*commitRange, error) {
	r := &commitRange{mode: "first", commits: []rangeCommit{{sha: to}}}
	if e.hist == nil {
		r.notes = append(r.notes, "no release history applies to this repository (it is not an app's source repository): the commit alone is checked")
		return r, nil
	}
	cands, sawAny, notes, err := e.releases(ctx)
	if err != nil {
		return nil, err
	}
	r.notes = append(r.notes, notes...)
	if !sawAny {
		r.notes = append(r.notes, "the gitops history shows no earlier release of this app: this is a first release, so the commit alone is checked")
		return r, nil
	}
	var skipped []string
	for _, c := range cands {
		st, body, err := e.do(ctx, fmt.Sprintf("compare/%s...%s?per_page=1", c.Sha, to))
		if err != nil {
			return nil, err
		}
		switch st {
		case http.StatusOK:
			var cmp struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(body, &cmp); err != nil {
				return nil, &unverifiable{fmt.Sprintf("GitHub's compare of %s...%s could not be read: %v", short(c.Sha), short(to), err)}
			}
			switch cmp.Status {
			case "ahead":
				ce, err := e.commitEvidence(ctx, branch, c.Sha, nil, nil)
				if err != nil {
					return nil, err
				}
				if !ce.Reviewed {
					skipped = append(skipped, fmt.Sprintf("%s (%s) itself has no review evidence: %s", short(c.Sha), c.Release, ce.Reason))
					continue
				}
				list, err := e.listRange(ctx, c.Sha, to)
				if err != nil {
					return nil, err
				}
				r.mode, r.from, r.fromRelease, r.commits = "range", c.Sha, c.Release, list
				r.notes = append(r.notes, skipped...)
				return r, nil
			case "identical", "behind":
				r.notes = append(r.notes, fmt.Sprintf("this commit is at or before the release %s (%s): a rollback or a repeat, so the commit alone is checked", short(c.Sha), c.Release))
				return r, nil
			default:
				skipped = append(skipped, fmt.Sprintf("%s (%s) is %s of %s, not an ancestor", short(c.Sha), c.Release, cmp.Status, short(to)))
			}
		case http.StatusNotFound, http.StatusUnprocessableEntity:
			skipped = append(skipped, fmt.Sprintf("%s (%s) is not a commit GitHub knows in this repository", short(c.Sha), c.Release))
		default:
			return nil, &unverifiable{fmt.Sprintf("GitHub answered %d to the compare of %s...%s: %s", st, short(c.Sha), short(to), hostMessage(body))}
		}
	}
	return nil, &unverifiable{fmt.Sprintf("cannot establish which commits this release carries: the gitops history shows earlier releases of this app but none can start the range (%s)", strings.Join(append(skipped, notes...), "; "))}
}

// listRange is the commits after from up to and including to, oldest first, read from GitHub's compare.
func (e *engine) listRange(ctx context.Context, from, to string) ([]rangeCommit, error) {
	var out []rangeCommit
	total := -1
	err := e.paged(ctx, fmt.Sprintf("compare/%s...%s", from, to), maxListPages, func(body []byte) (int, error) {
		var c struct {
			Status       string     `json:"status"`
			TotalCommits int        `json:"total_commits"`
			Commits      []ghCommit `json:"commits"`
		}
		if err := json.Unmarshal(body, &c); err != nil {
			return 0, err
		}
		if c.Status != "ahead" {
			return 0, fmt.Errorf("compare is %q, not ahead", c.Status)
		}
		total = c.TotalCommits
		if total > MaxRangeCommits {
			return 0, errRangeTooLong
		}
		for _, gc := range c.Commits {
			rc := rangeCommit{sha: gc.SHA, verified: gc.Commit.Verification.Verified, known: true}
			if gc.Committer != nil {
				rc.committer = gc.Committer.Login
			}
			for _, p := range gc.Parents {
				rc.parents = append(rc.parents, p.SHA)
			}
			out = append(out, rc)
		}
		return len(c.Commits), nil
	})
	if err != nil {
		if u, ok := err.(*unverifiable); ok && strings.Contains(u.why, errRangeTooLong.Error()) {
			return nil, &unverifiable{fmt.Sprintf("the range %s..%s has more than %d commits, which is more than one check verifies: release more often, or release an earlier commit first to shorten it", short(from), short(to), MaxRangeCommits)}
		}
		return nil, err
	}
	if len(out) != total || len(out) == 0 {
		return nil, &unverifiable{fmt.Sprintf("GitHub listed %d of the range's %d commits (%s..%s): not trusted", len(out), total, short(from), short(to))}
	}
	if out[len(out)-1].sha != to {
		return nil, &unverifiable{fmt.Sprintf("the range %s..%s does not end at %s", short(from), short(to), short(to))}
	}
	return out, nil
}

var errRangeTooLong = fmt.Errorf("range too long")

// ---- one commit's pull request and its evidence

// commitEvidence: does sha belong to a pull request merged into branch of this repository that has review evidence?
// idx/list (the range, oldest first) let a rebase-merged commit be matched to its pull request; both nil for a lone commit.
func (e *engine) commitEvidence(ctx context.Context, branch, sha string, idx map[string]int, list []rangeCommit) (CommitEvidence, error) {
	ce := CommitEvidence{Sha: sha}
	var pulls []ghPull
	err := e.paged(ctx, "commits/"+sha+"/pulls", maxListPages, func(body []byte) (int, error) {
		var page []ghPull
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, err
		}
		pulls = append(pulls, page...)
		return len(page), nil
	})
	if err != nil {
		return ce, err
	}
	var other []string
	reason := ""
	for _, p := range pulls {
		if p.MergedAt == nil || *p.MergedAt == "" {
			other = append(other, fmt.Sprintf("#%d (not merged)", p.Number))
			continue
		}
		if p.Base.Ref != branch || !e.sameRepo(p.Base.Repo) {
			other = append(other, fmt.Sprintf("#%d (merged into %s)", p.Number, e.baseName(p)))
			continue
		}
		st, err := e.pr(ctx, p.Number, branch)
		if err != nil {
			return ce, err
		}
		if st == nil {
			other = append(other, fmt.Sprintf("#%d (not merged into %s)", p.Number, branch))
			continue
		}
		in, why, err := e.member(ctx, st, sha, idx, list)
		if err != nil {
			return ce, err
		}
		if !in {
			if why == "" {
				why = "which does not contain it (it is not that pull request's merge commit, one of its commits, or a commit it rebased in)"
			}
			reason = fmt.Sprintf("GitHub associates it with #%d, %s", p.Number, why)
			continue
		}
		if st.Reviewed {
			ce.PullRequest, ce.Reviewed = p.Number, true
			return ce, nil
		}
		reason = fmt.Sprintf("in #%d, which has no usable approval: %s", p.Number, st.Missing)
		ce.PullRequest = p.Number
	}
	if reason == "" {
		if len(other) > 0 {
			reason = fmt.Sprintf("no pull request merged into %s contains it (only %s)", branch, strings.Join(other, ", "))
		} else {
			reason = fmt.Sprintf("no pull request: pushed to %s directly (a direct push, a force push or an administrator's bypass)", branch)
		}
	}
	ce.Reason = reason
	return ce, nil
}

// baseName is where a pull request was merged: the branch, with the repository when it is not this one.
func (e *engine) baseName(p ghPull) string {
	if p.Base.Repo != nil && p.Base.Repo.FullName != "" && !e.sameRepo(p.Base.Repo) {
		return p.Base.Repo.FullName + ":" + p.Base.Ref
	}
	return p.Base.Ref
}

func (e *engine) sameRepo(r *ghRepoRef) bool {
	return r != nil && strings.EqualFold(r.FullName, e.owner+"/"+e.repo)
}

// member: sha is one of the pull request's own commits, its merge commit, or a commit a rebase merge landed from it. GitHub
// associates it with the pull request, and:
//   - the approval covers the pull request's HEAD, so a merge commit that is not that head is accepted when GitHub made it:
//     committer web-flow with a verified signature (a merge commit or a squash). That signature already says GitHub made the
//     commit from the head through its merge button, so no content comparison is added for those. A merge pushed by hand has
//     neither.
//   - a rebase merge lands the pull request's commits as new, unsigned commits committed by the merger, so nothing GitHub signs
//     covers them. They count only as a run whose CONTENT is the pull request's: see rebaseRun.
func (e *engine) member(ctx context.Context, st *prState, sha string, idx map[string]int, list []rangeCommit) (bool, string, error) {
	if st.commitSet[sha] {
		return true, "", nil
	}
	info, err := e.info(ctx, st.MergeCommit, idx, list)
	if err != nil {
		return false, "", err
	}
	if strings.EqualFold(info.committer, "web-flow") && info.verified {
		// GitHub made the merge (or squash) commit: that is the pull request's merge commit, and nothing else of the pull request
		// is a landed commit.
		return sha == st.MergeCommit, "", nil
	}
	ok, why, err := e.rebaseRun(ctx, st)
	if err != nil {
		return false, "", err
	}
	if !ok {
		if sha != st.MergeCommit && why == "" {
			return false, "", nil
		}
		return false, why, nil
	}
	return st.run[sha], "", nil
}

type ghFile struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	SHA              string `json:"sha"`
	PreviousFilename string `json:"previous_filename"`
}

// rebaseRun decides, once per pull request, whether its merge commit and the commits before it are a rebase merge of the pull
// request: the run is exactly the pull request's commit count of single-parent commits ending at its merge commit (read as the
// branch history from it, one read), and what the run changed (compare from the commit before the run to its tip) is, file by
// file, the same as what the reviewed head changed (compare from that commit to the head: GitHub compares from the merge base):
// the same files, the same status, the same resulting blob. Anything else is not that pull request's landing, so its commits
// have no pull request. A rebase over a base that changed the same file produces a different blob and is refused (conservative:
// re-run the review, or merge with a merge commit or a squash). A file list GitHub may have cut at its cap is unverifiable.
func (e *engine) rebaseRun(ctx context.Context, st *prState) (bool, string, error) {
	if st.runDone {
		return st.runOK, st.runWhy, nil
	}
	ok, why, err := e.rebaseRunCompute(ctx, st)
	if err != nil {
		return false, "", err
	}
	st.runDone, st.runOK, st.runWhy = true, ok, why
	return ok, why, nil
}

func (e *engine) rebaseRunCompute(ctx context.Context, st *prState) (bool, string, error) {
	n := st.count
	if n < 1 || n > maxRebaseCommits {
		return false, "", &unverifiable{fmt.Sprintf("pull request #%d has %d commits: a rebase landing of more than %d commits is not read", st.Number, n, maxRebaseCommits)}
	}
	body, err := e.ok(ctx, fmt.Sprintf("commits?sha=%s&per_page=%d", st.MergeCommit, n+1))
	if err != nil {
		return false, "", err
	}
	var hist []ghCommit
	if err := json.Unmarshal(body, &hist); err != nil {
		return false, "", &unverifiable{fmt.Sprintf("GitHub's history of %s could not be read: %v", short(st.MergeCommit), err)}
	}
	notRun := fmt.Sprintf("whose merge commit was not made by GitHub (committer web-flow with a verified signature) and is not the tip of a rebase landing of it (a run of %d single-parent commits): a merge commit pushed by hand can carry changes nobody reviewed", n)
	if len(hist) < n+1 || hist[0].SHA != st.MergeCommit {
		return false, notRun, nil
	}
	st.run = map[string]bool{}
	for i := 0; i < n; i++ {
		if len(hist[i].Parents) != 1 || hist[i].Parents[0].SHA != hist[i+1].SHA {
			return false, notRun, nil
		}
		st.run[hist[i].SHA] = true
	}
	parent := hist[n].SHA
	if st.commitSet[parent] {
		return false, notRun, nil // a rebase rewrites the commits: the pull request's own are not on the branch under it
	}
	landed, err := e.compareFiles(ctx, parent, st.MergeCommit)
	if err != nil {
		return false, "", err
	}
	reviewed, err := e.compareFiles(ctx, parent, st.Head)
	if err != nil {
		return false, "", err
	}
	for k := range landed {
		if reviewed[k] != landed[k] {
			return false, e.differs(st, k), nil
		}
	}
	for k := range reviewed {
		if landed[k] != reviewed[k] {
			return false, e.differs(st, k), nil
		}
	}
	return true, "", nil
}

func (e *engine) differs(st *prState, file string) string {
	return fmt.Sprintf("%s (first difference: %s)", rebaseRefusal, file)
}

// rebaseRefusal is what a developer reads when a rebase merge's content is not the reviewed head's.
const rebaseRefusal = "rebase-merged pull request whose landed content differs from its reviewed head: re-run the review or use a merge commit or squash"

const maxRebaseCommits = 99

// compareFiles is the files of GET compare/{a}...{b} (changes of b since the merge base), each as filename, status, resulting blob
// and previous name. Paged to GitHub's cap of 300 files: a list that reaches it may be cut, which is unverifiable, never partial.
func (e *engine) compareFiles(ctx context.Context, a, b string) (map[string]string, error) {
	out := map[string]string{} // filename -> "status|blob|previous name"
	err := e.paged(ctx, fmt.Sprintf("compare/%s...%s", a, b), maxListPages, func(body []byte) (int, error) {
		var c struct {
			Files []ghFile `json:"files"`
		}
		if err := json.Unmarshal(body, &c); err != nil {
			return 0, err
		}
		for _, f := range c.Files {
			out[f.Filename] = f.Status + "|" + f.SHA + "|" + f.PreviousFilename
		}
		return len(c.Files), nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// info is the committer, signature verdict and parents of one commit: from the range list when it is there, else one read.
func (e *engine) info(ctx context.Context, sha string, idx map[string]int, list []rangeCommit) (*commitInfo, error) {
	if i, ok := idx[sha]; ok && list[i].known {
		return &commitInfo{committer: list[i].committer, verified: list[i].verified, parents: list[i].parents}, nil
	}
	if ci, ok := e.infos[sha]; ok {
		return ci, nil
	}
	body, err := e.ok(ctx, "commits/"+sha)
	if err != nil {
		return nil, err
	}
	var gc ghCommit
	if err := json.Unmarshal(body, &gc); err != nil {
		return nil, &unverifiable{fmt.Sprintf("GitHub's commit %s could not be read: %v", short(sha), err)}
	}
	ci := &commitInfo{verified: gc.Commit.Verification.Verified}
	if gc.Committer != nil {
		ci.committer = gc.Committer.Login
	}
	for _, p := range gc.Parents {
		ci.parents = append(ci.parents, p.SHA)
	}
	e.infos[sha] = ci
	return ci, nil
}

// pr reads one pull request once per check and evaluates its review evidence. It returns nil when the pull request, read in
// full, is not a pull request merged into branch of this repository (the association said so, the pull request does not).
func (e *engine) pr(ctx context.Context, n int, branch string) (*prState, error) {
	if st, ok := e.prs[n]; ok {
		return st, nil
	}
	body, err := e.ok(ctx, fmt.Sprintf("pulls/%d", n))
	if err != nil {
		return nil, err
	}
	var p ghPull
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, &unverifiable{fmt.Sprintf("GitHub's pull request #%d could not be read: %v", n, err)}
	}
	if p.MergedAt == nil || *p.MergedAt == "" || p.Base.Ref != branch || !e.sameRepo(p.Base.Repo) {
		return nil, nil
	}
	st := &prState{count: p.Commits, commitSet: map[string]bool{}}
	pe := &st.PullEvidence
	pe.Number, pe.URL, pe.MergedAt, pe.MergeCommit, pe.Head = p.Number, p.HTMLURL, *p.MergedAt, p.MergeCommitSHA, p.Head.SHA
	pe.Commits, pe.Authors, pe.Approvers = []string{}, []string{}, []string{}
	if p.User != nil {
		pe.Opener = p.User.Login
	}
	if p.MergedBy != nil {
		pe.Merger = p.MergedBy.Login
	}
	// Its commits, and who wrote them (linked accounts only: a commit's author name and email are the pusher's claim).
	var commits []ghCommit
	if err := e.paged(ctx, fmt.Sprintf("pulls/%d/commits", n), maxListPages, func(b []byte) (int, error) {
		var page []ghCommit
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		commits = append(commits, page...)
		return len(page), nil
	}); err != nil {
		return nil, err // GitHub did not show it in full: unverifiable, never "unreviewed"
	}
	excluded := map[string]bool{}
	if pe.Opener != "" {
		excluded[strings.ToLower(pe.Opener)] = true
	}
	authors := map[string]bool{}
	for _, gc := range commits {
		pe.Commits = append(pe.Commits, gc.SHA)
		st.commitSet[gc.SHA] = true
		if gc.Author != nil && gc.Author.Login != "" {
			excluded[strings.ToLower(gc.Author.Login)] = true
			authors[gc.Author.Login] = true
		} else {
			pe.Unlinked++
		}
		if gc.Committer != nil && gc.Committer.Login != "" {
			// GitHub itself commits what is done in its web editor and merge buttons: that account says nothing about who wrote it.
			if !strings.EqualFold(gc.Committer.Login, "web-flow") {
				excluded[strings.ToLower(gc.Committer.Login)] = true
				authors[gc.Committer.Login] = true
			}
		} else if gc.Commit.Committer.Name != gc.Commit.Author.Name {
			pe.Unlinked++ // someone else committed it, under an identity GitHub cannot link to an account
		}
	}
	pe.Authors = keysOf(authors)
	if pe.Unlinked > 0 {
		// Some author or committer is unknown, so an approval cannot be shown to come from someone else than them. The
		// conservative rule: the approver must also not be the account that merged it (the one account besides the opener
		// that could be that unknown person, and who acted on it).
		if pe.Merger != "" {
			excluded[strings.ToLower(pe.Merger)] = true
		}
	}
	switch {
	case st.count != len(commits):
		st.Missing = fmt.Sprintf("GitHub lists %d commits but the pull request has %d", len(commits), st.count)
	case !st.commitSet[pe.Head]:
		st.Missing = "its final head " + short(pe.Head) + " is not among its commits"
	case pe.Opener == "":
		st.Missing = "GitHub shows no opener (a deleted account)"
	}
	if st.Missing != "" {
		e.prs[n] = st
		return st, nil
	}
	// Its reviews.
	latest := map[string]ghReview{}
	var order []string
	if err := e.paged(ctx, fmt.Sprintf("pulls/%d/reviews", n), maxReviewPages, func(b []byte) (int, error) {
		var page []ghReview
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		for _, r := range page {
			if r.User == nil || r.User.Login == "" {
				continue
			}
			switch r.State {
			case "APPROVED", "CHANGES_REQUESTED", "DISMISSED": // a comment or a pending review changes nothing
				k := strings.ToLower(r.User.Login)
				if _, ok := latest[k]; !ok {
					order = append(order, k)
				}
				latest[k] = r
			}
		}
		return len(page), nil
	}); err != nil {
		return nil, err
	}
	var dismissed, stale, own, bots, changes, noWrite []string
	var cand []ghReview
	for _, k := range order {
		r := latest[k]
		login := r.User.Login
		switch {
		case r.State == "DISMISSED":
			dismissed = append(dismissed, login)
		case r.State == "CHANGES_REQUESTED":
			changes = append(changes, login)
		case r.CommitID != pe.Head:
			stale = append(stale, fmt.Sprintf("%s on %s", login, short(r.CommitID)))
		case e.isBot(r.User):
			bots = append(bots, login)
		case excluded[k]:
			own = append(own, login)
		default:
			cand = append(cand, r)
		}
	}
	// An approval from an account that cannot write to the repository is not a review: GitHub records one from anyone who can see
	// the pull request, and it does not count toward its own branch protection. Read each distinct approver's permission now.
	pe.ApproverPermissions = map[string]string{}
	for _, r := range cand {
		perm, err := e.permission(ctx, r)
		if err != nil {
			return nil, err
		}
		pe.ApproverPermissions[r.User.Login] = perm
		if writes(perm) {
			pe.Approvers = append(pe.Approvers, r.User.Login)
		} else {
			noWrite = append(noWrite, fmt.Sprintf("%s (%s)", r.User.Login, perm))
		}
	}
	sort.Strings(pe.Approvers)
	if len(pe.Approvers) > 0 {
		pe.Reviewed = true
		st.PullEvidence = *pe
		e.prs[n] = st
		return st, nil
	}
	var why []string
	if len(stale) > 0 {
		why = append(why, fmt.Sprintf("approved only on an older head (%s) but the final head is %s: it was pushed to after the approval", strings.Join(stale, ", "), short(pe.Head)))
	}
	if len(dismissed) > 0 {
		why = append(why, "the approval of "+strings.Join(dismissed, ", ")+" was dismissed")
	}
	if len(changes) > 0 {
		why = append(why, "changes requested by "+strings.Join(changes, ", ")+" and not since approved")
	}
	if len(own) > 0 {
		who := "the opener or an author or committer of its commits"
		if pe.Unlinked > 0 {
			who += " or the merger (some commits' authors are unknown to GitHub)"
		}
		why = append(why, fmt.Sprintf("approved only by %s, who is %s", strings.Join(own, ", "), who))
	}
	if len(noWrite) > 0 {
		why = append(why, "approved only by an account without write access to the repository ("+strings.Join(noWrite, ", ")+"): GitHub records an approval from anyone who can see a pull request, and it is not a review")
	}
	if len(bots) > 0 {
		why = append(why, "approved only by a bot ("+strings.Join(bots, ", ")+")")
	}
	if len(why) == 0 {
		why = append(why, "no approving review")
	}
	st.Missing = strings.Join(why, "; ")
	e.prs[n] = st
	return st, nil
}

// isBot: GitHub types the account Bot, or its login ends in [bot], or it is one of the pipeline's own configured logins.
func (e *engine) isBot(u *ghUser) bool {
	if u == nil {
		return false
	}
	l := strings.ToLower(u.Login)
	if u.Type == "Bot" || strings.HasSuffix(l, "[bot]") {
		return true
	}
	for _, b := range e.c.BotLogins {
		if strings.EqualFold(strings.TrimSpace(b), u.Login) {
			return true
		}
	}
	return false
}

// writes: the repository permissions that may review for the branch (GitHub's legacy names: maintain reads as write).
func writes(perm string) bool {
	return perm == "admin" || perm == "write" || perm == "maintain" || perm == "association:OWNER" || perm == "association:COLLABORATOR" || perm == "association:MEMBER"
}

// permission is the reviewer's permission on this repository now: GET collaborators/{login}/permission (documented to need
// Metadata: read). When GitHub says the integration may not read it (403), the review's own author_association is used instead
// and recorded as "association:<X>": OWNER, COLLABORATOR and MEMBER count. That is weaker: an organization MEMBER need not hold
// write on this repository. A failure of any other kind is unverifiable.
func (e *engine) permission(ctx context.Context, r ghReview) (string, error) {
	k := strings.ToLower(r.User.Login)
	if p, ok := e.perms[k]; ok {
		return p, nil
	}
	st, body, err := e.do(ctx, "collaborators/"+url.PathEscape(r.User.Login)+"/permission")
	if err != nil {
		return "", err
	}
	var perm string
	switch st {
	case http.StatusOK:
		var out struct {
			Permission string `json:"permission"`
			RoleName   string `json:"role_name"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", &unverifiable{fmt.Sprintf("GitHub's permission of %s could not be read: %v", r.User.Login, err)}
		}
		perm = out.Permission
		if out.RoleName == "maintain" || out.RoleName == "write" || out.RoleName == "admin" {
			perm = out.RoleName
		}
		if perm == "" {
			return "", &unverifiable{fmt.Sprintf("GitHub's permission of %s is empty", r.User.Login)}
		}
	case http.StatusNotFound:
		perm = "none" // not a user GitHub knows (deleted): no access
	case http.StatusForbidden:
		// "Resource not accessible by integration" (an App token without the permission) and "Must have push access to view
		// collaborator permission" (what GitHub answered a token without push access, recorded in testdata) both mean: not readable.
		if m := strings.ToLower(hostMessage(body)); !strings.Contains(m, "not accessible") && !strings.Contains(m, "push access") {
			return "", &unverifiable{fmt.Sprintf("GitHub answered 403 to the permission of %s: %s", r.User.Login, hostMessage(body))}
		}
		perm = "association:" + strings.ToUpper(r.AuthorAssociation)
		e.assocUsed = true
	default:
		return "", &unverifiable{fmt.Sprintf("GitHub answered %d to the permission of %s: %s", st, r.User.Login, hostMessage(body))}
	}
	e.perms[k] = perm
	return perm, nil
}
