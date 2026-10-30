package evidence

// GHCR: a port of Tower's ghcr.ts (parseImageRef, getPullToken, resolveDigest), with one change of
// source: the token exchange is authenticated with the app's own registry-credentials Secret (the
// dockerconfigjson every app's -cicd namespace has, auths["ghcr.io"]) instead of Tower's GitHub
// integration token. The Secret is read per request and dropped when the request ends: never
// cached, logged, returned or put in an error. GHCR's anonymous exchange only works for public
// packages, which is why a credential is sent at all.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// Registry is the only registry Tower supports.
	Registry = "ghcr.io"
	// PullSecret is the dockerconfigjson Secret in every app's -cicd namespace.
	PullSecret = "registry-credentials"
)

// Invalid is a request that can't be answered: an image outside ghcr.io, or with no tag or digest.
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

// ParseImageRef splits ghcr.io/<repo>:<tag>, ghcr.io/<repo>@<digest> or ghcr.io/<repo>:<tag>@<digest>.
// A digest-pinned ref's tag is dropped: the digest is the ref.
func ParseImageRef(image string) (repo, ref string, err error) {
	prefix := Registry + "/"
	if !strings.HasPrefix(image, prefix) {
		return "", "", Invalid{fmt.Sprintf("only %s images are supported, got: %s", Registry, image)}
	}
	rest := image[len(prefix):]
	if at := strings.Index(rest, "@"); at != -1 {
		named := rest[:at]
		if colon := strings.LastIndex(named, ":"); colon > strings.LastIndex(named, "/") {
			named = named[:colon]
		}
		return named, rest[at+1:], nil
	}
	colon := strings.LastIndex(rest, ":")
	if colon == -1 {
		return "", "", Invalid{"image reference has no tag or digest: " + image}
	}
	return rest[:colon], rest[colon+1:], nil
}

// basicAuth is the registry credential as a Basic Authorization value, or "" for anonymous.
type basicAuth string

// CredentialSource returns the Authorization header for GHCR's token exchange, for one request.
type CredentialSource func(ctx context.Context, namespace string) (basicAuth, error)

// SecretCredentials reads PullSecret in the app's namespace. A missing Secret, or one without a
// ghcr.io entry, is anonymous: Tower falls back the same way when it has no token.
func SecretCredentials(cs kubernetes.Interface) CredentialSource {
	return func(ctx context.Context, namespace string) (basicAuth, error) {
		return readPullSecret(ctx, cs, namespace, PullSecret)
	}
}

// FixedSecretCredentials reads one dockerconfigjson Secret whatever the app: the Flight registry's
// read credential (glidepath-control-plane's flight-registry-read in the catalog namespace), since
// an app's own registry-credentials holds the Ground owner's credential, which Flight refuses.
func FixedSecretCredentials(cs kubernetes.Interface, namespace, name string) CredentialSource {
	return func(ctx context.Context, _ string) (basicAuth, error) {
		return readPullSecret(ctx, cs, namespace, name)
	}
}

func readPullSecret(ctx context.Context, cs kubernetes.Interface, namespace, name string) (basicAuth, error) {
	s, err := cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		// The API server's error names the Secret, never its contents.
		return "", fmt.Errorf("reading %s/%s: %w", namespace, name, err)
	}
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if json.Unmarshal(s.Data[".dockerconfigjson"], &cfg) != nil {
		return "", fmt.Errorf("%s/%s is not a dockerconfigjson", namespace, name)
	}
	a, ok := cfg.Auths[Registry]
	switch {
	case !ok:
		return "", nil
	case a.Username != "" && a.Password != "":
		return basicAuth("Basic " + base64.StdEncoding.EncodeToString([]byte(a.Username+":"+a.Password))), nil
	case a.Auth != "":
		return basicAuth("Basic " + a.Auth), nil
	}
	return "", nil
}

// registry talks to GHCR's v2 API.
type registry struct {
	base string // https://ghcr.io
	http *http.Client
}

// pullToken exchanges the credential for a pull-scoped bearer for repo. Errors carry the status
// only: GHCR's body is never read into one.
func (r *registry) pullToken(ctx context.Context, repo string, auth basicAuth) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/token?service=%s&scope=repository:%s:pull", r.base, Registry, repo), nil)
	if err != nil {
		return "", err
	}
	if auth != "" {
		req.Header.Set("Authorization", string(auth))
	}
	res, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("GHCR token request failed: %s", scrubURLError(err))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("GHCR token request failed with %d", res.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil || body.Token == "" {
		return "", fmt.Errorf("GHCR token response had no token")
	}
	return body.Token, nil
}

// scrubURLError keeps a transport error's cause without echoing anything request-specific.
func scrubURLError(err error) string {
	if ue, ok := err.(interface{ Unwrap() error }); ok && ue.Unwrap() != nil {
		return ue.Unwrap().Error()
	}
	return err.Error()
}

const manifestAccept = "application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json"

// resolveDigest asks for the tag's manifest and returns its Docker-Content-Digest.
func (r *registry) resolveDigest(ctx context.Context, repo, tag, token string) (string, error) {
	res, err := r.get(ctx, fmt.Sprintf("/v2/%s/manifests/%s", repo, tag), token, manifestAccept)
	if err != nil {
		return "", err
	}
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("manifest request for %s:%s failed with %d", repo, tag, res.StatusCode)
	}
	d := res.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("registry did not return a digest for %s:%s", repo, tag)
	}
	return d, nil
}

// get sends an authenticated GET. The caller closes the body.
func (r *registry) get(ctx context.Context, path, token, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GHCR request failed: %s", scrubURLError(err))
	}
	return res, nil
}

// getBody fetches path and returns its status and body; the caller decides what a status means.
func (r *registry) getBody(ctx context.Context, path, token, accept string) (status int, body []byte, err error) {
	res, err := r.get(ctx, path, token, accept)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err = io.ReadAll(io.LimitReader(res.Body, 64<<20))
	return res.StatusCode, body, err
}

func ok(status int) bool { return status >= 200 && status <= 299 }
