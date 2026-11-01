package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// People sign in to glidepath-api directly with an ID token from people-dex (Dex in front of GitHub, the same issuer
// Clearance trusts), for calls over the Gateway route rather than the API server's proxy (docs/admin/api.md#people).
//
// A token whose issuer is the configured one is verified here: signature against the issuer's JWKS (read in cluster),
// iss, aud (glidepath-api's own client id, so a token minted for Clearance is not accepted here), exp. The person is
// UsernamePrefix + the username claim (people:jfillman), authorized by the same SubjectAccessReview as every caller,
// so what a person may do is ordinary RBAC on that user name. Dex signs in any GitHub user, so only AllowedUsers get
// in at all; a person is never a delegate. Every other bearer token goes to TokenReview as before.

// OIDC verifies people-dex ID tokens.
type OIDC struct {
	Issuer         string
	UsernameClaim  string // default preferred_username
	UsernamePrefix string // default "people:"
	AllowedUsers   map[string]bool
	verifier       *oidc.IDTokenVerifier
}

// login is a GitHub login (the username claim's only legal shape), so a prefix plus it is a safe RBAC user name.
var login = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// NewOIDC builds the verifier. jwksURL is where the issuer's keys are read from inside the cluster (the issuer's own
// URL is the browser's, a hostname that need not resolve here); audience is glidepath-api's client id at the issuer.
func NewOIDC(issuer, jwksURL, audience string, allowed []string, client *http.Client) (*OIDC, error) {
	if issuer == "" || jwksURL == "" || audience == "" {
		return nil, fmt.Errorf("OIDC needs an issuer, a JWKS URL and an audience")
	}
	o := &OIDC{Issuer: issuer, UsernameClaim: "preferred_username", UsernamePrefix: "people:", AllowedUsers: map[string]bool{}}
	for _, u := range allowed {
		if u = strings.TrimSpace(u); u != "" {
			o.AllowedUsers[u] = true
		}
	}
	if len(o.AllowedUsers) == 0 {
		return nil, fmt.Errorf("OIDC needs OIDC_ALLOWED_USERS: the issuer signs in any GitHub user")
	}
	ctx := context.Background()
	if client != nil {
		ctx = oidc.ClientContext(ctx, client)
	}
	keys := oidc.NewRemoteKeySet(ctx, jwksURL)
	o.verifier = oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}})
	return o, nil
}

// Mine reports whether a bearer token claims this issuer. It only routes the token: the claim is read unverified,
// and Verify checks it.
func (o *OIDC) Mine(token string) bool {
	if o == nil {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var c struct {
		Iss string `json:"iss"`
	}
	return json.Unmarshal(raw, &c) == nil && c.Iss == o.Issuer
}

// Verify returns the person a valid token names, or an *Error.
func (o *OIDC) Verify(ctx context.Context, token string) (*Caller, error) {
	idt, err := o.verifier.Verify(ctx, token)
	if err != nil {
		return nil, &Error{http.StatusUnauthorized, "GP-AUTH-002", "ID token not accepted", "sign in again (ID tokens last an hour): " + trim(err.Error())}
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return nil, &Error{http.StatusUnauthorized, "GP-AUTH-002", "ID token claims unreadable", ""}
	}
	name, _ := claims[o.UsernameClaim].(string)
	if !login.MatchString(name) {
		return nil, &Error{http.StatusUnauthorized, "GP-AUTH-002", "ID token has no usable " + o.UsernameClaim, ""}
	}
	if !o.AllowedUsers[name] {
		return nil, &Error{http.StatusForbidden, "GP-AUTH-005", name + " may not sign in to glidepath-api",
			"ask an admin to add the GitHub login to api.oidc.allowedUsers"}
	}
	return &Caller{User: o.UsernamePrefix + name, UID: idt.Subject}, nil
}

func trim(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
