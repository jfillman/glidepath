package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Reviewer is a TokenReview with audiences; tests swap it.
type Reviewer interface {
	Review(ctx context.Context, token string, audiences []string) (*authenticationv1.TokenReviewStatus, error)
}

type kubeReviewer struct{ cs kubernetes.Interface }

// NewKubeReviewer reviews tokens with the cluster's TokenReview.
func NewKubeReviewer(cs kubernetes.Interface) Reviewer { return kubeReviewer{cs} }

func (k kubeReviewer) Review(ctx context.Context, token string, audiences []string) (*authenticationv1.TokenReviewStatus, error) {
	tr, err := k.cs.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return &tr.Status, nil
}

// Verifier decides whether an event the tap saw really came from who it claims, with the same
// TokenReview cdevents-broker-auth does (glidepath/broker/cmd/token-review-interceptor): the bearer
// token must be a ServiceAccount's, for the broker's audience. The tap is reachable by anything in
// the cluster, so it never trusts that the broker's own interceptor ran.
//
// Which app an event belongs to:
//   - a pipeline's event (catalog/lib/cdevents.sh) has context.source /glidepath/<namespace>/<run>; the
//     token's namespace must be that namespace, and it must be an app's <type>-<app>-cicd namespace.
//     The app's own Triggers check exactly this (extensions.app_namespace against the source).
//   - a relay's event (glidepath-relay, forwarding a release outcome for any app) names the app in
//     subject.content.appName/appNamespace; only the identities in Relays may, the claimed namespace must
//     be that app's -cicd namespace and the source's. The bounded trust release-outcome-trigger.yaml takes.
type Verifier struct {
	Reviewer Reviewer
	Audience string          // the broker's audience (cdevents-broker)
	Relays   map[string]bool // usernames that forward events for any app
}

var (
	sourcePattern = regexp.MustCompile(`^/(?:glidepath|platform-cicd)/([a-z0-9][a-z0-9-]{0,62})(?:/|$)`)
	cicdPattern   = regexp.MustCompile(`^(?:app|infra)-([a-z0-9][a-z0-9-]*)-cicd$`)
	appPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// Rejected is an event the tap drops; Reason never contains the token.
type Rejected struct{ Reason string }

func (e Rejected) Error() string { return e.Reason }

func bearer(v string) string {
	t := strings.TrimPrefix(v, "Bearer ")
	if t == v {
		return ""
	}
	return strings.TrimSpace(t)
}

// Verify returns the event's app, or Rejected.
func (v *Verifier) Verify(ctx context.Context, authorization string, h *header) (string, error) {
	token := bearer(authorization)
	if token == "" {
		return "", Rejected{"missing or malformed Authorization: Bearer <token> header"}
	}
	st, err := v.Reviewer.Review(ctx, token, []string{v.Audience})
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	if !st.Authenticated {
		return "", Rejected{"token not authenticated for audience " + v.Audience}
	}
	user := st.User.Username
	parts := strings.Split(user, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return "", Rejected{"caller is not a ServiceAccount identity: " + user}
	}
	m := sourcePattern.FindStringSubmatch(h.Context.Source)
	if m == nil {
		return "", Rejected{"context.source is not /glidepath/<namespace>/...: " + h.Context.Source}
	}
	srcNS := m[1]
	claimedApp, _ := h.Subject.Content["appName"].(string)
	if v.Relays[user] {
		ns, _ := h.Subject.Content["appNamespace"].(string)
		if !appPattern.MatchString(claimedApp) {
			return "", Rejected{user + " is a relay; its event must name subject.content.appName"}
		}
		if ns != srcNS || (ns != "app-"+claimedApp+"-cicd" && ns != "infra-"+claimedApp+"-cicd") {
			return "", Rejected{fmt.Sprintf("relay event: appNamespace %q, source namespace %q and app %q disagree", ns, srcNS, claimedApp)}
		}
		return claimedApp, nil
	}
	if parts[2] != srcNS {
		return "", Rejected{fmt.Sprintf("token namespace %q is not the event's source namespace %q", parts[2], srcNS)}
	}
	am := cicdPattern.FindStringSubmatch(srcNS)
	if am == nil || !appPattern.MatchString(am[1]) {
		return "", Rejected{"source namespace is not an app's -cicd namespace: " + srcNS}
	}
	if claimedApp != "" && claimedApp != am[1] {
		return "", Rejected{fmt.Sprintf("subject.content.appName %q is not the namespace's app %q", claimedApp, am[1])}
	}
	return am[1], nil
}

// The Tekton Triggers interceptor contract (what cdevents-broker-auth speaks too).
type interceptorRequest struct {
	Body   string              `json:"body"`
	Header map[string][]string `json:"header"`
}

type interceptorStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type interceptorResponse struct {
	Continue bool              `json:"continue"`
	Status   interceptorStatus `json:"status"`
}

const (
	codeInvalidArgument    = 3
	codeFailedPrecondition = 9 // "recorded; this Trigger never fires"
	codeUnauthenticated    = 16
	maxTapBody             = 1 << 20
)

func firstHeader(h map[string][]string, k string) string {
	for name, v := range h {
		if strings.EqualFold(name, k) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// Tap is the ClusterInterceptor glidepath-api-events: the broker hands it every event, it keeps
// the ones whose sender checks out, and it always answers continue: false, so its Trigger never
// creates anything.
type Tap struct {
	Verifier *Verifier
	Store    *Store
	Peers    *Peers // nil: a single replica
	Log      *slog.Logger
	Now      func() time.Time
}

func (t *Tap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	resp := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(interceptorResponse{Continue: false, Status: interceptorStatus{Code: code, Message: msg}})
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxTapBody+1))
	if err != nil || len(raw) > maxTapBody {
		t.Log.Warn("events.tap drop", "reason", "request body unreadable or over 1 MiB")
		resp(codeInvalidArgument, "glidepath-api tap: request body unreadable or too large")
		return
	}
	var req interceptorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Log.Warn("events.tap drop", "reason", "not an interceptor request")
		resp(codeInvalidArgument, "glidepath-api tap: not an interceptor request")
		return
	}
	h, err := parseHeader([]byte(req.Body))
	if err != nil {
		t.Log.Warn("events.tap drop", "reason", err.Error())
		resp(codeInvalidArgument, "glidepath-api tap: "+err.Error())
		return
	}
	app, err := t.Verifier.Verify(r.Context(), firstHeader(req.Header, "Authorization"), h)
	if err != nil {
		// Never the token or the headers: only why, and which event.
		t.Log.Warn("events.tap drop", "reason", err.Error(), "type", h.Context.Type, "source", h.Context.Source, "eventId", h.Context.ID)
		resp(codeUnauthenticated, "glidepath-api tap: dropped")
		return
	}
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	at := now().UTC()
	env := Envelope{
		ID:         eventID(at, key(h.Context.Source, h.Context.ID)),
		App:        app,
		RoutingKey: RoutingKey(app, h.Context.Type),
		ReceivedAt: at,
		Event:      json.RawMessage(req.Body),
	}
	added, err := t.Store.Add(env)
	if err != nil {
		resp(codeInvalidArgument, "glidepath-api tap: "+err.Error())
		return
	}
	t.Log.Info("events.tap keep", "app", app, "routingKey", env.RoutingKey, "id", env.ID, "eventId", h.Context.ID, "duplicate", !added)
	if added && t.Peers != nil {
		t.Peers.Forward(env)
	}
	resp(codeFailedPrecondition, "glidepath-api tap: recorded; this Trigger never fires")
}
