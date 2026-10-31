// Package github is glidepath-api's GitHub reads, for promote and rollback (internal/environments): a file's
// contents, a commit, and the open PR from one branch. These are the calls Tower's githubApi.ts, releasePlan.ts and
// glidepathPromote.ts make, with the same URLs and the same error words.
//
// glidepath-api holds no GitHub credential. Each request opens a Session, and the Session asks the broker
// (cdevents-broker-auth's /github-read-token) for a token the first time it reads a repo: the broker checks the
// caller is glidepath-api's ServiceAccount and the repo is the app's own, and mints an installation token for that one
// repo with contents, pull_requests and metadata read only. The token lives in the Session, which lives for one
// request; it is never cached across requests, logged, returned or put in an error. The broker call is authenticated
// with this pod's projected ServiceAccount token (audience github-read-token), read from disk on every call, so a
// rotated token is picked up without a restart.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

const API = "https://api.github.com"

// Broker mints per-repo read tokens.
type Broker struct {
	URL       string // e.g. http://cdevents-broker-auth.platform-system.svc.cluster.local:8080
	TokenPath string // the projected ServiceAccount token, audience github-read-token
	HTTP      *http.Client
	API       string // GitHub's API base; "" = https://api.github.com (tests point it at a stub)
}

// Session is one request's GitHub reads for one app. Not for reuse across requests.
type Session struct {
	b      *Broker
	app    string
	mu     sync.Mutex
	tokens map[string]*mint // owner/repo -> its one mint, for this request only
}

// mint is one broker call; done closes when t/err are set, so a read that needs the token while a Prefetch is still
// minting it waits for that call instead of making a second.
type mint struct {
	done chan struct{}
	t    string
	err  error
}

// Session opens a request's reads for app.
func (b *Broker) Session(app string) *Session {
	return &Session{b: b, app: app, tokens: map[string]*mint{}}
}

// Prefetch starts minting the tokens for repos now, in the background, so a request whose later reads depend on
// earlier ones doesn't pay for each repo's mint in turn. A read already waiting on a failed mint gets its error; a later
// read mints again.
func (s *Session) Prefetch(ctx context.Context, owner string, repos ...string) {
	for _, r := range repos {
		go s.token(ctx, owner, r)
	}
}

func (b *Broker) api() string {
	if b.API != "" {
		return b.API
	}
	return API
}

// HTTPError is GitHub's (or the broker's) status, for callers that treat one status as an answer (a 404 cicd.yaml).
type HTTPError struct {
	Status int
	What   string
}

func (e HTTPError) Error() string { return e.What }

// token returns the read token for owner/repo, minting it on first use in this session.
func (s *Session) token(ctx context.Context, owner, repo string) (string, error) {
	key := owner + "/" + repo
	s.mu.Lock()
	m, ok := s.tokens[key]
	if !ok {
		m = &mint{done: make(chan struct{})}
		s.tokens[key] = m
	}
	s.mu.Unlock()
	if !ok {
		m.t, m.err = s.mint(ctx, owner, repo)
		if m.err != nil {
			// Not kept: the session's next read of this repo asks the broker again, as it did before Prefetch.
			s.mu.Lock()
			delete(s.tokens, key)
			s.mu.Unlock()
		}
		close(m.done)
	}
	select {
	case <-m.done:
		return m.t, m.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Session) mint(ctx context.Context, owner, repo string) (string, error) {
	key := owner + "/" + repo
	sa, err := os.ReadFile(s.b.TokenPath)
	if err != nil {
		return "", fmt.Errorf("reading glidepath-api's broker token: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"owner": owner, "repo": repo, "app": s.app})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.b.URL, "/")+"/github-read-token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(sa)))
	req.Header.Set("Content-Type", "application/json")
	res, err := s.b.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking the broker for a GitHub read token for %s: %w", key, err)
	}
	defer res.Body.Close()
	var out struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&out)
	if res.StatusCode != http.StatusOK || out.Token == "" {
		// The broker's error names the rule that refused; it never carries a token.
		return "", fmt.Errorf("the broker refused a GitHub read token for %s (%d): %s", key, res.StatusCode, out.Error)
	}
	return out.Token, nil
}

func (s *Session) get(ctx context.Context, owner, repo, u, accept string) (*http.Response, error) {
	t, err := s.token(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+t)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return s.b.HTTP.Do(req)
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, x := range parts {
		parts[i] = url.PathEscape(x)
	}
	return strings.Join(parts, "/")
}

// Contents is Tower's getContents: the file at path on the default branch, decoded. A non-2xx is an HTTPError with
// Tower's message ("GET contents <owner>/<repo>/<path> failed with <status>").
func (s *Session) Contents(ctx context.Context, owner, repo, path string) (string, error) {
	res, err := s.get(ctx, owner, repo, fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.b.api(), owner, repo, escapePath(path)), "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", HTTPError{res.StatusCode, fmt.Sprintf("GET contents %s/%s/%s failed with %d", owner, repo, path, res.StatusCode)}
	}
	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("GET contents %s/%s/%s: %w", owner, repo, path, err)
	}
	if body.Encoding != "base64" {
		return "", fmt.Errorf(`unexpected encoding "%s" for %s/%s/%s`, body.Encoding, owner, repo, path)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decoding %s/%s/%s: %w", owner, repo, path, err)
	}
	return string(raw), nil
}

// Raw is releasePlan.ts's fetchCicdYaml read: the file's raw bytes (Accept: application/vnd.github.raw+json). A 404
// is an HTTPError with Status 404, which that caller treats as "no file".
func (s *Session) Raw(ctx context.Context, owner, repo, path string) (string, error) {
	return s.RawAt(ctx, owner, repo, path, "")
}

// RawAt is Raw at a branch, tag or commit; "" is the default branch.
func (s *Session) RawAt(ctx context.Context, owner, repo, path, ref string) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.b.api(), owner, repo, escapePath(path))
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	res, err := s.get(ctx, owner, repo, u, "application/vnd.github.raw+json")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", HTTPError{res.StatusCode, fmt.Sprintf("GitHub contents lookup for %s/%s/%s failed with %d", owner, repo, path, res.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	return string(raw), err
}

// CommitSHA is Tower's resolveFullSha: the full sha of ref in owner/repo.
func (s *Session) CommitSHA(ctx context.Context, owner, repo, ref string) (string, error) {
	res, err := s.get(ctx, owner, repo, fmt.Sprintf("%s/repos/%s/%s/commits/%s", s.b.api(), owner, repo, url.PathEscape(ref)), "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", HTTPError{res.StatusCode, fmt.Sprintf("resolving commit %s in %s/%s failed with %d - the image tag's embedded sha may not exist in this repo's history", ref, owner, repo, res.StatusCode)}
	}
	var c struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&c); err != nil {
		return "", fmt.Errorf("resolving commit %s in %s/%s: %w", ref, owner, repo, err)
	}
	return c.SHA, nil
}

// OpenPR is Tower's findOpenPr: the first open PR from owner:branch, or "" when there is none.
func (s *Session) OpenPR(ctx context.Context, owner, repo, branch string) (string, error) {
	q := url.Values{"head": {owner + ":" + branch}, "state": {"open"}}
	res, err := s.get(ctx, owner, repo, fmt.Sprintf("%s/repos/%s/%s/pulls?%s", s.b.api(), owner, repo, q.Encode()), "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", HTTPError{res.StatusCode, fmt.Sprintf("PR search for %s/%s#%s failed with %d", owner, repo, branch, res.StatusCode)}
	}
	var prs []struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&prs); err != nil {
		return "", fmt.Errorf("PR search for %s/%s#%s: %w", owner, repo, branch, err)
	}
	if len(prs) == 0 {
		return "", nil
	}
	return prs[0].HTMLURL, nil
}
