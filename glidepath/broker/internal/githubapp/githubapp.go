// Shared GitHub App client (JWT signing + installation-token minting), used by both
// token-review-interceptor and argocd-outcome-relay. The relay holds this client
// directly rather than calling the interceptor's /github-installation-token endpoint,
// because that endpoint's authorization check trusts only the caller's own
// TokenReview-verified app namespace - which doesn't fit a platform-wide service that
// legitimately needs tokens for any app's gitops repo. Same credential, same narrow
// per-request repo scoping either way - not a widening of what any single Application
// can reach.
//
// Standard library only for JWT signing (RS256) - not worth a library for this.
package githubapp

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	appIDPath     = "/etc/github-app/github-application-id"
	appKeyPath    = "/etc/github-app/github-private-key"
	githubAPIBase = "https://api.github.com"
)

type Creds struct {
	appID      string
	privateKey *rsa.PrivateKey
}

// LoadCreds reads the App ID/private key from the fixed paths the hand-created
// github-app-creds Secret is mounted at (a copy of two fields from PaC's own secret,
// never pasted through chat).
func LoadCreds() (*Creds, error) {
	idBytes, err := os.ReadFile(appIDPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", appIDPath, err)
	}
	keyBytes, err := os.ReadFile(appKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", appKeyPath, err)
	}
	block, _ := pem.Decode(keyBytes)
	if block == nil {
		return nil, fmt.Errorf("%s does not contain a PEM block", appKeyPath)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// GitHub App keys are sometimes issued as PKCS8 rather than PKCS1.
		parsed, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("parsing private key (tried PKCS1 and PKCS8): %v / %v", err, err2)
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
		key = rsaKey
	}
	return &Creds{appID: string(bytes.TrimSpace(idBytes)), privateKey: key}, nil
}

// SignAppJWT builds the short-lived JWT GitHub requires for App-level (not
// installation-level) API calls: RS256 over {"alg":"RS256","typ":"JWT"} and
// {"iat","exp","iss"} claims, base64url-encoded and dot-joined per the JWT spec.
func (c *Creds) SignAppJWT() (string, error) {
	now := time.Now()
	header := base64URLEncode([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		// Backdated 60s: GitHub explicitly recommends this to tolerate clock drift
		// between this pod and GitHub's own servers.
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": c.appID,
	})
	if err != nil {
		return "", err
	}
	payload := base64URLEncode(claims)
	signingInput := header + "." + payload

	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}
	return signingInput + "." + base64URLEncode(sig), nil
}

func base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// InstallationIDForRepo looks up the App's installation ID for a specific repo,
// authenticated with the App-level JWT (an installation token doesn't exist yet at this
// point) - avoids statically storing installation IDs, since a repo only resolves to
// one once the App is installed on it (a manual step).
func InstallationIDForRepo(appJWT, owner, repo string) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/installation", githubAPIBase, owner, repo), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("calling GitHub installation lookup: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GitHub installation lookup for %s/%s: status %d: %s", owner, repo, resp.StatusCode, string(body))
	}
	var parsed struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("parsing installation lookup response: %w", err)
	}
	return parsed.ID, nil
}

// MintInstallationToken exchanges the App-level JWT for a short-lived installation
// token scoped to exactly one repository - GitHub's `repositories` field on this
// endpoint is what makes the returned token unable to touch any other repo the App
// might be installed on, which is the whole point: whoever calls this holds the App's
// private key, but nothing it hands back can reach further than the one repo asked for.
func MintInstallationToken(appJWT string, installationID int64, repo string) (token string, expiresAt string, err error) {
	reqBody, _ := json.Marshal(map[string]any{"repositories": []string{repo}})
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/app/installations/%s/access_tokens", githubAPIBase, strconv.FormatInt(installationID, 10)),
		bytes.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("calling GitHub access-token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("GitHub access-token exchange for installation %d, repo %s: status %d: %s", installationID, repo, resp.StatusCode, string(body))
	}
	var parsed struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("parsing access-token response: %w", err)
	}
	return parsed.Token, parsed.ExpiresAt, nil
}
