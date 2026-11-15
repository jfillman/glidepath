package refprotection

// A small fake GitHub for the evidence tests: a main branch history, pull requests with their commits and reviews, and
// the commit-to-pull-request associations, served in the response shapes GitHub's REST API returns (the shapes in
// testdata/real_*.json are recorded from a public repository and checked by TestRecordedShapes).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// sh is a distinct 40-character sha whose first seven characters differ per n (the answer prints those).
func sh(n int) string { return fmt.Sprintf("%07x%033x", n, n) }

type wCommit struct {
	sha          string
	parents      []string
	author       string // "" = GitHub could not link the author's email to an account
	committer    string // "" = unlinked (see cname)
	aname, cname string
}

type wReview struct {
	user, typ, state, commit string
}

type wPR struct {
	n          int
	opener     string
	openerType string
	merger     string
	base       string // base branch (default main)
	baseRepo   string // default o/app
	headRepo   string // default o/app
	unmerged   bool
	mergeSha   string
	head       string
	commits    []string // the PR's own commits (original shas)
	count      int      // 0: len(commits)
	reviews    []wReview
}

type world struct {
	t        *testing.T
	main     []string // first-parent history of the protected branch, oldest first
	commits  map[string]*wCommit
	prs      map[int]*wPR
	assoc    map[string][]int // commit sha -> pull request numbers GitHub associates with it
	side     map[string]bool  // commits that exist but are not on main
	override map[string]resp  // path (without query) -> forced answer
	calls    []string
	errs     map[string]error // path (without query) -> transport error
}

func newWorld(t *testing.T) *world {
	return &world{t: t, commits: map[string]*wCommit{}, prs: map[int]*wPR{}, assoc: map[string][]int{}, side: map[string]bool{}, override: map[string]resp{}, errs: map[string]error{}}
}

// land adds commits to main in order (each the child of the one before).
func (w *world) land(cs ...*wCommit) {
	for _, c := range cs {
		if c.parents == nil && len(w.main) > 0 {
			c.parents = []string{w.main[len(w.main)-1]}
		}
		if c.aname == "" {
			c.aname = c.author
		}
		if c.cname == "" {
			c.cname = c.committer
		}
		w.commits[c.sha] = c
		w.main = append(w.main, c.sha)
	}
}

// prCommit makes a commit that lives on a pull request branch only (it is not on main).
func (w *world) prCommit(sha, author string) *wCommit {
	c := &wCommit{sha: sha, author: author, committer: author, aname: author, cname: author}
	w.commits[sha] = c
	w.side[sha] = true
	return c
}

func (w *world) pr(p *wPR) *wPR {
	if p.base == "" {
		p.base = "main"
	}
	if p.baseRepo == "" {
		p.baseRepo = "o/app"
	}
	if p.headRepo == "" {
		p.headRepo = "o/app"
	}
	if p.openerType == "" {
		p.openerType = "User"
	}
	if p.merger == "" {
		p.merger = p.opener
	}
	if p.head == "" && len(p.commits) > 0 {
		p.head = p.commits[len(p.commits)-1]
	}
	w.prs[p.n] = p
	return p
}

// associate says GitHub associates sha with pull request n.
func (w *world) associate(sha string, n ...int) { w.assoc[sha] = append(w.assoc[sha], n...) }

func user(login, typ string) string {
	if login == "" {
		return "null"
	}
	if typ == "" {
		typ = "User"
	}
	return fmt.Sprintf(`{"login":%q,"type":%q}`, login, typ)
}

func (p *wPR) json() string {
	merged := `null`
	if !p.unmerged {
		merged = `"2026-11-10T10:00:00Z"`
	}
	n := p.count
	if n == 0 {
		n = len(p.commits)
	}
	mb := `null`
	if !p.unmerged {
		mb = user(p.merger, "")
	}
	return fmt.Sprintf(`{"number":%d,"html_url":"https://github.com/o/app/pull/%d","state":"closed","merged_at":%s,"merge_commit_sha":%q,"commits":%d,"user":%s,"merged_by":%s,`+
		`"head":{"sha":%q,"ref":"feature","repo":{"full_name":%q}},"base":{"ref":%q,"repo":{"full_name":%q}}}`,
		p.n, p.n, merged, p.mergeSha, n, user(p.opener, p.openerType), mb, p.head, p.headRepo, p.base, p.baseRepo)
}

func (c *wCommit) json() string {
	var ps []string
	for _, p := range c.parents {
		ps = append(ps, fmt.Sprintf(`{"sha":%q}`, p))
	}
	return fmt.Sprintf(`{"sha":%q,"parents":[%s],"author":%s,"committer":%s,"commit":{"author":{"name":%q},"committer":{"name":%q}}}`,
		c.sha, strings.Join(ps, ","), user(c.author, ""), user(c.committer, ""), c.aname, c.cname)
}

func page(items []string, q url.Values) string {
	per, _ := strconv.Atoi(q.Get("per_page"))
	pg, _ := strconv.Atoi(q.Get("page"))
	if per == 0 {
		per = 30
	}
	if pg == 0 {
		pg = 1
	}
	lo := (pg - 1) * per
	if lo > len(items) {
		lo = len(items)
	}
	hi := lo + per
	if hi > len(items) {
		hi = len(items)
	}
	return "[" + strings.Join(items[lo:hi], ",") + "]"
}

func (w *world) index(sha string) int {
	for i, m := range w.main {
		if m == sha {
			return i
		}
	}
	return -1
}

func (w *world) GetJSON(_ context.Context, _, _, path string) (int, []byte, error) {
	w.calls = append(w.calls, path)
	p, rawq, _ := strings.Cut(path, "?")
	q, _ := url.ParseQuery(rawq)
	if err, ok := w.errs[p]; ok {
		return 0, nil, err
	}
	if r, ok := w.override[p]; ok {
		return r.st, []byte(r.body), nil
	}
	reply := func(body string) (int, []byte, error) { return 200, []byte(body), nil }
	switch {
	case strings.HasPrefix(p, "compare/"):
		a, b, _ := strings.Cut(strings.TrimPrefix(p, "compare/"), "...")
		if a == "main" {
			a = w.main[len(w.main)-1]
		}
		_, aok := w.commits[a]
		_, bok := w.commits[b]
		if !aok || !bok {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		ia, ib := w.index(a), w.index(b)
		status, list := "diverged", []string(nil)
		switch {
		case ia >= 0 && ib >= 0 && ia < ib:
			status = "ahead"
			for _, s := range w.main[ia+1 : ib+1] {
				list = append(list, w.commits[s].json())
			}
		case ia >= 0 && ia == ib:
			status = "identical"
		case ia >= 0 && ib >= 0:
			status = "behind"
		case ia >= 0 && ib < 0: // b is a side commit: diverged from main
			status = "diverged"
		}
		total := len(list)
		body := fmt.Sprintf(`{"status":%q,"total_commits":%d,"ahead_by":%d,"commits":%s}`, status, total, total, page(list, q))
		return reply(body)
	case strings.HasPrefix(p, "commits/") && strings.HasSuffix(p, "/pulls"):
		sha := strings.TrimSuffix(strings.TrimPrefix(p, "commits/"), "/pulls")
		if _, ok := w.commits[sha]; !ok {
			return 404, []byte(`{"message":"No commit found for SHA: ` + sha + `"}`), nil
		}
		var items []string
		for _, n := range w.assoc[sha] {
			items = append(items, w.prs[n].json())
		}
		return reply(page(items, q))
	case strings.HasPrefix(p, "pulls/") && strings.HasSuffix(p, "/commits"):
		n, _ := strconv.Atoi(strings.Split(p, "/")[1])
		pr, ok := w.prs[n]
		if !ok {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		var items []string
		for _, s := range pr.commits {
			items = append(items, w.commits[s].json())
		}
		return reply(page(items, q))
	case strings.HasPrefix(p, "pulls/") && strings.HasSuffix(p, "/reviews"):
		n, _ := strconv.Atoi(strings.Split(p, "/")[1])
		pr, ok := w.prs[n]
		if !ok {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		var items []string
		for i, r := range pr.reviews {
			items = append(items, fmt.Sprintf(`{"id":%d,"user":%s,"state":%q,"commit_id":%q,"submitted_at":"2026-11-0%dT10:00:00Z"}`, i+1, user(r.user, r.typ), r.state, r.commit, 1+i%9))
		}
		return reply(page(items, q))
	case strings.HasPrefix(p, "pulls/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(p, "pulls/"))
		pr, ok := w.prs[n]
		if !ok {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		return reply(pr.json())
	case strings.HasPrefix(p, "rules/branches/"):
		return reply(`[]`)
	case strings.HasPrefix(p, "branches/") && strings.HasSuffix(p, "/protection"):
		return 404, []byte(notProt404), nil
	case strings.HasPrefix(p, "rulesets"):
		return reply(`[]`)
	}
	w.t.Fatalf("unexpected GET %s", path)
	return 0, nil, nil
}

func (w *world) count(sub string) int {
	n := 0
	for _, c := range w.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// approved is the usual review: someone approves the head.
func approved(user, head string) []wReview {
	return []wReview{{user: user, state: "APPROVED", commit: head}}
}

// squash lands one commit for a pull request: the commit is its merge commit, and its own commits stay on the branch.
func (w *world) squash(n int, sha, opener, approver string, prShas ...int) *wPR {
	var own []string
	for _, x := range prShas {
		s := sh(x)
		w.prCommit(s, opener)
		own = append(own, s)
	}
	if len(own) == 0 {
		s := sh(100000 + n)
		w.prCommit(s, opener)
		own = []string{s}
	}
	w.land(&wCommit{sha: sha, author: opener, committer: "web-flow", aname: opener, cname: "GitHub"})
	p := w.pr(&wPR{n: n, opener: opener, mergeSha: sha, commits: own})
	p.reviews = approved(approver, p.head)
	w.associate(sha, n)
	return p
}

func mustCheck(t *testing.T, c *Checker, w *world, ref, to string, cands []Candidate) *Verdict {
	t.Helper()
	v, err := c.Check(context.Background(), w, "o", "app", ref, to, cands)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return v
}

func j(v any) string { b, _ := json.Marshal(v); return string(b) }
