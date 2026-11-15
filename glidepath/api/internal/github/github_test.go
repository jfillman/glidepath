package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A session mints once per repo and sends the broker exactly {owner, repo, app} with the pod's projected token, read
// from disk on every mint (so a rotated token is used without a restart). A new session mints again.
func TestSessionMintsPerRequestPerRepo(t *testing.T) {
	var mints []map[string]string
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github-read-token" {
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			mints = append(mints, req)
			auths = append(auths, r.Header.Get("Authorization"))
			json.NewEncoder(w).Encode(map[string]string{"token": "ghs-" + req["repo"]})
			return
		}
		if r.Header.Get("Authorization") != "Bearer ghs-gitops-shop" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `[{"html_url":"https://github.com/o/gitops-shop/pull/1"},{"html_url":"second"}]`)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa-1\n"), 0o600)
	b := &Broker{URL: srv.URL, TokenPath: tok, HTTP: srv.Client(), API: srv.URL}

	s := b.Session("shop")
	for i := 0; i < 2; i++ {
		if u, err := s.OpenPR(context.Background(), "o", "gitops-shop", "release-prod-abc"); err != nil || u != "https://github.com/o/gitops-shop/pull/1" {
			t.Fatalf("OpenPR: %q %v", u, err)
		}
	}
	os.WriteFile(tok, []byte("sa-2\n"), 0o600)
	if _, err := b.Session("shop").OpenPR(context.Background(), "o", "gitops-shop", "b"); err != nil {
		t.Fatal(err)
	}
	if len(mints) != 2 || mints[0]["owner"] != "o" || mints[0]["repo"] != "gitops-shop" || mints[0]["app"] != "shop" || len(mints[0]) != 3 {
		t.Fatalf("mints: %v", mints)
	}
	if auths[0] != "Bearer sa-1" || auths[1] != "Bearer sa-2" {
		t.Fatalf("the projected token is read per mint: %v", auths)
	}
}

// A broker refusal is an error with the broker's reason, and a GitHub failure one with Tower's words; neither carries
// a token.
func TestErrorsCarryNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github-read-token" {
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			if req["repo"] == "gitops-hangar-dev" {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"error":"gitops-hangar-dev is not shop's own repo"}`)
				return
			}
			io.WriteString(w, `{"token":"ghs-secret"}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"boom ghs-secret"}`)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa"), 0o600)
	s := (&Broker{URL: srv.URL, TokenPath: tok, HTTP: srv.Client(), API: srv.URL}).Session("shop")
	_, err := s.Contents(context.Background(), "o", "gitops-hangar-dev", "x")
	if err == nil || !strings.Contains(err.Error(), "not shop's own repo") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("refusal: %v", err)
	}
	_, err = s.Contents(context.Background(), "o", "shop", "glidepath/envs/dev.yaml")
	if err == nil || err.Error() != "GET contents o/shop/glidepath/envs/dev.yaml failed with 500" {
		t.Fatalf("GitHub failure: %v", err)
	}
	if _, err := s.CommitSHA(context.Background(), "o", "shop", "abc1234"); err == nil || strings.Contains(err.Error(), "ghs-") {
		t.Fatalf("commit: %v", err)
	}
}

// Prefetch mints every repo at once, and a read that needs a token still being prefetched waits for that mint rather
// than asking the broker again.
func TestPrefetchMintsConcurrentlyOnce(t *testing.T) {
	var mu sync.Mutex
	mints := map[string]int{}
	inflight, maxInflight := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github-read-token" {
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			mints[req["repo"]]++
			inflight++
			if inflight > maxInflight {
				maxInflight = inflight
			}
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			inflight--
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"token": "ghs-" + req["repo"]})
			return
		}
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa"), 0o600)
	s := (&Broker{URL: srv.URL, TokenPath: tok, HTTP: srv.Client(), API: srv.URL}).Session("shop")

	s.Prefetch(context.Background(), "o", "shop", "gitops-shop")
	for _, repo := range []string{"gitops-shop", "shop"} {
		if _, err := s.OpenPR(context.Background(), "o", repo, "b"); err != nil {
			t.Fatal(err)
		}
	}
	if mints["shop"] != 1 || mints["gitops-shop"] != 1 || maxInflight != 2 {
		t.Fatalf("mints %v, at most %d at once", mints, maxInflight)
	}
}

// A failed mint is not kept: the session's next read of that repo asks the broker again.
func TestFailedMintIsRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github-read-token" {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusBadGateway)
				io.WriteString(w, `{"error":"GitHub installation lookup failed"}`)
				return
			}
			io.WriteString(w, `{"token":"ghs-shop"}`)
			return
		}
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa"), 0o600)
	s := (&Broker{URL: srv.URL, TokenPath: tok, HTTP: srv.Client(), API: srv.URL}).Session("shop")
	if _, err := s.OpenPR(context.Background(), "o", "shop", "b"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("first: %v", err)
	}
	if _, err := s.OpenPR(context.Background(), "o", "shop", "b"); err != nil || calls != 2 {
		t.Fatalf("second: %v after %d mints", err, calls)
	}
}

// A session asks the broker for the plain read-only token: the request carries no purpose and no permission list (the
// elevated ref-protection purpose is gone: review evidence needs contents, pull requests and metadata read only).
func TestSessionAsksForNoPurposeAndNoPermissions(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github-read-token" {
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			bodies = append(bodies, req)
			json.NewEncoder(w).Encode(map[string]string{"token": "ghs-x"})
			return
		}
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa\n"), 0o600)
	b := &Broker{URL: srv.URL, TokenPath: tok, HTTP: srv.Client(), API: srv.URL}
	if st, _, err := b.Session("shop").GetJSON(context.Background(), "o", "shop", "pulls/1/reviews"); err != nil || st != 200 {
		t.Fatalf("%d %v", st, err)
	}
	if len(bodies) != 1 {
		t.Fatalf("%v", bodies)
	}
	if len(bodies[0]) != 3 || bodies[0]["owner"] != "o" || bodies[0]["repo"] != "shop" || bodies[0]["app"] != "shop" {
		t.Fatalf("the request names the repository and the app and nothing else: %v", bodies[0])
	}
}

func rateStub(t *testing.T, statuses []int, hdr map[string]string, msg string) (*Broker, *int, *[]time.Duration) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github-read-token" {
			io.WriteString(w, `{"token":"ghs-x"}`)
			return
		}
		st := statuses[min(calls, len(statuses)-1)]
		calls++
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(st)
		if st == 200 {
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, `{"message":"`+msg+`"}`)
	}))
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa\n"), 0o600)
	var slept []time.Duration
	return &Broker{URL: srv.URL, TokenPath: tok, HTTP: srv.Client(), API: srv.URL, Sleep: func(d time.Duration) { slept = append(slept, d) }}, &calls, &slept
}

// 429 and the secondary rate limit (403) are retried a bounded number of times, waiting what GitHub asked up to a cap.
func TestGetJSONRetriesRateLimits(t *testing.T) {
	b, calls, slept := rateStub(t, []int{429, 200}, map[string]string{"Retry-After": "2"}, "rate limited")
	st, _, err := b.Session("shop").GetJSON(context.Background(), "o", "shop", "rulesets")
	if err != nil || st != 200 || *calls != 2 || len(*slept) != 1 || (*slept)[0] != 2*time.Second {
		t.Fatalf("429 then 200: %d %v calls=%d slept=%v", st, err, *calls, *slept)
	}
	b, calls, slept = rateStub(t, []int{403, 200}, map[string]string{"Retry-After": "1"}, "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.")
	if st, _, err := b.Session("shop").GetJSON(context.Background(), "o", "shop", "rulesets"); err != nil || st != 200 || *calls != 2 {
		t.Fatalf("secondary limit: %d %v calls=%d", st, err, *calls)
	}
	// An ordinary 403 (a permission, a plan) is an answer, never retried.
	b, calls, _ = rateStub(t, []int{403, 200}, nil, "Resource not accessible by integration")
	if st, _, err := b.Session("shop").GetJSON(context.Background(), "o", "shop", "rulesets"); err != nil || st != 403 || *calls != 1 {
		t.Fatalf("plain 403: %d %v calls=%d", st, err, *calls)
	}
}

func TestGetJSONRateLimitGivesUpActionably(t *testing.T) {
	// Asked to wait longer than the cap: not waited for at all, and the caller is told how long.
	b, calls, slept := rateStub(t, []int{429}, map[string]string{"Retry-After": "3600"}, "rate limited")
	_, _, err := b.Session("shop").GetJSON(context.Background(), "o", "shop", "rulesets")
	var rl RateLimited
	if !errors.As(err, &rl) || rl.Wait != time.Hour || *calls != 1 || len(*slept) != 0 {
		t.Fatalf("over the cap: %v calls=%d slept=%v", err, *calls, *slept)
	}
	// A short limit that does not lift: bounded tries, each wait within the cap.
	b, calls, slept = rateStub(t, []int{429}, map[string]string{"Retry-After": "5"}, "rate limited")
	_, _, err = b.Session("shop").GetJSON(context.Background(), "o", "shop", "rulesets")
	if !errors.As(err, &rl) {
		t.Fatalf("want RateLimited, got %v", err)
	}
	if *calls != maxRateAttempts || len(*slept) != maxRateAttempts-1 {
		t.Fatalf("calls %d slept %v, bound %d", *calls, *slept, maxRateAttempts)
	}
	for _, d := range *slept {
		if d > maxRateWait {
			t.Fatalf("waited %s, cap %s", d, maxRateWait)
		}
	}
	if !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("the message must say what to do: %v", err)
	}
	// x-ratelimit-reset (epoch seconds) is honoured when there is no Retry-After.
	reset := strconv.FormatInt(time.Now().Add(2*time.Second).Unix()+1, 10)
	b, calls, slept = rateStub(t, []int{403, 200}, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, "API rate limit exceeded")
	if st, _, err := b.Session("shop").GetJSON(context.Background(), "o", "shop", "rulesets"); err != nil || st != 200 || len(*slept) != 1 || (*slept)[0] <= 0 || (*slept)[0] > maxRateWait {
		t.Fatalf("reset: %d %v slept=%v", st, err, *slept)
	}
}

// GraphQL is one POST to /graphql with the token minted for that repository, the query and its variables in the body.
func TestGraphQLPostsTheQueryWithTheRepositoryToken(t *testing.T) {
	var got struct {
		Auth, Method, Path string
		Body               map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/github-read-token" {
			io.WriteString(w, `{"token":"ghs-shop"}`)
			return
		}
		got.Auth, got.Method, got.Path = r.Header.Get("Authorization"), r.Method, r.URL.Path
		json.NewDecoder(r.Body).Decode(&got.Body)
		io.WriteString(w, `{"data":{}}`)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("sa\n"), 0o600)
	b := &Broker{URL: srv.URL, TokenPath: tok, HTTP: srv.Client(), API: srv.URL}
	st, body, err := b.Session("shop").GraphQL(context.Background(), "o", "shop", "query($n:Int!){x}", map[string]any{"n": 5})
	if err != nil || st != 200 || string(body) != `{"data":{}}` {
		t.Fatalf("%d %s %v", st, body, err)
	}
	if got.Method != "POST" || got.Path != "/graphql" || got.Auth != "Bearer ghs-shop" || got.Body["query"] != "query($n:Int!){x}" || got.Body["variables"].(map[string]any)["n"] != float64(5) {
		t.Fatalf("%+v", got)
	}
}

// github.com serves GraphQL at <api>/graphql; GitHub Enterprise Server's REST base ends in /api/v3 and its GraphQL is /api/graphql.
func TestGraphQLURLFromTheAPIBase(t *testing.T) {
	for base, want := range map[string]string{
		"":                                "https://api.github.com/graphql",
		"https://api.github.com":          "https://api.github.com/graphql",
		"https://api.github.com/":         "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3":  "https://ghe.example.com/api/graphql",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com/api/graphql",
		"http://127.0.0.1:5555":           "http://127.0.0.1:5555/graphql",
	} {
		if got := (&Broker{API: base}).graphqlURL(); got != want {
			t.Errorf("%q: %s, want %s", base, got, want)
		}
	}
}
