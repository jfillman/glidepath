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
	"sort"
	"strings"
)

type engine struct {
	c           *Checker
	rd          Reader
	owner, repo string
	calls       int
	prs         map[int]*prState // pull requests read in THIS check, by number (never kept across requests)
}

type prState struct {
	PullEvidence
	commitSet map[string]bool
	count     int // the pull request's own commit count
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
	sha     string
	parents []string
}

type commitRange struct {
	mode        string
	from        string
	fromRelease string
	commits     []rangeCommit // oldest first, to inclusive
	notes       []string
}

type related struct {
	c       Candidate
	aheadBy int
}

// resolveRange decides which commits must carry evidence. The baseline (the last verified release) is a CLAIM: a release
// record is written by the pipeline's ServiceAccount, so a forged one could name a commit near the head and shrink the
// range. So it is never used to skip anything unless GitHub confirms that it is an ancestor of the commit on the protected
// branch and that it itself has review evidence. A candidate that fails either is ignored; when records claim a verified
// release and none of them can be used (and the commit is not at or before one), the range cannot be established and the
// answer is unverifiable, never a shorter range. With no claim at all it is a first release: the commit alone.
func (e *engine) resolveRange(ctx context.Context, branch, to string, cands []Candidate) (*commitRange, error) {
	seen := map[string]bool{}
	var use []Candidate
	for _, c := range cands {
		if !shaRE.MatchString(c.Sha) || seen[c.Sha] {
			continue
		}
		seen[c.Sha] = true
		if use = append(use, c); len(use) == maxCandidates {
			break
		}
	}
	r := &commitRange{mode: "first", commits: []rangeCommit{{sha: to}}}
	if len(use) == 0 {
		r.notes = append(r.notes, "no earlier verified release of this app is recorded: this is a first release, so the commit alone is checked")
		return r, nil
	}
	var ahead []related
	atOrBehind := false
	var skipped []string
	for _, c := range use {
		st, body, err := e.do(ctx, fmt.Sprintf("compare/%s...%s?per_page=1", c.Sha, to))
		if err != nil {
			return nil, err
		}
		switch st {
		case http.StatusOK:
			var cmp struct {
				Status  string `json:"status"`
				AheadBy int    `json:"ahead_by"`
			}
			if err := json.Unmarshal(body, &cmp); err != nil {
				return nil, &unverifiable{fmt.Sprintf("GitHub's compare of %s...%s could not be read: %v", short(c.Sha), short(to), err)}
			}
			switch cmp.Status {
			case "ahead":
				ahead = append(ahead, related{c, cmp.AheadBy})
			case "identical", "behind":
				atOrBehind = true
			default:
				skipped = append(skipped, fmt.Sprintf("%s is %s of %s, not an ancestor", short(c.Sha), cmp.Status, short(to)))
			}
		case http.StatusNotFound, http.StatusUnprocessableEntity:
			skipped = append(skipped, fmt.Sprintf("%s is not a commit GitHub knows in this repository", short(c.Sha)))
		default:
			return nil, &unverifiable{fmt.Sprintf("GitHub answered %d to the compare of %s...%s: %s", st, short(c.Sha), short(to), hostMessage(body))}
		}
	}
	sort.SliceStable(ahead, func(i, j int) bool { return ahead[i].aheadBy < ahead[j].aheadBy })
	for _, a := range ahead {
		ce, err := e.commitEvidence(ctx, branch, a.c.Sha, nil, nil)
		if err != nil {
			return nil, err
		}
		if !ce.Reviewed {
			skipped = append(skipped, fmt.Sprintf("%s (release %s) itself has no review evidence: %s", short(a.c.Sha), a.c.Release, ce.Reason))
			continue
		}
		// The ancestor is confirmed and evidenced: only now may it shorten the range.
		list, err := e.listRange(ctx, a.c.Sha, to)
		if err != nil {
			return nil, err
		}
		r.mode, r.from, r.fromRelease, r.commits = "range", a.c.Sha, a.c.Release, list
		r.notes = append(r.notes, skipped...)
		return r, nil
	}
	if atOrBehind {
		r.notes = append(r.notes, "this commit is at or before the last verified release (a rollback or a repeat): the commit alone is checked")
		r.notes = append(r.notes, skipped...)
		return r, nil
	}
	return nil, &unverifiable{fmt.Sprintf("cannot establish which commits this release carries: the release records claim a verified release, but none can be used as the start of the range (%s). A release record is not trusted to skip commits", strings.Join(skipped, "; "))}
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
			rc := rangeCommit{sha: gc.SHA}
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
		if !e.member(st, sha, idx, list) {
			reason = fmt.Sprintf("GitHub associates it with #%d, which does not contain it (it is not that pull request's merge commit, one of its commits, or a commit it rebased in)", p.Number)
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

// member: sha is the pull request's merge commit, one of its commits, or a commit a rebase merge landed from it: GitHub
// associates it with the pull request, it lies within the CONTIGUOUS single-parent run of at most (commit count) commits
// ending at the merge commit, and the merge commit itself has one parent. A squash lands one commit, a merge commit lands
// two parents and the branch's own commits, a rebase lands N linear commits: nothing else qualifies.
func (e *engine) member(st *prState, sha string, idx map[string]int, list []rangeCommit) bool {
	if sha == st.MergeCommit || st.commitSet[sha] {
		return true
	}
	if idx == nil {
		return false
	}
	j, okj := idx[st.MergeCommit]
	i, oki := idx[sha]
	if !okj || !oki || i >= j || j-i >= st.count || len(list[j].parents) != 1 {
		return false
	}
	for k := i; k < j; k++ {
		next := list[k+1]
		if len(next.parents) != 1 || next.parents[0] != list[k].sha {
			return false
		}
	}
	return len(list[i].parents) == 1
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
	var dismissed, stale, own, bots, changes []string
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
			pe.Approvers = append(pe.Approvers, login)
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
