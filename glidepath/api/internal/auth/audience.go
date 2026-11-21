package auth

// A route whose callers are workloads with a token minted for it alone (POST /v1/apps/{app}/pipelineruns/{name}/gate-verdict):
// the token is checked with a TokenReview that names the route's audience, so the pod's default token, or a token for any other
// audience, is refused, and the token is good for nothing else. A person's ID token never authenticates here, and nobody may act
// on behalf of anyone.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The pod claims the API server puts in a TokenReview of a pod-bound ServiceAccount token.
const (
	ExtraPodName = "authentication.kubernetes.io/pod-name"
	ExtraPodUID  = "authentication.kubernetes.io/pod-uid"
)

// AudienceReviewer is a Reviewer that can ask a TokenReview for given audiences.
type AudienceReviewer interface {
	TokenReviewAudiences(ctx context.Context, token string, audiences []string) (*authenticationv1.TokenReviewStatus, error)
}

func (k kubeReviewer) TokenReviewAudiences(ctx context.Context, token string, audiences []string) (*authenticationv1.TokenReviewStatus, error) {
	tr, err := k.cs.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return &tr.Status, nil
}

// AuthenticateAudience returns the caller of a request whose bearer token must be valid for audience, with the pod it is bound
// to (Caller.PodName, Caller.PodUID; empty when the token is not pod-bound).
func (a *Auth) AuthenticateAudience(r *http.Request, audience string) (*Caller, error) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		token = bearer(r.Header.Get(HeaderProxied))
	}
	if token == "" {
		return nil, &Error{http.StatusUnauthorized, "GP-AUTH-001", "no bearer token",
			"send Authorization: Bearer <a projected ServiceAccount token for audience " + audience + ">"}
	}
	if strings.TrimSpace(r.Header.Get(HeaderOnBehalf)) != "" {
		return nil, &Error{http.StatusForbidden, "GP-AUTH-003", "nobody acts on behalf of anyone on this route", ""}
	}
	ar, ok := a.Reviewer.(AudienceReviewer)
	if !ok {
		return nil, fmt.Errorf("token review: the reviewer cannot check an audience")
	}
	st, err := ar.TokenReviewAudiences(r.Context(), token, []string{audience})
	if err != nil {
		return nil, fmt.Errorf("token review: %w", err)
	}
	// The API server answers authenticated only for a token valid for one of the audiences asked; check what it says anyway, so a
	// reviewer that ignored the audience cannot let another token through.
	if !st.Authenticated || !slices.Contains(st.Audiences, audience) {
		return nil, &Error{http.StatusUnauthorized, "GP-AUTH-002", "token not accepted by the cluster for audience " + audience,
			"mount a projected ServiceAccount token with audience " + audience + " (the catalog Task does)"}
	}
	c := &Caller{User: st.User.Username, UID: st.User.UID, Groups: st.User.Groups}
	if v := st.User.Extra[ExtraPodName]; len(v) == 1 {
		c.PodName = v[0]
	}
	if v := st.User.Extra[ExtraPodUID]; len(v) == 1 {
		c.PodUID = v[0]
	}
	return c, nil
}
