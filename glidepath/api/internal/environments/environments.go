// Package environments is glidepath-api's tier T2 writes (docs/admin/api.md#promote-and-roll-back): promote an app's
// release to its next Flight environment, and roll a Flight environment back to an earlier release, plus the two
// reads they rest on (the release plan and the rollback plan). It replaced Tower's own (backstage
// packages/backend/src/glidepathPromote.ts, glidepathReleaseRun.ts, glidepathRollback.ts, releasePlan.ts, removed in
// P6), ported so the answers stayed the same; Tower's Flight Promote and Roll back call it.
//
// Both writes only start the app's own `release` Pipeline, from the promote-release or rollback-release
// TriggerTemplate glidepath-app renders into the app's -cicd namespace. That pipeline's open-release-pr opens the
// gitops PR with the platform's GitHub App, and a human merges it. Nothing here can merge, push or open a PR: the
// API's GitHub token (internal/github) reads only.
//
// What is not offered: promoting to a Ground environment (Tower's promoteToLowerEnv is a direct commit to the app
// repo, which needs a GitHub write identity) and cloud pins. Both stay in Tower.
//
// Idempotency is the re-run's (internal/pipelineruns): the run's name is derived from the caller's Idempotency-Key, so
// a retry from either replica hits AlreadyExists on the same name and is answered as a replay.
package environments

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/lineage"
	"github.com/glidepath/api/internal/pipelineruns"
	"github.com/glidepath/api/internal/releases"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	PromoteTemplate  = "promote-release"
	RollbackTemplate = "rollback-release"
	// envsRoot is Tower's ENVS_ROOT: an app's Ground environment files live under glidepath/ (ADR-0018).
	envsRoot = "glidepath"
)

var TemplateGVR = schema.GroupVersionResource{Group: "triggers.tekton.dev", Version: "v1beta1", Resource: "triggertemplates"}

var (
	// Environment and cluster names go into a label selector, a branch name and a PipelineRun name.
	NameRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	shortRE = regexp.MustCompile(`(?i)(?:^|-)([0-9a-f]{7,40})$`)
	// An image a first deploy names: a repository and a tag, nothing that could smuggle another param.
	imageRepoRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,254}$`)
	imageTagRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Invalid is a malformed request (400).
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

// Refused is a request Glidepath's rules turn down (422): out of order, a Ground target, an ineligible rollback
// target, nothing to promote. Retrying the same request will not help; Hint says what would.
type Refused struct{ What, Hint string }

func (e Refused) Error() string { return e.What }

// GitHub is the reads one request makes (internal/github's Session).
type GitHub interface {
	Contents(ctx context.Context, owner, repo, path string) (string, error)
	Raw(ctx context.Context, owner, repo, path string) (string, error)
	CommitSHA(ctx context.Context, owner, repo, ref string) (string, error)
	OpenPR(ctx context.Context, owner, repo, branch string) (string, error)
}

// prefetch starts gh minting the tokens for repos, when gh can (internal/github's Session does; test fakes needn't).
func prefetch(ctx context.Context, gh GitHub, owner string, repos ...string) {
	if p, ok := gh.(interface {
		Prefetch(ctx context.Context, owner string, repos ...string)
	}); ok {
		p.Prefetch(ctx, owner, repos...)
	}
}

// Templates reads a TriggerTemplate; tests swap it.
type Templates interface {
	Get(ctx context.Context, namespace, name string) (map[string]any, error)
}

type kubeTemplates struct{ dyn dynamic.Interface }

func NewKubeTemplates(dyn dynamic.Interface) Templates { return kubeTemplates{dyn} }

func (k kubeTemplates) Get(ctx context.Context, namespace, name string) (map[string]any, error) {
	u, err := k.dyn.Resource(TemplateGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return u.Object, nil
}

// Client promotes and rolls back.
type Client struct {
	Owner          string                  // the GitHub owner of every app repo (the chart's githubOwner)
	OriginRef      string                  // the full ref promote and rollback runs claim as their origin (refs/heads/<first protected branch>); "" stamps none
	GitHub         func(app string) GitHub // a new Session per request
	Templates      Templates               //
	Runs           pipelineruns.Store      //
	LineageTimeout time.Duration           // overall deadline of the origin lookup; LineageTimeout (5 s) when zero
	// Lineage resolves the build a release's image came from (internal/lineage); nil resolves none, and a release run
	// then carries no origin and its answer says nothing about one.
	Lineage interface {
		Resolve(ctx context.Context, namespace, imageRef, commit, digest string) lineage.Resolution
	}
	Records interface { // release records for one environment and cluster
		ListEnv(ctx context.Context, namespace, app, env, cluster string) ([]releases.Record, error)
	}
}

// Repos is Tower's appRepos: an app's source repo, gitops repo, and where its cicd.yaml lives. An InfraService
// (infra-<name>-cicd) has no source repo; its cicd.yaml is in gitops-infra-<name>.
type Repos struct{ Source, Gitops, Config string }

func ReposOf(app *apps.App) Repos {
	if strings.HasPrefix(app.Namespace, "infra-") {
		g := "gitops-infra-" + app.Name
		return Repos{Source: app.Name, Gitops: g, Config: g}
	}
	return Repos{Source: app.Name, Gitops: "gitops-" + app.Name, Config: app.Name}
}

// Plan is GET /environments: Tower's fetchReleasePlan for the app.
func (c *Client) Plan(ctx context.Context, app *apps.App) (*Plan, error) {
	return fetchPlan(ctx, c.GitHub(app.Name), c.Owner, ReposOf(app))
}

// RollbackPlan is GET /environments/{env}/rollback-plan: Tower's fetchRollbackPlan.
func (c *Client) RollbackPlan(ctx context.Context, app *apps.App, env, cluster string) (*RollbackPlan, error) {
	if !NameRE.MatchString(env) || !NameRE.MatchString(cluster) {
		return nil, Invalid{`missing or malformed "env"/"cluster"`}
	}
	recs, err := c.Records.ListEnv(ctx, app.Namespace, app.Name, env, cluster)
	if err != nil {
		return nil, err
	}
	p := PlanRollback(recs)
	return &p, nil
}

// RunName is Tower's runName with the key hash's first 6 hex characters in place of its 3 random bytes:
// <kind>-<env>-<sha8>-<hex6>, cut at 63 characters.
func RunName(kind, env, gitRevision, keyHash string) string {
	sha := gitRevision
	if len(sha) > 8 {
		sha = sha[:8]
	}
	n := fmt.Sprintf("%s-%s-%s-%s", kind, env, sha, keyHash[:6])
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

// KeyHash is what a promote's or rollback's run name and annotation are derived from: the op, its environment and
// cluster, and the caller's key.
func KeyHash(app, kind, env, cluster, key string) string {
	return pipelineruns.KeyHash(app, kind+"/"+env+"/"+cluster, key)
}

var paramRE = regexp.MustCompile(`\$\(tt\.params\.([a-z0-9-]+)\)`)

// Instantiate is Tower's instantiate: the template's resource with every $(tt.params.<name>) replaced; a param the
// template names and the API does not set is an error, never left in.
func Instantiate(resource any, params map[string]string, template string) (any, error) {
	var missing string
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return paramRE.ReplaceAllStringFunc(x, func(m string) string {
				name := paramRE.FindStringSubmatch(m)[1]
				p, ok := params[name]
				if !ok && missing == "" {
					missing = name
				}
				return p
			})
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = walk(e)
			}
			return out
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, e := range x {
				out[k] = walk(e)
			}
			return out
		}
		return v
	}
	out := walk(resource)
	if missing != "" {
		return nil, Refused{fmt.Sprintf("%s template needs param '%s', which glidepath-api does not set", template, missing),
			"glidepath-app's TriggerTemplate is newer than this API; upgrade glidepath-api"}
	}
	return out, nil
}

// releaseRun is Tower's ReleaseRunParams.
type releaseRun struct {
	env, cluster, gitURL, gitRevision, imageRef, initiator string
	digest                                                 string          // the manifest digest the source environment's release file pins (promotion.pinDigest), or ""
	rollback                                               *rollbackParams // set for a rollback
}

// rollbackParams: the release-id a rollback replaces, why, and the app-repo revision to read cicd.yaml at.
type rollbackParams struct{ of, reason, configRevision string }

// Started is a release run's answer.
type Started struct {
	Name     string
	Replayed bool
	Object   map[string]any // dry run: the PipelineRun it creates (or found, on a replay)
	Origin   *Origin        // the build the release's image came from, when resolved
	// OriginUnknown says why there is no Origin ("" when Lineage is not configured or Origin is set). A release with no
	// origin is not refused: lineage is information, not a gate.
	OriginUnknown string
}

// Origin is the flow that built the image a release carries (docs/admin/api.md#promotion-lineage).
type Origin struct {
	ChainID   string `json:"chainId"`
	ChainSlug string `json:"chainSlug,omitempty"`
	TraceID   string `json:"traceId"`
	Run       string `json:"run,omitempty"` // the build's PipelineRun (gone from the cluster about an hour after it ends)
}

func originOf(o *lineage.Origin) *Origin {
	if o == nil {
		return nil
	}
	return &Origin{ChainID: o.ChainID, ChainSlug: o.ChainSlug, TraceID: o.TraceID, Run: o.Run}
}

// lineageKeys are the annotations stampOrigin sets, so a refused run can have them taken off again.
var lineageKeys = []string{pipelineruns.AnnotationOriginChainID, pipelineruns.AnnotationOriginChainSlug, pipelineruns.AnnotationOriginTraceID,
	pipelineruns.AnnotationOriginTraceparent, pipelineruns.AnnotationOriginFinishedAt}

// stampOrigin sets the lineage annotations, each checked against lineage.Shapes first; false (nothing set) when a value is
// outside its shape, because admission would refuse the whole set.
func stampOrigin(ann map[string]any, o *lineage.Origin) bool {
	a, ok := o.Annotations()
	if !ok {
		return false
	}
	for k, v := range a {
		ann[k] = v
	}
	return true
}

// startRun is Tower's startReleaseRun with a derived name: read the template, fill its params, create the run (a
// server-side dry run when dryRun). A name already taken by this key's run is a replay; by anything else a Conflict.
func (c *Client) startRun(ctx context.Context, app *apps.App, p releaseRun, keyHash string, dryRun bool) (*Started, error) {
	tmpl, kind := PromoteTemplate, "promote"
	if p.rollback != nil {
		tmpl, kind = RollbackTemplate, "rollback"
	}
	t, err := c.Templates.Get(ctx, app.Namespace, tmpl)
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, Refused{fmt.Sprintf("%s has no '%s' TriggerTemplate yet (rendered by glidepath-app): %v", app.Namespace, tmpl, err),
			"the app's glidepath-app render must include it (a Kubernetes target)"}
	}
	if err != nil {
		return nil, err
	}
	res, _, _ := unstructured.NestedSlice(t, "spec", "resourcetemplates")
	var resource map[string]any
	if len(res) > 0 {
		resource, _ = res[0].(map[string]any)
	}
	if resource == nil || resource["kind"] != "PipelineRun" {
		return nil, Refused{fmt.Sprintf("%s/%s does not describe a PipelineRun", app.Namespace, tmpl), ""}
	}
	name := RunName(kind, p.env, p.gitRevision, keyHash)
	params := map[string]string{
		"pipeline-run-name": name, "env": p.env, "cluster": p.cluster, "git-url": p.gitURL,
		"git-revision": p.gitRevision, "image-ref": p.imageRef, "initiator": p.initiator,
	}
	if p.rollback != nil {
		params["rollback-of"], params["rollback-reason"], params["config-revision"] = p.rollback.of, p.rollback.reason, p.rollback.configRevision
	}
	out, err := Instantiate(resource, params, tmpl)
	if err != nil {
		return nil, err
	}
	run := out.(map[string]any)
	meta, _ := run["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["namespace"] = app.Namespace
	meta["name"] = name // the template's own $(tt.params.pipeline-run-name), so a template without it still gets one
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
	}
	ann[pipelineruns.AnnotationKeyHash] = keyHash
	// The origin the run's guard (verify-ref-protection) verifies: stamped here, by the API, from the commit this release is
	// built from (a release file or record the platform wrote) and the platform's configured protected branch; nothing the
	// caller sent. The guard checks the claim against the git host (the ref protected, the commit on it), so a rollback to an
	// older reviewed commit of that branch passes by design and a commit that never reached it is refused.
	if c.OriginRef != "" && p.gitRevision != "" {
		ann[pipelineruns.AnnotationOriginRef], ann[pipelineruns.AnnotationOriginSha] = c.OriginRef, p.gitRevision
	}
	// Promotion lineage: the build of the image this release carries, resolved here from the build's own run for the
	// repository and commit being released (never from the caller). Unresolved is not refused.
	var origin *lineage.Origin
	unknown := ""
	if c.Lineage != nil {
		res := c.resolveOrigin(ctx, app.Namespace, p.imageRef, p.gitRevision, p.digest)
		if res.Origin != nil && stampOrigin(ann, res.Origin) {
			origin = res.Origin
		} else if res.Origin != nil {
			unknown = "the resolved origin has a value outside the shape the cluster's admission accepts"
		} else {
			unknown = res.Unknown
		}
	}
	meta["annotations"] = ann
	run["metadata"] = meta

	err = c.Runs.Create(ctx, app.Namespace, run, dryRun)
	if origin != nil && lineageRefused(err) {
		// An older admission policy for this API's runs lists the annotations a run may carry and does not know these
		// yet: the release goes ahead without its lineage, never refused for it. Only that refusal is retried, and the run
		// resubmitted is the same run less the lineage annotations.
		for _, k := range lineageKeys {
			delete(ann, k)
		}
		origin, unknown = nil, "the cluster's admission policy for glidepath-api runs does not yet allow the lineage annotations"
		err = c.Runs.Create(ctx, app.Namespace, run, dryRun)
	}
	if apierrors.IsAlreadyExists(err) {
		found, gerr := c.Runs.Get(ctx, app.Namespace, name)
		if apierrors.IsNotFound(gerr) {
			return nil, pipelineruns.Conflict{What: fmt.Sprintf("PipelineRun %s existed and is now gone; retry with a new Idempotency-Key", name)}
		}
		if gerr != nil {
			return nil, gerr
		}
		a, _, _ := unstructured.NestedStringMap(found, "metadata", "annotations")
		if a[pipelineruns.AnnotationKeyHash] != keyHash {
			return nil, pipelineruns.Conflict{What: fmt.Sprintf("PipelineRun %s exists and is not this %s's run for this Idempotency-Key", name, kind)}
		}
		// A replay reports what the run it found carries, not what this call would resolve now.
		s := &Started{Name: name, Replayed: true}
		if a[pipelineruns.AnnotationOriginChainID] != "" {
			s.Origin = &Origin{ChainID: a[pipelineruns.AnnotationOriginChainID], ChainSlug: a[pipelineruns.AnnotationOriginChainSlug], TraceID: a[pipelineruns.AnnotationOriginTraceID]}
		}
		if dryRun {
			s.Object = found
		}
		return s, nil
	}
	if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return nil, pipelineruns.Refused{Err: err}
	}
	if err != nil {
		return nil, err
	}
	s := &Started{Name: name, Origin: originOf(origin), OriginUnknown: unknown}
	if dryRun {
		s.Object = run
	}
	return s, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// ExtractShortSha is Tower's extractShortSha: the trailing hex run of an image tag (<version>-<sha7>, or a bare sha)
// is the commit it was built from.
func ExtractShortSha(tag string) (string, error) {
	m := shortRE.FindStringSubmatch(tag)
	if m == nil {
		return "", Refused{fmt.Sprintf(`could not extract a git sha from image tag "%s" - unrecognized tagging convention`, tag), ""}
	}
	return m[1], nil
}

// lineageRefused reports an admission refusal (Forbidden or Invalid) that is about the annotations a run may carry: the older
// policy's allowlist message, or the newer one's lineage shape rule. Any other refusal, or any other error, is not retried.
func lineageRefused(err error) bool {
	if err == nil || !(apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "may carry only the annotations") || strings.Contains(m, "promotion-lineage")
}

// LineageTimeout is how long one promote or rollback waits for the origin lookup (Client.LineageTimeout overrides it).
const LineageTimeout = 5 * time.Second

// resolveOrigin is Lineage.Resolve under an overall deadline: the lookup can read several archived runs one after another, and a
// slow Results service must not stall a release it only annotates. The lookup runs on its own goroutine so a source that ignores
// its context cannot hold the request either.
func (c *Client) resolveOrigin(ctx context.Context, namespace, imageRef, commit, digest string) lineage.Resolution {
	d := c.LineageTimeout
	if d <= 0 {
		d = LineageTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	done := make(chan lineage.Resolution, 1)
	go func() { done <- c.Lineage.Resolve(ctx, namespace, imageRef, commit, digest) }()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		return lineage.Resolution{Unknown: "lineage lookup timed out"}
	}
}

// unknownText is the answer's "origin unknown: <why>", or "" when nothing was asked.
func unknownText(why string) string {
	if why == "" {
		return ""
	}
	return "origin unknown: " + why
}
