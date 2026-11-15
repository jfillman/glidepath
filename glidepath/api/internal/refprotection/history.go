package refprotection

// The baseline of a range comes from the app's GITOPS repository, not from the release records (ConfigMaps) the pipeline's
// ServiceAccount can write (and delete) until pipeline trust T4, and not from anything a person with write or triage rights on
// the gitops repository can edit afterwards (a pull request's title, its body, its labels). A release is identified by immutable
// facts read at its merge:
//
//   - the pull request was merged into the default branch and changed exactly one release manifest,
//     <cluster>/<env>/release.yaml or values.yaml, for the environment (and cluster) this check is for;
//   - GitHub shows one of the platform's own accounts (api.pipelineLogins) as the pull request's opener, and the pull request has
//     exactly one commit, its head, whose author and committer GitHub could not resolve to a person (the platform's release
//     commits carry the unlinked identity release@glidepath.invalid and are unsigned: read from a real one) or resolved to a
//     pipeline login;
//   - that commit's message carries X-App-Repo (this repository) and X-App-Commit trailers;
//   - the manifest AS MERGED sets an image tag that ends in that commit's sha.
//
// "The history shows a release" is decided from the pull requests' changed paths alone, whether or not the pull request passes
// the checks above, so retitling or re-describing a release pull request hides nothing.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// History says where an app's releases are recorded and which of them this check is for: the gitops repository, the app, and
// the environment and cluster the run is releasing to (empty: the run has no single target, and the strictest reading applies).
type History struct {
	GitopsRepo string
	App        string
	Env        string
	Cluster    string
}

const (
	maxHistoryPages   = 3  // x100 closed pull requests of the gitops repository
	maxHistoryScan    = 30 // merged pull requests (newest first) whose changed files are read
	maxHistoryChecked = 8  // pull requests that change a release manifest of the target (newest first) read in full
)

var (
	trailerCommitRE = regexp.MustCompile(`(?m)^X-App-Commit: ([0-9a-f]{40})\s*$`)
	trailerRepoRE   = regexp.MustCompile(`(?m)^X-App-Repo: (\S+)\s*$`)
	manifestPathRE  = regexp.MustCompile(`^([^/]+)/([^/]+)/(?:release|values)\.yaml$`)
	tagShaRE        = regexp.MustCompile(`([0-9a-f]{7,40})$`)
)

type ghHistoryPull struct {
	Number   int     `json:"number"`
	MergedAt *string `json:"merged_at"`
	User     *ghUser `json:"user"`
	Base     struct {
		Ref  string `json:"ref"`
		Repo *struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repo"`
	} `json:"base"`
	path, cluster, env string // the one release manifest it changes (read from its files)
}

// doIn is a read of another repository of the same owner (the gitops one), under the same budget.
func (e *engine) doIn(ctx context.Context, repo, path string) ([]byte, error) {
	if e.calls >= maxEvidenceCalls {
		return nil, &unverifiable{fmt.Sprintf("the evidence needs more than %d GitHub reads: the range or its pull requests are too large to verify in one check", maxEvidenceCalls)}
	}
	e.calls++
	st, body, err := e.rd.GetJSON(ctx, e.owner, repo, path)
	if err != nil {
		return nil, err
	}
	if st != 200 {
		p, _, _ := strings.Cut(path, "?")
		return nil, &unverifiable{fmt.Sprintf("GitHub answered %d to GET %s/%s/%s: %s", st, e.owner, repo, p, hostMessage(body))}
	}
	return body, nil
}

// releases returns the corroborated releases for the target, in the order a range tries them, and whether the gitops history
// shows ANY change of a release manifest for the target (so "no baseline" can be told from "a first release"). A read that
// fails is an error: an unreadable history is never "no history".
func (e *engine) releases(ctx context.Context) (cands []Candidate, sawAny bool, notes []string, err error) {
	h := e.hist
	var merged []ghHistoryPull
	exhausted := false
	for page := 1; page <= maxHistoryPages && !exhausted; page++ {
		body, err := e.doIn(ctx, h.GitopsRepo, fmt.Sprintf("pulls?state=closed&per_page=100&page=%d", page))
		if err != nil {
			return nil, false, nil, err
		}
		var got []ghHistoryPull
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, false, nil, &unverifiable{fmt.Sprintf("GitHub's closed pull requests of %s could not be read: %v", h.GitopsRepo, err)}
		}
		for _, p := range got {
			if p.MergedAt != nil && *p.MergedAt != "" && p.Base.Repo != nil && p.Base.Ref == p.Base.Repo.DefaultBranch {
				merged = append(merged, p)
			}
		}
		exhausted = len(got) < 100
	}
	sort.SliceStable(merged, func(i, j int) bool { return *merged[i].MergedAt > *merged[j].MergedAt })
	// Which merged pull requests changed a release manifest of the target: by changed path only.
	var touching []ghHistoryPull
	scanned := 0
	for _, p := range merged {
		if scanned == maxHistoryScan {
			break
		}
		scanned++
		files, err := e.prFiles(ctx, h.GitopsRepo, p.Number)
		if err != nil {
			return nil, false, nil, err
		}
		var mine []string
		for _, f := range files {
			if m := manifestPathRE.FindStringSubmatch(f); m != nil && (h.Cluster == "" || m[1] == h.Cluster) && (h.Env == "" || m[2] == h.Env) {
				mine = append(mine, f)
				p.path, p.cluster, p.env = f, m[1], m[2]
			}
		}
		if len(mine) == 0 {
			continue
		}
		if len(files) != 1 || len(mine) != 1 {
			// it changes a release manifest, so it IS release history, but it is not the one-manifest pull request the pipeline writes
			touching = append(touching, ghHistoryPull{Number: p.Number})
			notes = append(notes, fmt.Sprintf("gitops #%d changes a release manifest with %d other file(s): not corroborated as a release, kept as history", p.Number, len(files)-len(mine)))
			continue
		}
		touching = append(touching, p)
	}
	if len(touching) == 0 {
		if scanned < len(merged) || !exhausted {
			return nil, false, nil, &unverifiable{fmt.Sprintf("%s has more merged pull requests than the %d newest read and none of them changes a release manifest of %s%s: its release history is not read in full", h.GitopsRepo, maxHistoryScan, h.App, targetText(h))}
		}
		return nil, false, nil, nil
	}
	var good []Candidate
	for i, p := range touching {
		if i == maxHistoryChecked {
			break
		}
		if p.path == "" {
			continue
		}
		c, why, err := e.corroborate(ctx, p)
		if err != nil {
			return nil, true, notes, err
		}
		if c == nil {
			notes = append(notes, fmt.Sprintf("gitops #%d is not corroborated as a release (%s): ignored as a start, kept as history", p.Number, why))
			continue
		}
		good = append(good, *c)
	}
	if len(good) == 0 {
		return nil, true, notes, nil
	}
	if h.Env != "" {
		// one target: its newest release first, then older ones if that cannot start the range
		return capCandidates(good), true, notes, nil
	}
	// No single target: the strictest reading. The OLDEST of the environments' newest releases is tried first (the largest range),
	// then every other corroborated release, newest first.
	seen := map[string]bool{}
	var newest []Candidate
	for _, c := range good { // newest merged first
		if k := c.Cluster + "/" + c.Env; !seen[k] {
			seen[k] = true
			newest = append(newest, c)
		}
	}
	for i, j := 0, len(newest)-1; i < j; i, j = i+1, j-1 {
		newest[i], newest[j] = newest[j], newest[i]
	}
	order := append([]Candidate{}, newest...)
	for _, c := range good {
		dup := false
		for _, n := range newest {
			dup = dup || n.Sha == c.Sha && n.Release == c.Release
		}
		if !dup {
			order = append(order, c)
		}
	}
	notes = append(notes, "this run has no single target environment: the range starts at the oldest of the environments' newest releases")
	return capCandidates(order), true, notes, nil
}

func capCandidates(c []Candidate) []Candidate {
	if len(c) > maxCandidates {
		return c[:maxCandidates]
	}
	return c
}

func targetText(h *History) string {
	switch {
	case h.Env != "" && h.Cluster != "":
		return " for " + h.Cluster + "/" + h.Env
	case h.Env != "":
		return " for " + h.Env
	}
	return ""
}

func (e *engine) prFiles(ctx context.Context, repo string, n int) ([]string, error) {
	body, err := e.doIn(ctx, repo, fmt.Sprintf("pulls/%d/files?per_page=100", n))
	if err != nil {
		return nil, err
	}
	var files []struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, &unverifiable{fmt.Sprintf("gitops #%d's files could not be read: %v", n, err)}
	}
	var out []string
	for _, f := range files {
		out = append(out, f.Filename)
	}
	return out, nil
}

// corroborate reads one release pull request at its merge (see the package comment). nil, with why, when it does not hold.
func (e *engine) corroborate(ctx context.Context, p ghHistoryPull) (*Candidate, string, error) {
	h := e.hist
	body, err := e.doIn(ctx, h.GitopsRepo, fmt.Sprintf("pulls/%d", p.Number))
	if err != nil {
		return nil, "", err
	}
	var pr struct {
		MergeCommitSHA string  `json:"merge_commit_sha"`
		Merged         bool    `json:"merged"`
		MergedAt       *string `json:"merged_at"`
		User           *ghUser `json:"user"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, "", &unverifiable{fmt.Sprintf("gitops #%d could not be read: %v", p.Number, err)}
	}
	if pr.MergedAt == nil || *pr.MergedAt == "" || pr.MergeCommitSHA == "" {
		return nil, "it is not merged", nil
	}
	if pr.User == nil || !e.isPipelineLogin(pr.User.Login) {
		who := "no one"
		if pr.User != nil {
			who = pr.User.Login
		}
		return nil, "it was opened by " + who + ", which is not a configured pipeline login (api.pipelineLogins)", nil
	}
	body, err = e.doIn(ctx, h.GitopsRepo, fmt.Sprintf("pulls/%d/commits?per_page=100", p.Number))
	if err != nil {
		return nil, "", err
	}
	var commits []struct {
		SHA       string  `json:"sha"`
		Author    *ghUser `json:"author"`
		Committer *ghUser `json:"committer"`
		Commit    struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &commits); err != nil {
		return nil, "", &unverifiable{fmt.Sprintf("gitops #%d's commits could not be read: %v", p.Number, err)}
	}
	if len(commits) != 1 {
		return nil, fmt.Sprintf("%d commits, the pipeline writes one", len(commits)), nil
	}
	c := commits[0]
	if c.SHA != pr.Head.SHA {
		return nil, "its one commit is not its head", nil
	}
	for _, who := range []*ghUser{c.Author, c.Committer} {
		if who != nil && who.Login != "" && !e.isPipelineLogin(who.Login) {
			return nil, "its commit was authored or committed by " + who.Login + ", a person's account, not the pipeline's", nil
		}
	}
	// The pull request was not changed after it was opened. The pipeline opens it with one commit and nothing touches it again: a
	// force push to its head (the only way to replace that commit), a reopen or a restored head branch, or a commit added to it,
	// shows in its timeline. Anyone with write access to the gitops repository could do those; the pipeline never does.
	if why, err := e.timelineWhy(ctx, h.GitopsRepo, p.Number); err != nil || why != "" {
		return nil, why, err
	}
	cm, rm := trailerCommitRE.FindStringSubmatch(c.Commit.Message), trailerRepoRE.FindStringSubmatch(c.Commit.Message)
	if cm == nil || rm == nil {
		return nil, "its commit has no X-App-Repo and X-App-Commit trailers", nil
	}
	repoURL := strings.TrimSuffix(strings.TrimSuffix(rm[1], "/"), ".git")
	if !strings.HasSuffix(strings.ToLower(repoURL), "/"+strings.ToLower(e.owner+"/"+e.repo)) {
		return nil, "its X-App-Repo is " + rm[1], nil
	}
	// the manifest AS MERGED: the file at the merge commit, not the file now and not a patch someone could have rewritten
	body, err = e.doIn(ctx, h.GitopsRepo, "contents/"+escapeSegs(p.path)+"?ref="+pr.MergeCommitSHA)
	if err != nil {
		return nil, "", err
	}
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &file); err != nil || file.Encoding != "base64" {
		return nil, "", &unverifiable{fmt.Sprintf("gitops %s at %s could not be read as a file", p.path, short(pr.MergeCommitSHA))}
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return nil, "", &unverifiable{fmt.Sprintf("gitops %s at %s could not be decoded: %v", p.path, short(pr.MergeCommitSHA), err)}
	}
	tag := manifestTag(raw)
	if t := tagShaRE.FindString(tag); t == "" || !strings.HasPrefix(cm[1], t) {
		return nil, fmt.Sprintf("its manifest as merged sets the image tag %q, which does not end in the sha %s", tag, short(cm[1])), nil
	}
	return &Candidate{Sha: cm[1], Release: fmt.Sprintf("%s#%d", h.GitopsRepo, p.Number), Env: p.env, Cluster: p.cluster, At: *p.MergedAt}, "", nil
}

// manifestTag is release.image.tag, or rollout.image.tag, of a release manifest ("" when neither is set).
func manifestTag(raw []byte) string {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	for _, top := range []string{"release", "rollout"} {
		if m, ok := doc[top].(map[string]any); ok {
			if img, ok := m["image"].(map[string]any); ok {
				if t, ok := img["tag"].(string); ok && t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func (e *engine) isPipelineLogin(login string) bool {
	for _, b := range e.c.BotLogins {
		if strings.EqualFold(strings.TrimSpace(b), login) && login != "" {
			return true
		}
	}
	return false
}

// timelineWhy reads the pull request's timeline (REST issues/{n}/timeline: events head_ref_force_pushed, reopened,
// head_ref_restored, committed ... recorded in testdata) and says why it is not the untouched pull request the pipeline opened,
// or "". A timeline that cannot be read in full is unverifiable.
func (e *engine) timelineWhy(ctx context.Context, repo string, n int) (string, error) {
	committed := 0
	why := ""
	sep := fmt.Sprintf("issues/%d/timeline", n)
	for page := 1; page <= maxListPages; page++ {
		body, err := e.doIn(ctx, repo, fmt.Sprintf("%s?per_page=100&page=%d", sep, page))
		if err != nil {
			return "", err
		}
		var events []struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(body, &events); err != nil {
			return "", &unverifiable{fmt.Sprintf("gitops #%d's timeline could not be read: %v", n, err)}
		}
		for _, ev := range events {
			switch ev.Event {
			case "committed":
				committed++
			case "head_ref_force_pushed", "reopened", "head_ref_restored":
				if why == "" {
					why = "its timeline shows " + ev.Event + ": the pull request was changed after it was opened"
				}
			}
		}
		if len(events) < 100 {
			if why == "" && committed != 1 {
				why = fmt.Sprintf("its timeline shows %d commits, the pipeline opens it with one", committed)
			}
			return why, nil
		}
	}
	return "", &unverifiable{fmt.Sprintf("gitops #%d's timeline has more than %d events: not read in full", n, maxListPages*100)}
}
