// Package changekind classifies a gitops pull request's head commit for the change-aware release gates (hangar
// specs/plans/change-aware-release-gates.md, CG-1; docs/admin/api.md "Change classification"): POST and GET
// /v1/apps/{app}/pulls/{n}/classification.
//
// The kind is worked out from the pull request's own diff, read through the GitHub API with the broker's per-request
// read-only token. It never reads the branch name, the pull request's text or commit trailers.
//
//   - image-release: an image reference in a deployable file changes (a new reference appears), and it is not an eligible rollback;
//   - rollback: every new image reference of an environment is its release image, returning to an image that ran healthy as
//     one of the last five healthy releases of that app, environment and cluster (ADR-0021 decision 8, ported unchanged from
//     the catalog's extract-promoted-image rollback-eligibility step);
//   - config: deployable files change semantically and no image reference is new;
//   - no-op: nothing deployable changes semantically (comments, whitespace, key order), or no deployable file changed.
//
// The pull request's kind is the highest kind of any environment it touches. Any doubt is image-release (fail closed): a
// file that does not parse, a comparison GitHub truncates at 300 files, a file too large to read, or a read that fails.
//
// One classification per head commit, computed once and then frozen: the answer is stored write-once in the ConfigMap
// change-kind-pr-<n> of the app's -cicd namespace, one key per head[:12]. Asks for the same head on one replica share one
// computation (a keyed single flight); two replicas that compute at once race on the write-once store and the first write
// wins for both. A fail-closed answer caused by a read that failed is returned but not stored, so a later ask computes it
// again; every other answer, fail-closed ones included, is stored. When the pull request has a release record, the stored
// answer is also copied onto it as changeKind.<sha12>.
package changekind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/environments"
	"github.com/glidepath/api/internal/releases"

	corev1 "k8s.io/api/core/v1"
)

// Kinds, highest precedence first.
const (
	ImageRelease = "image-release"
	Rollback     = "rollback"
	Config       = "config"
	NoOp         = "no-op"
)

// Render checks: whether only a render can tell if the environment's images changed (a chart pin, an own chart, a remote
// kustomize base). The render backstop in clearance-gate compares the rendered images (spec section 5.4).
const (
	RenderNone     = "none"
	RenderRequired = "required"
)

// Outcomes of a POST.
const (
	Computed = "computed" // computed by this call (or by the single flight it joined) and stored
	Stored   = "stored"   // already stored: nothing was computed
	Unstored = "unstored" // computed, but not stored (a read failed, or the store is full): ask again later
	DryRun   = "dry-run"  // computed (or read) and not stored, as asked
)

// Version is the classification format; Classifier names the implementation that produced it. changekind 2 (CG-3) adds each
// environment's fileClasses, the fact the gates narrow on; an answer stored by changekind 1 has none, and a gate then never
// narrows (it runs).
const (
	Version    = 1
	Classifier = "glidepath-api/changekind 2"
)

// File classes (CG-3): which kinds of deployable file changed in an environment. A gate whose matrix row may narrow from run to
// n/a reads them (glidepath-catalog's releaseGuardrails `narrow`, docs/admin/release-guardrails.md "Change kinds"): the values
// gate runs on a config change only when a values, release or base file changed, the predicate its own trigger has always used.
const (
	ClassValues        = "values"        // a path ending in values.yaml or release.yaml, or in /base.yaml: the values gate's trigger
	ClassKustomization = "kustomization" // a kustomization file
	ClassChart         = "chart"         // the template side of an own chart (not parsed)
	ClassManifest      = "manifest"      // any other deployable YAML or JSON
)

// Store keys and labels.
const (
	StorePrefix     = "change-kind-pr-"
	RecordKeyPrefix = "changeKind."
	labelComponent  = "hangar.io/subcomponent"
	componentValue  = "change-kind"
	labelApp        = "hangar.io/app"
	labelPR         = "hangar.io/pull-request"
	maxAttempts     = 5
	// A ConfigMap holds at most 1 MiB. Past this the answer is returned unstored.
	maxStoreBytes = 900 << 10
	// The computation outlives the call that started it (others may be waiting on it), but not by more than this.
	flightTimeout = 2 * time.Minute
)

var (
	shaRE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha12RE = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// ErrConflict is a write whose resourceVersion is no longer the object's.
var ErrConflict = errors.New("the ConfigMap changed underneath")

// ErrExists is a create of a ConfigMap that already exists.
var ErrExists = errors.New("the ConfigMap already exists")

// Invalid is a malformed request (400).
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

// NotFound is no such pull request, or no stored classification for that head (404).
type NotFound struct{ What string }

func (e NotFound) Error() string { return e.What }

// Refused is a request that cannot be answered as asked: the head is not a commit of the pull request (422).
type Refused struct{ What, Hint string }

func (e Refused) Error() string { return e.What }

// Reader is one bounded GET of a repo-relative path (github.Session.GetJSON).
type Reader interface {
	GetJSON(ctx context.Context, owner, repo, path string) (int, []byte, error)
}

// Records lists an app's release records for one environment and cluster (releases.Reader.ListEnv).
type Records interface {
	ListEnv(ctx context.Context, namespace, app, env, cluster string) ([]releases.Record, error)
}

// Store is the slice of the cluster the service reads and writes: the change-kind ConfigMaps and the release records.
type Store interface {
	Get(ctx context.Context, ns, name string) (*corev1.ConfigMap, error) // nil, nil when absent
	Create(ctx context.Context, ns string, cm *corev1.ConfigMap) error   // ErrExists when it exists
	// Patch merges data into the ConfigMap if its resourceVersion is still rv (ErrConflict otherwise).
	Patch(ctx context.Context, ns, name, rv string, data map[string]string) error
	Records(ctx context.Context, ns string) ([]corev1.ConfigMap, error)
}

// Classification is the stored answer (spec section 5.2, format v1).
type Classification struct {
	V           int    `json:"v"`
	Classifier  string `json:"classifier"`
	Repo        string `json:"repo"`
	PR          int    `json:"pr"`
	PrURL       string `json:"prUrl,omitempty"`
	Base        string `json:"base,omitempty"`
	Head        string `json:"head"`
	Kind        string `json:"kind"`
	Reason      string `json:"reason"`
	RenderCheck string `json:"renderCheck"`
	// FailClosed: the kind is image-release because of a doubt (Reason says which), not because an image changed.
	FailClosed bool   `json:"failClosed,omitempty"`
	ComputedAt string `json:"computedAt"`
	Envs       []Env  `json:"envs"`
}

// Env is one environment the pull request touches.
type Env struct {
	// Cluster and Env name the environment. A file at <cluster>/<name> (base.yaml) that feeds a cluster with no environment
	// directory has Env "*"; a deployable file at the repository's root has Cluster and Env "*".
	Cluster     string    `json:"cluster"`
	Env         string    `json:"env"`
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason"`
	RenderCheck string    `json:"renderCheck"`
	Images      ImageSets `json:"images"`
	Files       []string  `json:"files"`
	// FileClasses are the classes of the files in Files (values, kustomization, chart, manifest), sorted, each once; both paths of a
	// renamed file count. Always present from changekind 2 on.
	FileClasses []string `json:"fileClasses"`
	// Changed lists the configuration paths that changed (<file>: <path>), at most maxChanged.
	Changed  []string       `json:"changed,omitempty"`
	Rollback *RollbackCheck `json:"rollback,omitempty"`
}

// ImageSets are the distinct image references the environment's changed files hold, before and after.
type ImageSets struct {
	Base []string `json:"base"`
	Head []string `json:"head"`
}

// RollbackCheck is the rollback-eligibility answer for an environment whose only new image reference is its release image.
type RollbackCheck struct {
	Eligible bool   `json:"eligible"`
	Image    string `json:"image"`
	Note     string `json:"note"`
}

// Answer is what the routes return: the classification and how this call got it.
type Answer struct {
	Classification
	Outcome string `json:"outcome"`
	// Record is the release record the classification was copied onto ("" when the pull request has none, or on a GET).
	Record string `json:"record,omitempty"`
	// RecordCopy says what happened to that copy: copied, already-copied, no-record, or failed: <why> (never fails the call).
	RecordCopy string `json:"recordCopy,omitempty"`
	// UnstoredReason says why an unstored answer was not stored: read-failed (a later ask computes it again) or store-full
	// (the pull request's store has no room left; every later ask for this head computes it again and fails closed the same way).
	UnstoredReason string `json:"unstoredReason,omitempty"`
}

// Service answers the routes.
type Service struct {
	Owner   string                  // the GitHub owner of every app repo (the chart's githubOwner)
	GitHub  func(app string) Reader // one request's reads for the app (github.Broker.Session)
	Records Records                 // release records, for rollback eligibility
	Store   Store
	Now     func() time.Time

	mu      sync.Mutex
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
	a    *Answer
	err  error
}

// StoreName is the ConfigMap that holds pull request n's classifications.
func StoreName(n int) string { return StorePrefix + strconv.Itoa(n) }

// RecordKey is the release record key the classification of head is copied to.
func RecordKey(head string) string { return RecordKeyPrefix + head[:12] }

func validate(n int, head string) error {
	if n < 1 {
		return Invalid{"the pull request number must be a positive integer"}
	}
	if !shaRE.MatchString(head) {
		return Invalid{"head must be the full 40-character lowercase commit sha"}
	}
	return nil
}

// ParseNumber reads a pull request number from a path segment.
func ParseNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 1_000_000_000 || strconv.Itoa(n) != s {
		return 0, Invalid{"the pull request number must be a positive integer"}
	}
	return n, nil
}

// Get is GET: the stored classification of pull request n at head, or NotFound.
func (s *Service) Get(ctx context.Context, app *apps.App, n int, head string) (*Answer, error) {
	if err := validate(n, head); err != nil {
		return nil, err
	}
	c, err := s.stored(ctx, app.Namespace, n, head)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, NotFound{fmt.Sprintf("no classification is stored for pull request #%d at %s: POST computes it", n, head[:12])}
	}
	return &Answer{Classification: *c, Outcome: Stored}, nil
}

// Classify is POST: the stored classification of pull request n at head, computed and stored on the first ask. dry computes
// (or reads) and writes nothing.
func (s *Service) Classify(ctx context.Context, app *apps.App, n int, head string, dry bool) (*Answer, error) {
	if err := validate(n, head); err != nil {
		return nil, err
	}
	c, err := s.stored(ctx, app.Namespace, n, head)
	if err != nil {
		return nil, err
	}
	var a *Answer
	switch {
	case c != nil:
		a = &Answer{Classification: *c, Outcome: Stored}
	case dry:
		cl, _, err := s.compute(ctx, app, n, head)
		if err != nil {
			return nil, err
		}
		return &Answer{Classification: *cl, Outcome: DryRun}, nil
	default:
		if a, err = s.shared(ctx, app, n, head); err != nil {
			return nil, err
		}
	}
	if dry || a.Outcome == Unstored {
		return a, nil
	}
	// The release record may be written after the first ask (open-release-pr writes it once the PR exists, while the gates
	// already start), so every ask that has a stored answer makes sure the copy is there. Best effort: never fails the call.
	a.Record, a.RecordCopy = s.copyToRecord(ctx, app.Namespace, &a.Classification)
	return a, nil
}

// shared runs one computation per (namespace, pull request, head) on this replica: concurrent asks wait for the first.
func (s *Service) shared(ctx context.Context, app *apps.App, n int, head string) (*Answer, error) {
	key := app.Namespace + "/" + strconv.Itoa(n) + "/" + head
	s.mu.Lock()
	if s.flights == nil {
		s.flights = map[string]*flight{}
	}
	f, joined := s.flights[key]
	if !joined {
		f = &flight{done: make(chan struct{})}
		s.flights[key] = f
	}
	s.mu.Unlock()
	if !joined {
		// Detached from the caller: the others waiting on it must not fail because the first caller went away.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightTimeout)
		go func() {
			defer cancel()
			f.a, f.err = s.computeAndStore(fctx, app, n, head)
			s.mu.Lock()
			delete(s.flights, key)
			s.mu.Unlock()
			close(f.done)
		}()
	}
	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		a := *f.a // each caller gets its own copy (the handler fills Record and RecordCopy)
		return &a, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) computeAndStore(ctx context.Context, app *apps.App, n int, head string) (*Answer, error) {
	// Another replica (or a flight that just finished here) may have stored it since this call looked.
	if c, err := s.stored(ctx, app.Namespace, n, head); err != nil || c != nil {
		if err != nil {
			return nil, err
		}
		return &Answer{Classification: *c, Outcome: Stored}, nil
	}
	c, transient, err := s.compute(ctx, app, n, head)
	if err != nil {
		return nil, err
	}
	if transient {
		// Another replica may have stored this head meanwhile: once one answer is stored, every ask gets that one.
		if stored, err := s.stored(ctx, app.Namespace, n, head); err == nil && stored != nil {
			return &Answer{Classification: *stored, Outcome: Stored}, nil
		}
		return &Answer{Classification: *c, Outcome: Unstored, UnstoredReason: "read-failed"}, nil
	}
	return s.put(ctx, app, n, c)
}

// stored reads the classification of head from the store, nil when there is none.
func (s *Service) stored(ctx context.Context, ns string, n int, head string) (*Classification, error) {
	cm, err := s.Store.Get(ctx, ns, StoreName(n))
	if err != nil || cm == nil {
		return nil, err
	}
	return decodeStored(cm.Data[head[:12]], head)
}

func decodeStored(raw, head string) (*Classification, error) {
	if raw == "" {
		return nil, nil
	}
	var c Classification
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("the stored classification of %s does not parse: %w", head[:12], err)
	}
	if c.Head != head {
		return nil, fmt.Errorf("the stored classification under %s is for %s, not %s", head[:12], c.Head, head)
	}
	return &c, nil
}

// put stores c write-once. When another writer stored this head first, its answer is returned instead.
func (s *Service) put(ctx context.Context, app *apps.App, n int, c *Classification) (*Answer, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	key, name := c.Head[:12], StoreName(n)
	full := func() (*Answer, error) {
		// Not stored: an image-release answer that says why, never a write Kubernetes would refuse (1 MiB per ConfigMap).
		f := *c
		if f.Kind != ImageRelease {
			f.Kind, f.FailClosed, f.Envs, f.RenderCheck = ImageRelease, true, []Env{}, RenderNone
			f.Reason = "fail closed: the classification does not fit in the pull request's store (change-kind-pr-" + strconv.Itoa(n) + ")"
		}
		return &Answer{Classification: f, Outcome: Unstored, UnstoredReason: "store-full"}, nil
	}
	if len(raw) > maxStoreBytes {
		return full()
	}
	for attempt := 1; ; attempt++ {
		cm, err := s.Store.Get(ctx, app.Namespace, name)
		if err != nil {
			return nil, err
		}
		if cm == nil {
			err = s.Store.Create(ctx, app.Namespace, &corev1.ConfigMap{
				ObjectMeta: objectMeta(name, app.Name, n),
				Data:       map[string]string{key: string(raw)},
			})
		} else {
			if prev, derr := decodeStored(cm.Data[key], c.Head); derr != nil || prev != nil {
				if derr != nil {
					return nil, derr
				}
				return &Answer{Classification: *prev, Outcome: Stored}, nil
			}
			size := len(raw)
			for k, v := range cm.Data {
				size += len(k) + len(v)
			}
			if size > maxStoreBytes {
				return full()
			}
			err = s.Store.Patch(ctx, app.Namespace, name, cm.ResourceVersion, map[string]string{key: string(raw)})
		}
		switch {
		case err == nil:
			return &Answer{Classification: *c, Outcome: Computed}, nil
		case (errors.Is(err, ErrConflict) || errors.Is(err, ErrExists)) && attempt < maxAttempts:
			continue
		default:
			return nil, err
		}
	}
}

// copyToRecord copies c onto the release record whose prUrl is the pull request's, once. It never fails the call.
func (s *Service) copyToRecord(ctx context.Context, ns string, c *Classification) (string, string) {
	if c.PrURL == "" {
		return "", "no-record"
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", "failed: " + err.Error()
	}
	key := RecordKey(c.Head)
	for attempt := 1; ; attempt++ {
		recs, err := s.Store.Records(ctx, ns)
		if err != nil {
			return "", "failed: " + err.Error()
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
		var rec *corev1.ConfigMap
		for i := range recs {
			if recs[i].Data["prUrl"] == c.PrURL {
				rec = &recs[i]
				break
			}
		}
		if rec == nil {
			return "", "no-record"
		}
		if rec.Data[key] != "" {
			return rec.Name, "already-copied"
		}
		err = s.Store.Patch(ctx, ns, rec.Name, rec.ResourceVersion, map[string]string{key: string(raw)})
		switch {
		case err == nil:
			return rec.Name, "copied"
		case errors.Is(err, ErrConflict) && attempt < maxAttempts:
			continue
		default:
			return rec.Name, "failed: " + err.Error()
		}
	}
}

// repos is the app's gitops repository under the platform's owner: the only repository a classification reads.
func (s *Service) repo(app *apps.App) (string, string, error) {
	g := environments.ReposOf(app).Gitops
	if s.Owner == "" || g == "" {
		return "", "", Invalid{"this glidepath-api has no GitHub owner configured (the chart's githubOwner)"}
	}
	return s.Owner, g, nil
}
