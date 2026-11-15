package refprotection

// A small fake GitHub for the evidence tests: a main branch history, pull requests with their commits and reviews, and
// the commit-to-pull-request associations, served in the response shapes GitHub's REST API returns (the shapes in
// testdata/real_*.json are recorded from a public repository and checked by TestRecordedShapes).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
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
	edits        map[string]string // file -> blob sha this commit sets ("" removes it): its tree is its first parent's plus these
	unverified   bool              // GitHub does not show its signature as verified (web-flow commits are verified otherwise)
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
	t                    *testing.T
	main                 []string // first-parent history of the protected branch, oldest first
	commits              map[string]*wCommit
	prs                  map[int]*wPR
	assoc                map[string][]int // commit sha -> pull request numbers GitHub associates with it
	side                 map[string]bool  // commits that exist but are not on main
	override             map[string]resp  // path (without query) -> forced answer
	calls                []string
	errs                 map[string]error  // path (without query) -> transport error
	perms                map[string]string // reviewer login -> repository permission (default write)
	gql                  *resp             // forced answer of the GraphQL reviews read
	gqlErr               error
	gqlCalls             int
	gitops               []*gRelease // pull requests of gitops-app
	histEnv, histCluster string      // the target of the check
}

// gRelease is a pull request of the gitops repository gitops-app.
type gRelease struct {
	n          int
	title      string
	login      string
	mergedAt   string // "" = not merged
	commits    int    // commits of the pull request
	message    string
	files      []string // changed files
	tag        string   // the image tag the manifest sets as merged
	headSha    string
	events     []string // timeline events besides the one commit (default none)
	manyEvents int      // that many extra "labeled" events
	prHead     string   // the head the pull request reports, when it is not the one commit (a push after the commit listing)
	mergeSha   string
	// commitLogin is the account GitHub resolved the commit's author and committer to ("" = unlinked, as the platform's own are)
	commitLogin  string
	env, cluster string
}

func newWorld(t *testing.T) *world {
	return &world{t: t, commits: map[string]*wCommit{}, prs: map[int]*wPR{}, assoc: map[string][]int{}, side: map[string]bool{}, override: map[string]resp{}, errs: map[string]error{}, perms: map[string]string{}}
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

func (c *wCommit) verified() bool { return c.committer == "web-flow" && !c.unverified }

func (c *wCommit) json() string {
	var ps []string
	for _, p := range c.parents {
		ps = append(ps, fmt.Sprintf(`{"sha":%q}`, p))
	}
	return fmt.Sprintf(`{"sha":%q,"parents":[%s],"author":%s,"committer":%s,"commit":{"author":{"name":%q},"committer":{"name":%q},"verification":{"verified":%v}}}`,
		c.sha, strings.Join(ps, ","), user(c.author, ""), user(c.committer, ""), c.aname, c.cname, c.verified())
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

func (w *world) GetJSON(_ context.Context, _, repo, path string) (int, []byte, error) {
	if repo == "gitops-app" {
		return w.gitopsJSON(path)
	}
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
		var files []string
		for _, f := range w.diffFiles(a, b) {
			files = append(files, f)
		}
		body := fmt.Sprintf(`{"status":%q,"total_commits":%d,"ahead_by":%d,"commits":%s,"files":%s}`, status, total, total, page(list, q), page(files, q))
		return reply(body)
	case p == "commits":
		// the branch history from q.sha, newest first, following first parents (what the rebase landing is read from)
		n, _ := strconv.Atoi(q.Get("per_page"))
		var items []string
		for sha := q.Get("sha"); sha != "" && len(items) < n; {
			c, ok := w.commits[sha]
			if !ok {
				break
			}
			items = append(items, c.json())
			sha = ""
			if len(c.parents) > 0 {
				sha = c.parents[0]
			}
		}
		return reply("[" + strings.Join(items, ",") + "]")
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
	case strings.HasPrefix(p, "pulls/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(p, "pulls/"))
		pr, ok := w.prs[n]
		if !ok {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		return reply(pr.json())
	case strings.HasPrefix(p, "commits/") && !strings.Contains(strings.TrimPrefix(p, "commits/"), "/"):
		c, ok := w.commits[strings.TrimPrefix(p, "commits/")]
		if !ok {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		return reply(c.json())
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

// mustCheck checks `to` with the releases of cands (NEWEST FIRST) as the gitops repository's merged release pull requests.
func mustCheck(t *testing.T, c *Checker, w *world, ref, to string, cands []Candidate) *Verdict {
	t.Helper()
	w.setReleases(cands)
	v, err := c.Check(context.Background(), w, "o", "app", ref, to, withHistory(w))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return v
}

func j(v any) string { b, _ := json.Marshal(v); return string(b) }

// ---- the gitops repository's history

// release adds a merged release pull request for the app commit sha (each one merged after the one before): one commit, the
// pull request's head, with the trailers, one manifest hangar-dev/dev/release.yaml whose tag ends in the commit's sha.
func (w *world) release(sha string) *gRelease {
	n := 900 + len(w.gitops)
	g := &gRelease{n: n, title: fmt.Sprintf("Release: app to dev @ sha-%s", short(sha)), login: "pipeline[bot]", mergedAt: fmt.Sprintf("2026-11-%02dT10:00:00Z", 1+len(w.gitops)%28), commits: 1,
		message: fmt.Sprintf("Release: promote app to dev @ x:sha-%s\n\nX-App-Repo: https://github.com/o/app\nX-App-Commit: %s", short(sha), sha),
		tag:     "sha-" + short(sha), headSha: sh(7000 + n), mergeSha: sh(8000 + n), cluster: "hangar-dev", env: "dev"}
	g.files = []string{g.cluster + "/" + g.env + "/release.yaml"}
	w.gitops = append(w.gitops, g)
	return g
}

// setReleases registers the releases of cands, NEWEST FIRST (the order a person lists them), as merged gitops pull requests.
func (w *world) setReleases(cands []Candidate) {
	w.gitops = nil
	for i := len(cands) - 1; i >= 0; i-- { // oldest merged first
		g := w.release(cands[i].Sha)
		if cands[i].Env != "" {
			g.env = cands[i].Env
			g.files = []string{g.cluster + "/" + g.env + "/release.yaml"}
		}
	}
}

func (w *world) gitopsJSON(path string) (int, []byte, error) {
	w.calls = append(w.calls, "gitops:"+path)
	p, rawq, _ := strings.Cut(path, "?")
	q, _ := url.ParseQuery(rawq)
	if err, ok := w.errs["gitops:"+p]; ok {
		return 0, nil, err
	}
	if r, ok := w.override["gitops:"+p]; ok {
		return r.st, []byte(r.body), nil
	}
	find := func(n int) *gRelease {
		for _, g := range w.gitops {
			if g.n == n {
				return g
			}
		}
		return nil
	}
	userJSON := func(login string) string { return fmt.Sprintf(`{"login":%q,"type":"Bot"}`, login) }
	switch {
	case p == "pulls":
		var items []string
		for _, g := range w.gitops {
			m := "null"
			if g.mergedAt != "" {
				m = fmt.Sprintf("%q", g.mergedAt)
			}
			items = append(items, fmt.Sprintf(`{"number":%d,"title":%q,"merged_at":%s,"user":%s,"base":{"ref":"main","repo":{"default_branch":"main"}}}`, g.n, g.title, m, userJSON(g.login)))
		}
		return 200, []byte(page(items, q)), nil
	case strings.HasPrefix(p, "pulls/") && strings.HasSuffix(p, "/commits"):
		n, _ := strconv.Atoi(strings.Split(p, "/")[1])
		g := find(n)
		if g == nil {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		who := "null"
		if g.commitLogin != "" {
			who = userJSON(g.commitLogin)
		}
		var items []string
		for i := 0; i < g.commits; i++ {
			sha := g.headSha
			if i > 0 {
				sha = sh(7500 + i)
			}
			items = append(items, fmt.Sprintf(`{"sha":%q,"author":%s,"committer":%s,"commit":{"message":%q}}`, sha, who, who, g.message))
		}
		return 200, []byte("[" + strings.Join(items, ",") + "]"), nil
	case strings.HasPrefix(p, "issues/") && strings.HasSuffix(p, "/timeline"):
		n, _ := strconv.Atoi(strings.Split(p, "/")[1])
		g := find(n)
		if g == nil {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		items := []string{fmt.Sprintf(`{"event":"committed","sha":%q}`, g.headSha)}
		for _, ev := range g.events {
			items = append(items, fmt.Sprintf(`{"event":%q}`, ev))
		}
		for i := 0; i < g.manyEvents; i++ {
			items = append(items, `{"event":"labeled"}`)
		}
		return 200, []byte(page(items, q)), nil
	case strings.HasPrefix(p, "pulls/") && strings.HasSuffix(p, "/files"):
		n, _ := strconv.Atoi(strings.Split(p, "/")[1])
		g := find(n)
		if g == nil {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		var items []string
		for _, f := range g.files {
			items = append(items, fmt.Sprintf(`{"filename":%q,"status":"modified"}`, f))
		}
		return 200, []byte(page(items, q)), nil
	case strings.HasPrefix(p, "pulls/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(p, "pulls/"))
		g := find(n)
		if g == nil {
			return 404, []byte(`{"message":"Not Found"}`), nil
		}
		return 200, []byte(fmt.Sprintf(`{"number":%d,"merged":true,"merged_at":%q,"merge_commit_sha":%q,"user":%s,"head":{"sha":%q}}`, g.n, g.mergedAt, g.mergeSha, userJSON(g.login), firstNonEmpty(g.prHead, g.headSha))), nil
	case strings.HasPrefix(p, "contents/"):
		ref := q.Get("ref")
		for _, g := range w.gitops {
			if g.mergeSha == ref {
				yml := fmt.Sprintf("release:\n  image:\n    repository: x\n    tag: %s\n", g.tag)
				return 200, []byte(fmt.Sprintf(`{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(yml)))), nil
			}
		}
		return 404, []byte(`{"message":"Not Found"}`), nil
	}
	w.t.Fatalf("unexpected gitops GET %s", path)
	return 0, nil, nil
}

// withHistory is where the gitops history of the world is read from, for the target the world names (none: no single target).
func withHistory(w *world) *History {
	return &History{GitopsRepo: "gitops-app", App: "app", Env: w.histEnv, Cluster: w.histCluster}
}

// ---- trees and compare files

func (w *world) tree(sha string) map[string]string {
	out := map[string]string{}
	c, ok := w.commits[sha]
	if !ok {
		return out
	}
	if len(c.parents) > 0 {
		for k, v := range w.tree(c.parents[0]) {
			out[k] = v
		}
	}
	for k, v := range c.edits {
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func (w *world) ancestors(sha string) map[string]bool {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		if c, ok := w.commits[s]; ok {
			for _, p := range c.parents {
				walk(p)
			}
		}
	}
	walk(sha)
	return seen
}

// mergeBase is the nearest common ancestor of a and b along b's first parents ("" if none).
func (w *world) mergeBase(a, b string) string {
	in := w.ancestors(a)
	for s := b; s != ""; {
		if in[s] {
			return s
		}
		c, ok := w.commits[s]
		if !ok || len(c.parents) == 0 {
			return ""
		}
		s = c.parents[0]
	}
	return ""
}

// diffFiles is what compare a...b lists: b's tree against the merge base's, as GitHub's file objects.
func (w *world) diffFiles(a, b string) []string {
	base := w.tree(w.mergeBase(a, b))
	head := w.tree(b)
	var names []string
	for k := range head {
		names = append(names, k)
	}
	for k := range base {
		if _, ok := head[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var out []string
	for _, k := range names {
		switch {
		case head[k] == base[k]:
		case base[k] == "":
			out = append(out, fmt.Sprintf(`{"filename":%q,"status":"added","sha":%q}`, k, head[k]))
		case head[k] == "":
			out = append(out, fmt.Sprintf(`{"filename":%q,"status":"removed","sha":%q}`, k, base[k]))
		default:
			out = append(out, fmt.Sprintf(`{"filename":%q,"status":"modified","sha":%q}`, k, head[k]))
		}
	}
	return out
}

// GraphQL answers the reviews query the way GitHub does: the pull request's reviews oldest first, 100 to a page, with
// authorCanPushToRepository from the reviewer's permission (write when not set).
func (w *world) GraphQL(_ context.Context, _, _, query string, vars map[string]any) (int, []byte, error) {
	w.gqlCalls++
	w.calls = append(w.calls, "graphql")
	if w.gqlErr != nil {
		return 0, nil, w.gqlErr
	}
	if w.gql != nil {
		return w.gql.st, []byte(w.gql.body), nil
	}
	n := vars["n"].(int)
	pr, ok := w.prs[n]
	if !ok {
		return 200, []byte(`{"data":{"repository":{"pullRequest":null}}}`), nil
	}
	start := 0
	if after, ok := vars["after"].(string); ok {
		start, _ = strconv.Atoi(strings.TrimPrefix(after, "c"))
	}
	end := start + 100
	if end > len(pr.reviews) {
		end = len(pr.reviews)
	}
	var nodes []string
	for _, r := range pr.reviews[start:end] {
		author := "null"
		if r.user != "" {
			typ := r.typ
			if typ == "" {
				typ = "User"
			}
			author = fmt.Sprintf(`{"login":%q,"__typename":%q}`, r.user, typ)
		}
		perm, ok := w.perms[r.user]
		if !ok {
			perm = "write"
		}
		can := perm == "write" || perm == "maintain" || perm == "admin"
		nodes = append(nodes, fmt.Sprintf(`{"state":%q,"author":%s,"authorCanPushToRepository":%v,"commit":{"oid":%q}}`, r.state, author, can, r.commit))
	}
	return 200, []byte(fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"reviews":{"pageInfo":{"hasNextPage":%v,"endCursor":"c%d"},"nodes":[%s]}}}}}`, end < len(pr.reviews), end, strings.Join(nodes, ","))), nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
