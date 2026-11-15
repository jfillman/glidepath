package refprotection

import (
	"context"
	"strings"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/environments"
)

// Client is what the API route calls: it ties a request to the app's own repository before asking the host.
type Client struct {
	Owner   string // the GitHub owner of every app repo (the chart's githubOwner)
	Checker *Checker
	// GitHub opens one request's reads for the app (github.Broker.Session): one token minted for this call, never kept.
	GitHub func(app string) Reader
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
	return c.Checker.Check(ctx, c.GitHub(app.Name), c.Owner, name, ref, sha)
}
