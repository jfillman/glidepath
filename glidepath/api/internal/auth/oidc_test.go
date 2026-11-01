package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
)

const issuer = "http://dex.hangar-dev.local/dex"

type keys struct {
	key *rsa.PrivateKey
	srv *httptest.Server
}

func newKeys(t *testing.T) *keys {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ks := &keys{key: k}
	ks.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(ks.srv.Close)
	return ks
}

func (ks *keys) token(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"iss": issuer, "aud": "glidepath-api", "sub": "Cg1qZmlsbG1hbhIGZ2l0aHVi", "preferred_username": "jfillman",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
	for k, v := range claims {
		if v == nil {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	raw, err := jwt.Signed(sig).Claims(base).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A reviewer that fails the test if a people-dex token ever reaches TokenReview.
type noReview struct{ t *testing.T }

func (n noReview) TokenReview(context.Context, string) (*authenticationv1.TokenReviewStatus, error) {
	n.t.Fatal("a people-dex token went to TokenReview")
	return nil, nil
}
func (noReview) AccessReview(context.Context, *Caller, authorizationv1.ResourceAttributes) (*authorizationv1.SubjectAccessReviewStatus, error) {
	return &authorizationv1.SubjectAccessReviewStatus{}, nil
}

func request(token string, hdr ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return r
}

func TestPeopleTokens(t *testing.T) {
	ks := newKeys(t)
	o, err := NewOIDC(issuer, ks.srv.URL, "glidepath-api", []string{"jfillman", " other "}, ks.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	a := &Auth{Reviewer: noReview{t}, OIDC: o, Delegates: map[string]bool{"people:jfillman": true}}

	c, err := a.Authenticate(request(ks.token(t, ks.key, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "people:jfillman" || c.Delegate || c.UID == "" {
		t.Fatalf("caller: %+v (a person is never a delegate, even if named one)", c)
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := []struct {
		name   string
		token  string
		hdr    []string
		status int
		rule   string
	}{
		{"signed by another key", ks.token(t, other, nil), nil, 401, "GP-AUTH-002"},
		{"minted for Clearance", ks.token(t, ks.key, map[string]any{"aud": "clearance"}), nil, 401, "GP-AUTH-002"},
		{"expired", ks.token(t, ks.key, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), nil, 401, "GP-AUTH-002"},
		{"no username", ks.token(t, ks.key, map[string]any{"preferred_username": nil}), nil, 401, "GP-AUTH-002"},
		{"not a GitHub login", ks.token(t, ks.key, map[string]any{"preferred_username": "system:masters"}), nil, 401, "GP-AUTH-002"},
		{"not allowed", ks.token(t, ks.key, map[string]any{"preferred_username": "stranger"}), nil, 403, "GP-AUTH-005"},
		{"acting for someone", ks.token(t, ks.key, nil), []string{HeaderOnBehalf, "user:default/someone"}, 403, "GP-AUTH-003"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Authenticate(request(tc.token, tc.hdr...))
			var e *Error
			if !errors.As(err, &e) || e.Status != tc.status || e.Rule != tc.rule {
				t.Fatalf("got %v, want %d %s", err, tc.status, tc.rule)
			}
		})
	}
	if c, err := a.Authenticate(request(ks.token(t, ks.key, map[string]any{"preferred_username": "other"}))); err != nil || c.User != "people:other" {
		t.Fatalf("allowlist entries are trimmed: %v %v", c, err)
	}
}

// A ServiceAccount token (another issuer) still goes to TokenReview, untouched by OIDC.
type saReview struct{ seen *string }

func (s saReview) TokenReview(_ context.Context, tok string) (*authenticationv1.TokenReviewStatus, error) {
	*s.seen = tok
	return &authenticationv1.TokenReviewStatus{Authenticated: true, User: authenticationv1.UserInfo{Username: "system:serviceaccount:x:y"}}, nil
}
func (saReview) AccessReview(context.Context, *Caller, authorizationv1.ResourceAttributes) (*authorizationv1.SubjectAccessReviewStatus, error) {
	return &authorizationv1.SubjectAccessReviewStatus{}, nil
}

func TestOtherTokensGoToTokenReview(t *testing.T) {
	ks := newKeys(t)
	o, _ := NewOIDC(issuer, ks.srv.URL, "glidepath-api", []string{"jfillman"}, ks.srv.Client())
	var seen string
	a := &Auth{Reviewer: saReview{&seen}, OIDC: o}
	sa := ks.token(t, ks.key, map[string]any{"iss": "https://kubernetes.default.svc"})
	c, err := a.Authenticate(request(sa))
	if err != nil || c.User != "system:serviceaccount:x:y" || seen != sa {
		t.Fatalf("%v %v", c, err)
	}
	for _, opaque := range []string{"opaque-token", "a.b", "a.!!!.c"} {
		if _, err := a.Authenticate(request(opaque)); err != nil || seen != opaque {
			t.Fatalf("%q: %v", opaque, err)
		}
	}
	// Without OIDC configured nothing changes.
	if _, err := (&Auth{Reviewer: saReview{&seen}}).Authenticate(request(sa)); err != nil {
		t.Fatal(err)
	}
}

func TestNewOIDCRefusesAnOpenDoor(t *testing.T) {
	if _, err := NewOIDC(issuer, "http://keys", "glidepath-api", []string{"", " "}, nil); err == nil {
		t.Fatal("an issuer that signs in any GitHub user needs an allowlist")
	}
	if _, err := NewOIDC(issuer, "http://keys", "", []string{"jfillman"}, nil); err == nil {
		t.Fatal("no audience")
	}
}
