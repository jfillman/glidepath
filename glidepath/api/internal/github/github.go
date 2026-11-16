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
	"strconv"
	"strings"
	"sync"
	"time"
)

const API = "https://api.github.com"

// Broker mints per-repo read tokens.
type Broker struct {
	URL       string // e.g. http://cdevents-broker-auth.platform-system.svc.cluster.local:8080
	TokenPath string // the projected ServiceAccount token, audience github-read-token
	HTTP      *http.Client
	API       string // GitHub's API base; "" = https://api.github.com (tests point it at a stub)
	// Sleep waits out a rate limit (nil: time.Sleep, cut short by the request's context).
	Sleep func(time.Duration)
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

// graphqlURL is GitHub's GraphQL endpoint for the configured API base: <base>/graphql on github.com, and on GitHub Enterprise
// Server (REST base ending in /api/v3) /api/graphql.
func (b *Broker) graphqlURL() string {
	base := strings.TrimRight(b.api(), "/")
	if strings.HasSuffix(base, "/api/v3") {
		return strings.TrimSuffix(base, "/api/v3") + "/api/graphql"
	}
	return base + "/graphql"
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
	reqBody := map[string]string{"owner": owner, "repo": repo, "app": s.app}
	body, _ := json.Marshal(reqBody)
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

// RateLimited is GitHub's 429, or its primary or secondary rate limit as a 403, still in force after the bounded
// retries. It is an error (the caller answers 502), never a verdict.
type RateLimited struct {
	Wait time.Duration // what GitHub asked us to wait, 0 if it did not say
}

func (e RateLimited) Error() string {
	w := "a while"
	if e.Wait > 0 {
		w = e.Wait.Round(time.Second).String()
	}
	return fmt.Sprintf("GitHub is rate limiting glidepath-api's reads (primary or secondary rate limit) and still was after %d tries; retry in %s", maxRateAttempts, w)
}

const (
	maxRateAttempts = 3
	maxRateWait     = 10 * time.Second // the longest single wait; a longer ask is not waited for here
)

// rateLimitWait reports whether a response is a rate limit and how long GitHub asked to wait (Retry-After, else
// x-ratelimit-reset when the quota is exhausted).
func rateLimitWait(res *http.Response, body []byte, now time.Time) (bool, time.Duration) {
	limited := res.StatusCode == http.StatusTooManyRequests
	if res.StatusCode == http.StatusForbidden {
		m := strings.ToLower(string(body))
		limited = strings.Contains(m, "rate limit") || res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != ""
	}
	if !limited {
		return false, 0
	}
	if v := res.Header.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return true, time.Duration(n) * time.Second
		}
	}
	if v := res.Header.Get("X-RateLimit-Reset"); v != "" && res.Header.Get("X-RateLimit-Remaining") == "0" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := time.Unix(n, 0).Sub(now); d > 0 {
				return true, d
			}
		}
	}
	return true, 0
}

func (s *Session) sleep(ctx context.Context, d time.Duration) error {
	if s.b.Sleep != nil {
		s.b.Sleep(d)
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetJSON is one bounded GET of a repo-relative API path (with its query), for callers that read the status itself
// (internal/refprotection: a 403 "upgrade" and a 404 "not protected" are answers, not failures). At most 4 MiB of the
// body is returned. A transport failure or a broker refusal is an error; any HTTP status is not, except a rate limit
// (429, or a 403 that says so): that is retried up to maxRateAttempts times, waiting what GitHub asked (Retry-After,
// x-ratelimit-reset) but never more than maxRateWait at once, then returned as RateLimited.
func (s *Session) GetJSON(ctx context.Context, owner, repo, path string) (int, []byte, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/%s", s.b.api(), owner, repo, strings.TrimLeft(path, "/"))
	var last time.Duration
	for attempt := 1; ; attempt++ {
		res, err := s.get(ctx, owner, repo, u, "application/vnd.github+json")
		if err != nil {
			return 0, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if err != nil {
			return res.StatusCode, body, err
		}
		limited, wait := rateLimitWait(res, body, time.Now())
		if !limited {
			return res.StatusCode, body, nil
		}
		last = wait
		if attempt >= maxRateAttempts || wait > maxRateWait {
			return res.StatusCode, nil, RateLimited{Wait: last}
		}
		if wait <= 0 {
			wait = time.Duration(attempt) * time.Second // no hint: back off 1s, 2s
		}
		if err := s.sleep(ctx, min(wait, maxRateWait)); err != nil {
			return 0, nil, err
		}
	}
}

// GraphQL is one POST to GitHub's GraphQL endpoint with the token minted for owner/repo (read-only: the query is a read), for the
// few facts REST does not carry (internal/refprotection: whether a review's author has push access). The status and body are
// returned like GetJSON's, with the same bounded rate-limit retries; a 200 body with an `errors` list is the caller's to refuse.
func (s *Session) GraphQL(ctx context.Context, owner, repo, query string, vars map[string]any) (int, []byte, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return 0, nil, err
	}
	var last time.Duration
	for attempt := 1; ; attempt++ {
		t, err := s.token(ctx, owner, repo)
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.b.graphqlURL(), bytes.NewReader(payload))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+t)
		res, err := s.b.HTTP.Do(req)
		if err != nil {
			return 0, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if err != nil {
			return res.StatusCode, body, err
		}
		limited, wait := rateLimitWait(res, body, time.Now())
		if !limited {
			return res.StatusCode, body, nil
		}
		last = wait
		if attempt >= maxRateAttempts || wait > maxRateWait {
			return res.StatusCode, nil, RateLimited{Wait: last}
		}
		if wait <= 0 {
			wait = time.Duration(attempt) * time.Second
		}
		if err := s.sleep(ctx, min(wait, maxRateWait)); err != nil {
			return 0, nil, err
		}
	}
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

// PullRequest is the facts of one pull request a reissue reads: whether it is open, whether it merged, and the branch it
// was opened from. Everything comes from the git host, never from the caller.
type PullRequest struct {
	Number  int
	State   string // open or closed
	Merged  bool
	HeadRef string
	URL     string
}

// PullRequest reads pull request number in owner/repo (GET pulls/{n}).
func (s *Session) PullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	res, err := s.get(ctx, owner, repo, fmt.Sprintf("%s/repos/%s/%s/pulls/%d", s.b.api(), owner, repo, number), "application/vnd.github+json")
	if err != nil {
		return PullRequest{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return PullRequest{}, HTTPError{res.StatusCode, fmt.Sprintf("reading pull request %s/%s#%d failed with %d", owner, repo, number, res.StatusCode)}
	}
	var pr struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&pr); err != nil {
		return PullRequest{}, fmt.Errorf("reading pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	return PullRequest{Number: pr.Number, State: pr.State, Merged: pr.Merged, HeadRef: pr.Head.Ref, URL: pr.HTMLURL}, nil
}

// OpenPRsFrom lists the open pull requests of owner/repo whose head branch starts with one of prefixes (the newest 100
// open ones: a gitops repo has a handful). Used to refuse a reissue while another release PR for the environment is open.
func (s *Session) OpenPRsFrom(ctx context.Context, owner, repo string, prefixes ...string) ([]PullRequest, error) {
	q := url.Values{"state": {"open"}, "per_page": {"100"}}
	res, err := s.get(ctx, owner, repo, fmt.Sprintf("%s/repos/%s/%s/pulls?%s", s.b.api(), owner, repo, q.Encode()), "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, HTTPError{res.StatusCode, fmt.Sprintf("listing open pull requests of %s/%s failed with %d", owner, repo, res.StatusCode)}
	}
	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&prs); err != nil {
		return nil, fmt.Errorf("listing open pull requests of %s/%s: %w", owner, repo, err)
	}
	out := []PullRequest{}
	for _, p := range prs {
		for _, pre := range prefixes {
			if strings.HasPrefix(p.Head.Ref, pre) {
				out = append(out, PullRequest{Number: p.Number, State: "open", HeadRef: p.Head.Ref, URL: p.HTMLURL})
				break
			}
		}
	}
	return out, nil
}
