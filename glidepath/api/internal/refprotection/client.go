package refprotection

import (
	"context"
	"sort"
	"strings"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/environments"
	"github.com/glidepath/api/internal/releases"
)

// Client is what the API route calls: it ties a request to the app's own repository before asking the host.
type Client struct {
	Owner   string // the GitHub owner of every app repo (the chart's githubOwner)
	Checker *Checker
	// GitHub opens one request's reads for the app (github.Broker.Session): one token minted for this call, never kept.
	GitHub func(app string) Reader
	// Records lists the app's release records (the release-tracking ConfigMaps). nil: no release is known, so every check is
	// a first release (the commit alone).
	Records func(ctx context.Context, app *apps.App) ([]releases.Record, error)
}

// Verify answers for repo ("<owner>/<name>", as Pipelines-as-Code recorded the run's repository) at ref and sha. The
// repository must be one of the app's own (its source repository, its gitops repository, or where its cicd.yaml
// lives) under the configured owner: the endpoint answers nothing about any other repository.
func (c *Client) Verify(ctx context.Context, app *apps.App, repo, ref, sha string) (*Verdict, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return nil, Invalid{"repo must be <owner>/<name>"}
	}
	if c.Owner == "" || !strings.EqualFold(owner, c.Owner) {
		return nil, Invalid{"repo " + repo + " is not under the owner this platform's apps live in"}
	}
	r := environments.ReposOf(app)
	if name != r.Source && name != r.Gitops && name != r.Config {
		return nil, Invalid{"repo " + repo + " is not one of " + app.Name + "'s repositories (" + r.Source + ", " + r.Gitops + ")"}
	}
	var cands []Candidate
	if c.Records != nil {
		recs, err := c.Records(ctx, app)
		if err != nil {
			return nil, err // the baseline cannot be read: no answer (fail closed)
		}
		cands = Candidates(recs, owner, name)
	}
	return c.Checker.Check(ctx, c.GitHub(app.Name), owner, name, ref, sha, cands)
}

// Candidates are the releases of owner/repo whose record says review evidence was verified (the guard's summary, written
// into refProtection), newest first. They are claims: Check verifies the one it uses. A record that names another
// repository, no commit, or never merged, or was not verified (an "Off here" release, a release before review evidence existed) is not a
// candidate.
func Candidates(recs []releases.Record, owner, repo string) []Candidate {
	var out []Candidate
	for _, rec := range recs {
		d := rec.Data
		rev := d["gitRevision"]
		if !shaRE.MatchString(rev) || !strings.Contains(d["refProtection"], VerifiedMarker) {
			continue
		}
		u := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(d["gitUrl"]), "/"), ".git")
		if !strings.HasSuffix(strings.ToLower(u), "/"+strings.ToLower(owner+"/"+repo)) {
			continue
		}
		at := d["mergedAt"]
		if at == "" {
			continue // its release PR was never merged: nothing was released
		}
		out = append(out, Candidate{Sha: rev, Release: rec.Name, Env: d["env"], At: at})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}
