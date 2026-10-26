// Package auth decides who a caller is and what it may read (docs/admin/api.md).
//
// Who: a bearer token, checked with the cluster's own TokenReview, so anything the API server
// trusts works here unchanged: an in-cluster ServiceAccount, and a federated identity from another
// cluster (Tower's backstage-ingestor and tower-actor from the prod cluster). The token comes in
// Authorization, or in X-Glidepath-Authorization when the call came through the API server's
// services/proxy, which authenticates the caller and then drops Authorization before forwarding
// (the same limit glidepath/tekton-results-relay works around).
//
// What: a SubjectAccessReview on a virtual resource in the app's own -cicd namespace, group
// glidepath.hangar.io (runs, runs/logs, releases). Nothing in the cluster serves that group; RBAC
// only needs the strings. So who may read an app's runs is ordinary RBAC, bound by the chart.
//
// On behalf of: a delegate (Tower) may name the person it acts for in X-Glidepath-On-Behalf-Of.
// Only callers listed as delegates may; anyone else is refused. In P1 every call is a read, and a
// read is authorized as the caller itself; the person is recorded in the audit line. Writes (P3)
// will authorize the person.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	Group          = "glidepath.hangar.io"
	HeaderProxied  = "X-Glidepath-Authorization"
	HeaderOnBehalf = "X-Glidepath-On-Behalf-Of"
)

// Caller is an authenticated request's identity.
type Caller struct {
	User       string   `json:"user"`
	UID        string   `json:"uid,omitempty"`
	Groups     []string `json:"groups,omitempty"`
	OnBehalfOf string   `json:"onBehalfOf,omitempty"`
	Delegate   bool     `json:"delegate"`
}

// Error carries the HTTP status and a stable rule id, in the shape Clearance's decisions use.
type Error struct {
	Status int
	Rule   string
	Msg    string
	Hint   string
}

func (e *Error) Error() string { return e.Msg }

// Reviewer is the slice of the Kubernetes API this package needs; tests swap it.
type Reviewer interface {
	TokenReview(ctx context.Context, token string) (*authenticationv1.TokenReviewStatus, error)
	AccessReview(ctx context.Context, c *Caller, attrs authorizationv1.ResourceAttributes) (*authorizationv1.SubjectAccessReviewStatus, error)
}

type kubeReviewer struct{ cs kubernetes.Interface }

func NewKubeReviewer(cs kubernetes.Interface) Reviewer { return kubeReviewer{cs} }

func (k kubeReviewer) TokenReview(ctx context.Context, token string) (*authenticationv1.TokenReviewStatus, error) {
	tr, err := k.cs.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return &tr.Status, nil
}

func (k kubeReviewer) AccessReview(ctx context.Context, c *Caller, attrs authorizationv1.ResourceAttributes) (*authorizationv1.SubjectAccessReviewStatus, error) {
	sar, err := k.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{User: c.User, UID: c.UID, Groups: c.Groups, ResourceAttributes: &attrs},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return &sar.Status, nil
}

// Auth authenticates and authorizes requests.
type Auth struct {
	Reviewer  Reviewer
	Delegates map[string]bool // usernames allowed to send X-Glidepath-On-Behalf-Of
}

func bearer(v string) string {
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// Authenticate returns the caller, or an *Error.
func (a *Auth) Authenticate(r *http.Request) (*Caller, error) {
	token := bearer(r.Header.Get("Authorization"))
	if token == "" {
		token = bearer(r.Header.Get(HeaderProxied))
	}
	if token == "" {
		return nil, &Error{http.StatusUnauthorized, "GP-AUTH-001", "no bearer token",
			"send Authorization: Bearer <token>, or " + HeaderProxied + " when calling through the API server's service proxy"}
	}
	st, err := a.Reviewer.TokenReview(r.Context(), token)
	if err != nil {
		return nil, fmt.Errorf("token review: %w", err)
	}
	if !st.Authenticated {
		return nil, &Error{http.StatusUnauthorized, "GP-AUTH-002", "token not accepted by the cluster", st.Error}
	}
	c := &Caller{User: st.User.Username, UID: st.User.UID, Groups: st.User.Groups, Delegate: a.Delegates[st.User.Username]}
	if obo := strings.TrimSpace(r.Header.Get(HeaderOnBehalf)); obo != "" {
		if !c.Delegate {
			return nil, &Error{http.StatusForbidden, "GP-AUTH-003", c.User + " may not act on behalf of anyone",
				"only the chart's api.delegates may send " + HeaderOnBehalf}
		}
		if len(obo) > 256 {
			return nil, &Error{http.StatusBadRequest, "GP-AUTH-003", HeaderOnBehalf + " is too long", ""}
		}
		c.OnBehalfOf = obo
	}
	return c, nil
}

// Authorize checks the caller may `verb` the virtual resource in namespace.
func (a *Auth) Authorize(ctx context.Context, c *Caller, verb, resource, subresource, namespace, name string) error {
	st, err := a.Reviewer.AccessReview(ctx, c, authorizationv1.ResourceAttributes{
		Group: Group, Resource: resource, Subresource: subresource, Verb: verb, Namespace: namespace, Name: name,
	})
	if err != nil {
		return fmt.Errorf("access review: %w", err)
	}
	if !st.Allowed {
		what := resource
		if subresource != "" {
			what += "/" + subresource
		}
		return &Error{http.StatusForbidden, "GP-AUTHZ-001",
			fmt.Sprintf("%s may not %s %s.%s in %s", c.User, verb, what, Group, namespace),
			"bind the glidepath-api-reader ClusterRole to this identity (the chart's api.readers)"}
	}
	return nil
}

// AsError unwraps an *Error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
