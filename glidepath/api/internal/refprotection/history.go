package refprotection

// The baseline of a range comes from the app's GITOPS repository, not from the release records (ConfigMaps) the pipeline's
// ServiceAccount can write (and delete) until pipeline trust T4. A release is a merged pull request into the gitops
// repository's default branch that the platform's own GitHub App opened (a configured pipeline login), whose single commit
// carries the X-App-Repo and X-App-Commit trailers open-release-pr writes and whose one changed manifest sets an image tag
// that ends in that commit's sha. Writing that needs a pull request someone merged: the pipeline's token cannot fake one
// into the history, nor remove one.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// History says where an app's releases are recorded: the gitops repository, and the app (its release pull requests are
// titled "Release: <app> to <env> @ <tag>" or "Rollback: <app> <env> to <tag>").
type History struct {
	GitopsRepo string
	App        string
}

const (
	maxHistoryPages   = 3  // x100 closed pull requests of the gitops repository
	maxHistoryChecked = 12 // release pull requests (newest first) whose commit and manifest are read
)

var (
	trailerCommitRE = regexp.MustCompile(`(?m)^X-App-Commit: ([0-9a-f]{40})\s*$`)
	trailerRepoRE   = regexp.MustCompile(`(?m)^X-App-Repo: (\S+)\s*$`)
	manifestPathRE  = regexp.MustCompile(`^[^/]+/([^/]+)/(release|values)\.yaml$`)
	addedTagRE      = regexp.MustCompile(`(?m)^\+\s*tag:\s*"?([A-Za-z0-9._-]+)"?\s*(#.*)?$`)
	tagShaRE        = regexp.MustCompile(`([0-9a-f]{7,40})$`)
)

type ghHistoryPull struct {
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	MergedAt *string `json:"merged_at"`
	User     *ghUser `json:"user"`
	Base     struct {
		Ref  string `json:"ref"`
		Repo *struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repo"`
	} `json:"base"`
}

// doIn is do against another repository of the same owner (the gitops one), under the same budget.
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

// releases returns the corroborated releases of the app, newest merged first, and whether the gitops history shows ANY
// release pull request at all (so that "no baseline" can be told from "a first release"). A read that fails is an error:
// an unreadable history is never "no history".
func (e *engine) releases(ctx context.Context) (cands []Candidate, sawAny bool, notes []string, err error) {
	h := e.hist
	title := regexp.MustCompile(`^(?:Release: ` + regexp.QuoteMeta(h.App) + ` to ([^ ]+) @ |Rollback: ` + regexp.QuoteMeta(h.App) + ` ([^ ]+) to )`)
	var shaped []ghHistoryPull
	for page := 1; page <= maxHistoryPages; page++ {
		body, err := e.doIn(ctx, h.GitopsRepo, fmt.Sprintf("pulls?state=closed&per_page=100&page=%d", page))
		if err != nil {
			return nil, false, nil, err
		}
		var got []ghHistoryPull
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, false, nil, &unverifiable{fmt.Sprintf("GitHub's closed pull requests of %s could not be read: %v", h.GitopsRepo, err)}
		}
		for _, p := range got {
			if p.MergedAt == nil || *p.MergedAt == "" || p.Base.Repo == nil || p.Base.Ref != p.Base.Repo.DefaultBranch || !title.MatchString(p.Title) {
				continue
			}
			shaped = append(shaped, p)
		}
		if len(got) < 100 {
			break
		}
		if page == maxHistoryPages && len(shaped) == 0 {
			return nil, false, nil, &unverifiable{fmt.Sprintf("%s has more than %d closed pull requests and none of the newest is a release of %s: its release history is not read in full", h.GitopsRepo, maxHistoryPages*100, h.App)}
		}
	}
	if len(shaped) == 0 {
		return nil, false, nil, nil
	}
	sort.SliceStable(shaped, func(i, j int) bool { return *shaped[i].MergedAt > *shaped[j].MergedAt })
	var authored []ghHistoryPull
	for _, p := range shaped {
		if p.User != nil && e.isPipelineLogin(p.User.Login) {
			authored = append(authored, p)
		} else {
			who := "no author"
			if p.User != nil {
				who = p.User.Login
			}
			notes = append(notes, fmt.Sprintf("gitops #%d looks like a release but was opened by %s, which is not a configured pipeline login (api.pipelineLogins): ignored", p.Number, who))
		}
	}
	if len(authored) == 0 {
		return nil, true, notes, &unverifiable{fmt.Sprintf("%s shows %d merged release pull request(s) of %s but none was opened by a configured pipeline login (api.pipelineLogins is %q): no release can be corroborated", h.GitopsRepo, len(shaped), h.App, strings.Join(e.c.BotLogins, ","))}
	}
	for i, p := range authored {
		if i == maxHistoryChecked || len(cands) == maxCandidates {
			break
		}
		c, why, err := e.corroborate(ctx, p)
		if err != nil {
			return nil, true, notes, err
		}
		if c == nil {
			notes = append(notes, fmt.Sprintf("gitops #%d is not corroborated as a release (%s): ignored", p.Number, why))
			continue
		}
		cands = append(cands, *c)
	}
	return cands, true, notes, nil
}

// corroborate reads one release pull request: its single commit must name this repository and a commit in the trailers, and
// its one manifest must set an image tag ending in that commit's sha. nil, with why, when it does not.
func (e *engine) corroborate(ctx context.Context, p ghHistoryPull) (*Candidate, string, error) {
	h := e.hist
	body, err := e.doIn(ctx, h.GitopsRepo, fmt.Sprintf("pulls/%d/commits?per_page=100", p.Number))
	if err != nil {
		return nil, "", err
	}
	var commits []struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &commits); err != nil {
		return nil, "", &unverifiable{fmt.Sprintf("gitops #%d's commits could not be read: %v", p.Number, err)}
	}
	if len(commits) != 1 {
		return nil, fmt.Sprintf("%d commits, the pipeline writes one", len(commits)), nil
	}
	msg := commits[0].Commit.Message
	cm, rm := trailerCommitRE.FindStringSubmatch(msg), trailerRepoRE.FindStringSubmatch(msg)
	if cm == nil || rm == nil {
		return nil, "no X-App-Repo and X-App-Commit trailers", nil
	}
	repoURL := strings.TrimSuffix(strings.TrimSuffix(rm[1], "/"), ".git")
	if !strings.HasSuffix(strings.ToLower(repoURL), "/"+strings.ToLower(e.owner+"/"+e.repo)) {
		return nil, "its X-App-Repo is " + rm[1], nil
	}
	body, err = e.doIn(ctx, h.GitopsRepo, fmt.Sprintf("pulls/%d/files?per_page=100", p.Number))
	if err != nil {
		return nil, "", err
	}
	var files []struct {
		Filename string `json:"filename"`
		Patch    string `json:"patch"`
	}
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, "", &unverifiable{fmt.Sprintf("gitops #%d's files could not be read: %v", p.Number, err)}
	}
	if len(files) != 1 {
		return nil, fmt.Sprintf("%d changed files, the pipeline writes one manifest", len(files)), nil
	}
	mp := manifestPathRE.FindStringSubmatch(files[0].Filename)
	if mp == nil {
		return nil, "it changes " + files[0].Filename + ", not a release manifest", nil
	}
	ok := false
	for _, m := range addedTagRE.FindAllStringSubmatch(files[0].Patch, -1) {
		if t := tagShaRE.FindString(m[1]); t != "" && strings.HasPrefix(cm[1], t) {
			ok = true
		}
	}
	if !ok {
		return nil, "its manifest sets no image tag ending in the sha " + short(cm[1]), nil
	}
	return &Candidate{Sha: cm[1], Release: fmt.Sprintf("%s#%d", h.GitopsRepo, p.Number), Env: mp[1], At: *p.MergedAt}, "", nil
}

func (e *engine) isPipelineLogin(login string) bool {
	for _, b := range e.c.BotLogins {
		if strings.EqualFold(strings.TrimSpace(b), login) && login != "" {
			return true
		}
	}
	return false
}
