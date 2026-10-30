// Package pipelineruns is glidepath-api's first write: re-run and cancel a PipelineRun in the app's
// -cicd namespace (docs/admin/api.md#writes). It replaced Tower's own (backstage
// packages/backend/src/tektonActions.ts, removed in P6) and builds the same objects; Tower's Re-run
// and Cancel call it.
//
// A re-run is idempotent across retries and replicas without shared memory: the caller sends an
// Idempotency-Key, and the new run's name is derived from it. A second create with the same key hits
// AlreadyExists, and the run it finds is answered as a replay when its annotations say it is the
// re-run of the same source with the same key.
package pipelineruns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const (
	// CancelStatus is the status Tower's Cancel always sent: CancelledRunFinally still runs the
	// pipeline's `finally` (notify-backstage, notify-slack), so a cancel is announced like a failure.
	CancelStatus = "CancelledRunFinally"

	AnnotationRerunOf = "glidepath.hangar.io/rerun-of"
	AnnotationKeyHash = "glidepath.hangar.io/idempotency-key-sha256"

	// maxGeneratedPrefix is the API server's own limit on a generateName prefix (63 - 5 random
	// characters), kept so a re-run's name is as long as the one Tower's generateName got.
	maxGeneratedPrefix = 58
)

// Kept labels: hangar.io/app + /flow (or the pre-rebrand platform.io pair), so the run shows in the CI list.
var keptLabels = []string{"hangar.io/app", "hangar.io/flow", "platform.io/app", "platform.io/flow"}

var (
	runName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`)
	keyRE   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

var GVR = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}

type NotFound struct{ What string }

func (e NotFound) Error() string { return e.What }

type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

// Conflict: the name a re-run derives from its key is taken by something that is not that re-run.
type Conflict struct{ What string }

func (e Conflict) Error() string { return e.What }

// Refused: the API server refused the write itself (an admission policy, or RBAC), not a failure to
// retry.
type Refused struct{ Err error }

func (e Refused) Error() string { return "the cluster refused the write: " + e.Err.Error() }
func (e Refused) Unwrap() error { return e.Err }

// Store is the slice of the Kubernetes API this package needs; tests swap it. dryRun is a
// server-side dry run (dryRun: All): admitted and validated, never persisted.
type Store interface {
	Get(ctx context.Context, namespace, name string) (map[string]any, error)
	Create(ctx context.Context, namespace string, obj map[string]any, dryRun bool) error
	Patch(ctx context.Context, namespace, name string, mergePatch []byte, dryRun bool) error
}

type kubeStore struct{ dyn dynamic.Interface }

func NewKubeStore(dyn dynamic.Interface) Store { return kubeStore{dyn} }

func dryRunOpt(dryRun bool) []string {
	if dryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

func (k kubeStore) Get(ctx context.Context, namespace, name string) (map[string]any, error) {
	u, err := k.dyn.Resource(GVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return u.Object, nil
}

func (k kubeStore) Create(ctx context.Context, namespace string, obj map[string]any, dryRun bool) error {
	_, err := k.dyn.Resource(GVR).Namespace(namespace).Create(ctx, &unstructured.Unstructured{Object: obj},
		metav1.CreateOptions{DryRun: dryRunOpt(dryRun), FieldManager: "glidepath-api"})
	return err
}

func (k kubeStore) Patch(ctx context.Context, namespace, name string, mergePatch []byte, dryRun bool) error {
	_, err := k.dyn.Resource(GVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, mergePatch,
		metav1.PatchOptions{DryRun: dryRunOpt(dryRun), FieldManager: "glidepath-api"})
	return err
}

// Client re-runs and cancels PipelineRuns.
type Client struct{ Store Store }

func str(m map[string]any, path ...string) string {
	v, _, _ := unstructured.NestedString(m, path...)
	return v
}

// Body is Tower's rerunBody, ported: a new PipelineRun with the original's spec. Tekton has no
// "restart", only "submit the same spec again" (what `tkn pipelinerun start --use-pipelinerun` does).
// Only the kept labels survive; the Pipelines-as-Code annotations and trigger label are dropped, since
// PaC would report a commit status for a trigger this run is not. spec.status is dropped too:
// re-running a cancelled run must not start it cancelled.
func Body(run map[string]any) map[string]any {
	labels := map[string]any{}
	orig, _, _ := unstructured.NestedStringMap(run, "metadata", "labels")
	for k, v := range orig {
		if slices.Contains(keptLabels, k) {
			labels[k] = v
		}
	}
	spec := map[string]any{}
	if s, ok := run["spec"].(map[string]any); ok {
		for k, v := range s {
			if k != "status" {
				spec[k] = v
			}
		}
	}
	return map[string]any{
		"apiVersion": "tekton.dev/v1",
		"kind":       "PipelineRun",
		"metadata": map[string]any{
			"generateName": str(run, "metadata", "name") + "-rerun-",
			"namespace":    str(run, "metadata", "namespace"),
			"labels":       labels,
		},
		"spec": spec,
	}
}

// KeyHash is sha256(app/name/key), hex: what a re-run's name and annotation are derived from.
func KeyHash(app, name, key string) string {
	sum := sha256.Sum256([]byte(app + "/" + name + "/" + key))
	return hex.EncodeToString(sum[:])
}

// RerunName is the re-run's name: Tower's generateName prefix, truncated as the API server truncates
// it, plus the first 5 hex characters of the key hash in place of its 5 random ones.
func RerunName(name, keyHash string) string {
	prefix := name + "-rerun-"
	if len(prefix) > maxGeneratedPrefix {
		prefix = prefix[:maxGeneratedPrefix]
	}
	return prefix + keyHash[:5]
}

// ValidName checks a PipelineRun name, the same pattern Tower's route checks.
func ValidName(name string) error {
	if !runName.MatchString(name) {
		return Invalid{fmt.Sprintf("invalid PipelineRun name '%s'", name)}
	}
	return nil
}

// ValidKey checks an Idempotency-Key.
func ValidKey(key string) error {
	if !keyRE.MatchString(key) {
		return Invalid{"Idempotency-Key must be 1-128 characters of [A-Za-z0-9._:-]"}
	}
	return nil
}

// Rerun is a re-run's answer.
type Rerun struct {
	Name     string         `json:"name"`
	Replayed bool           `json:"replayed"`
	DryRun   bool           `json:"dryRun,omitempty"`
	Object   map[string]any `json:"object,omitempty"` // dry run: what the API creates (or found, on a replay)
}

// replayOf reports whether obj is the re-run of name under keyHash.
func replayOf(obj map[string]any, name, keyHash string) bool {
	ann, _, _ := unstructured.NestedStringMap(obj, "metadata", "annotations")
	return ann[AnnotationRerunOf] == name && ann[AnnotationKeyHash] == keyHash
}

// existing answers a re-run whose name is taken: a replay when it is this re-run, else a Conflict.
func (c *Client) existing(ctx context.Context, namespace, name, keyHash, newName string, dryRun bool) (*Rerun, error) {
	obj, err := c.Store.Get(ctx, namespace, newName)
	if apierrors.IsNotFound(err) {
		// Deleted between the create and this read: the create did happen once.
		return nil, Conflict{fmt.Sprintf("PipelineRun %s existed and is now gone; retry with a new Idempotency-Key", newName)}
	}
	if err != nil {
		return nil, err
	}
	if !replayOf(obj, name, keyHash) {
		return nil, Conflict{fmt.Sprintf("PipelineRun %s exists and is not the re-run of %s for this Idempotency-Key", newName, name)}
	}
	r := &Rerun{Name: newName, Replayed: true, DryRun: dryRun}
	if dryRun {
		r.Object = obj
	}
	return r, nil
}

// Rerun starts a copy of namespace/name, once per key. The bool is true when this call created it.
func (c *Client) Rerun(ctx context.Context, namespace, app, name, key string, dryRun bool) (*Rerun, bool, error) {
	if err := ValidName(name); err != nil {
		return nil, false, err
	}
	if err := ValidKey(key); err != nil {
		return nil, false, err
	}
	keyHash := KeyHash(app, name, key)
	newName := RerunName(name, keyHash)
	run, err := c.Store.Get(ctx, namespace, name)
	if apierrors.IsNotFound(err) {
		// The source may be pruned after its re-run was made (finished runs go after about an hour), so
		// a retry still answers with the run it made.
		r, rerr := c.existing(ctx, namespace, name, keyHash, newName, dryRun)
		if rerr == nil {
			return r, false, nil
		}
		if !errors.As(rerr, new(Conflict)) {
			return nil, false, rerr
		}
		return nil, false, NotFound{fmt.Sprintf("no PipelineRun %s in %s", name, namespace)}
	}
	if err != nil {
		return nil, false, err
	}
	body := Body(run)
	meta := body["metadata"].(map[string]any)
	delete(meta, "generateName")
	meta["name"] = newName
	meta["annotations"] = map[string]any{AnnotationRerunOf: name, AnnotationKeyHash: keyHash}
	err = c.Store.Create(ctx, namespace, body, dryRun)
	if apierrors.IsAlreadyExists(err) {
		r, err := c.existing(ctx, namespace, name, keyHash, newName, dryRun)
		return r, false, err
	}
	if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return nil, false, Refused{err}
	}
	if err != nil {
		return nil, false, err
	}
	r := &Rerun{Name: newName, DryRun: dryRun}
	if dryRun {
		r.Object = body
	}
	return r, !dryRun, nil
}

// Cancel states.
const (
	Cancelled        = "cancelled"
	AlreadyCancelled = "already-cancelled"
	AlreadyFinished  = "already-finished"
)

// Cancellation is a cancel's answer.
type Cancellation struct {
	Name   string         `json:"name"`
	Status string         `json:"status"`
	DryRun bool           `json:"dryRun,omitempty"`
	Patch  map[string]any `json:"patch,omitempty"` // dry run: the merge patch the API sends, when it sends one
}

// cancelledReasons are the Succeeded condition reasons Tekton gives a cancelled run.
var cancelledReasons = []string{"Cancelled", "PipelineRunCancelled", "CancelledRunningFinally"}

// state decides a cancel from the run alone: spec.status already cancelling, or a Succeeded
// condition that is no longer Unknown.
func state(run map[string]any) string {
	if s := str(run, "spec", "status"); s == "Cancelled" || s == CancelStatus {
		return AlreadyCancelled
	}
	conds, _, _ := unstructured.NestedSlice(run, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] != "Succeeded" {
			continue
		}
		if slices.Contains(cancelledReasons, fmt.Sprint(m["reason"])) {
			return AlreadyCancelled
		}
		if st := fmt.Sprint(m["status"]); st == "True" || st == "False" {
			return AlreadyFinished
		}
	}
	return ""
}

// Cancel sets spec.status to CancelledRunFinally on namespace/name, unless it is already cancelled or
// finished, which is answered, not refused.
func (c *Client) Cancel(ctx context.Context, namespace, name string, dryRun bool) (*Cancellation, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	run, err := c.Store.Get(ctx, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, NotFound{fmt.Sprintf("no PipelineRun %s in %s", name, namespace)}
	}
	if err != nil {
		return nil, err
	}
	if s := state(run); s != "" {
		return &Cancellation{Name: name, Status: s, DryRun: dryRun}, nil
	}
	patch := map[string]any{"spec": map[string]any{"status": CancelStatus}}
	raw, _ := json.Marshal(patch)
	err = c.Store.Patch(ctx, namespace, name, raw, dryRun)
	if apierrors.IsNotFound(err) {
		return nil, NotFound{fmt.Sprintf("no PipelineRun %s in %s", name, namespace)}
	}
	if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return nil, Refused{err}
	}
	if err != nil {
		return nil, err
	}
	out := &Cancellation{Name: name, Status: Cancelled, DryRun: dryRun}
	if dryRun {
		out.Patch = patch
	}
	return out, nil
}
