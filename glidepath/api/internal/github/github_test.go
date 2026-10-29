package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
