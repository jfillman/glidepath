// Package environments is glidepath-api's tier T2 writes (docs/admin/api.md#promote-and-roll-back): promote an app's
// release to its next Flight environment, and roll a Flight environment back to an earlier release, plus the two
// reads they rest on (the release plan and the rollback plan). It replaces Tower's own (backstage
// packages/backend/src/glidepathPromote.ts, glidepathReleaseRun.ts, glidepathRollback.ts, releasePlan.ts), ported so
// Tower can shadow-compare the answers before its Promote and Roll back move here.
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

	"github.com/glidepath/api/internal/apps"
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
	Owner     string                  // the GitHub owner of every app repo (the chart's githubOwner)
	GitHub    func(app string) GitHub // a new Session per request
	Templates Templates               //
	Runs      pipelineruns.Store      //
	Records   interface {             // release records for one environment and cluster
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
	rollback                                               *rollbackParams // set for a rollback
}

// rollbackParams: the release-id a rollback replaces, why, and the app-repo revision to read cicd.yaml at.
type rollbackParams struct{ of, reason, configRevision string }

// Started is a release run's answer.
type Started struct {
	Name     string
	Replayed bool
	Object   map[string]any // dry run: the PipelineRun it creates (or found, on a replay)
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
	meta["annotations"] = ann
	run["metadata"] = meta

	err = c.Runs.Create(ctx, app.Namespace, run, dryRun)
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
		s := &Started{Name: name, Replayed: true}
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
	s := &Started{Name: name}
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
