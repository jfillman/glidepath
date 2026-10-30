// Package live reads PipelineRuns, TaskRuns and step logs as the cluster holds them right now: the
// runs still in flight, and the hour or so a finished run stays before Tekton deletes it. Archived
// runs come from Tekton Results (internal/results); this is everything Results doesn't have yet.
//
// It replaces Tower's browser hooks that read the Kubernetes API directly through its proxy
// (spec phase 6b: useTektonPipelineRuns, useTaskRunLogs, useFleetPipelineRuns), so its answers are
// the cluster's objects as Tower listed them, with metadata.managedFields dropped and nothing else
// changed. Lists are cached for ListTTL at most: Tower polls every few seconds per open view, and
// several people watching the fleet view should cost the API server one list, not one each.
package live

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	// ListTTL: Tower polls an app's runs every 6 s and the fleet every 10 s; 2 s keeps an answer
	// fresher than either poll while coalescing viewers.
	ListTTL = 2 * time.Second
	// MaxPipelineRuns: how many PipelineRun names one fleet TaskRun read may ask for.
	MaxPipelineRuns = 200
	// LabelPipelineRun is the label Tekton puts on every TaskRun of a PipelineRun.
	LabelPipelineRun = "tekton.dev/pipelineRun"
)

var (
	GVRPipelineRuns = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}
	GVRTaskRuns     = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}

	objName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`) // DNS-1123 subdomain
	// A PipelineRun name that fits a label value (63 characters): TaskRuns are found by label, and a
	// longer name matches none.
	labelValue = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)
	container  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`) // DNS-1123 label
)

// NotFound: the TaskRun, or the pod that ran it, is gone (or has no pod yet).
type NotFound struct{ What string }

func (e NotFound) Error() string { return e.What }

// Invalid: a malformed name, a container the pod doesn't have, or one with no log yet.
type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

type Obj = map[string]any

// Store is the slice of the Kubernetes API this package needs; tests swap it. namespace "" lists
// every namespace; selector "" selects everything.
type Store interface {
	List(ctx context.Context, gvr schema.GroupVersionResource, namespace, selector string) ([]Obj, error)
	Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (Obj, error)
	Containers(ctx context.Context, namespace, pod string) ([]string, error)
	Log(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error)
}

type kubeStore struct {
	dyn dynamic.Interface
	cs  kubernetes.Interface
}

func NewKubeStore(dyn dynamic.Interface, cs kubernetes.Interface) Store { return kubeStore{dyn, cs} }

func (k kubeStore) List(ctx context.Context, gvr schema.GroupVersionResource, namespace, selector string) ([]Obj, error) {
	l, err := k.dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	out := make([]Obj, 0, len(l.Items))
	for _, u := range l.Items {
		out = append(out, u.Object)
	}
	return out, nil
}

func (k kubeStore) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (Obj, error) {
	u, err := k.dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return u.Object, nil
}

func (k kubeStore) Containers(ctx context.Context, namespace, pod string) ([]string, error) {
	p, err := k.cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range append(p.Spec.InitContainers, p.Spec.Containers...) {
		out = append(out, c.Name)
	}
	return out, nil
}

func (k kubeStore) Log(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
	return k.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Timestamps: true}).Stream(ctx)
}

// Client reads live runs.
type Client struct {
	Store Store

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	objs []Obj
	exp  time.Time
}

// stripped copies o without metadata.managedFields, so a cached list is never changed by a caller.
func stripped(o Obj) Obj {
	out := make(Obj, len(o))
	for k, v := range o {
		out[k] = v
	}
	if meta, ok := o["metadata"].(map[string]any); ok {
		m := make(map[string]any, len(meta))
		for k, v := range meta {
			if k != "managedFields" {
				m[k] = v
			}
		}
		out["metadata"] = m
	}
	return out
}

func (c *Client) list(ctx context.Context, gvr schema.GroupVersionResource, namespace, selector string) ([]Obj, error) {
	key := gvr.Resource + "|" + namespace + "|" + selector
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.objs, nil
	}
	c.mu.Unlock()
	objs, err := c.Store.List(ctx, gvr, namespace, selector)
	if err != nil {
		return nil, err
	}
	out := make([]Obj, 0, len(objs))
	for _, o := range objs {
		out = append(out, stripped(o))
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]cached{}
	}
	for k, e := range c.cache { // only a handful of live keys at a time: one per app being watched
		if now.After(e.exp) {
			delete(c.cache, k)
		}
	}
	c.cache[key] = cached{out, now.Add(ListTTL)}
	c.mu.Unlock()
	return out, nil
}

// PipelineRuns lists the PipelineRuns in namespace ("" = every namespace).
func (c *Client) PipelineRuns(ctx context.Context, namespace string) ([]Obj, error) {
	return c.list(ctx, GVRPipelineRuns, namespace, "")
}

// TaskRuns lists the TaskRuns in namespace.
func (c *Client) TaskRuns(ctx context.Context, namespace string) ([]Obj, error) {
	return c.list(ctx, GVRTaskRuns, namespace, "")
}

// ParseNames reads a comma-separated list of PipelineRun names: each a valid label value (63 characters at most), at most
// MaxPipelineRuns, duplicates and empty entries dropped, sorted (so the same set is one cache entry).
func ParseNames(v string) ([]string, error) {
	var out []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if !labelValue.MatchString(n) {
			return nil, Invalid{fmt.Sprintf("invalid PipelineRun name '%s'", n)}
		}
		out = append(out, n)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxPipelineRuns {
		return nil, Invalid{fmt.Sprintf("at most %d PipelineRun names per call, got %d", MaxPipelineRuns, len(out))}
	}
	return out, nil
}

// TaskRunsOf lists the TaskRuns in namespace ("" = every namespace) of the named PipelineRuns, by
// their tekton.dev/pipelineRun label. No names: none.
func (c *Client) TaskRunsOf(ctx context.Context, namespace string, pipelineRuns []string) ([]Obj, error) {
	if len(pipelineRuns) == 0 {
		return []Obj{}, nil
	}
	return c.list(ctx, GVRTaskRuns, namespace, LabelPipelineRun+" in ("+strings.Join(pipelineRuns, ",")+")")
}

// Log opens the log of one container of the pod that runs taskRun in namespace, with the runtime's
// timestamps, as kubectl logs --timestamps would. Never cached: it is the live tail.
func (c *Client) Log(ctx context.Context, namespace, taskRun, ctr string) (io.ReadCloser, error) {
	if !objName.MatchString(taskRun) {
		return nil, Invalid{fmt.Sprintf("invalid TaskRun name '%s'", taskRun)}
	}
	if !container.MatchString(ctr) {
		return nil, Invalid{fmt.Sprintf("invalid container name '%s'", ctr)}
	}
	tr, err := c.Store.Get(ctx, GVRTaskRuns, namespace, taskRun)
	if apierrors.IsNotFound(err) {
		return nil, NotFound{fmt.Sprintf("no TaskRun '%s' in %s: it finished and was deleted (read it from GET /v1/apps/{app}/runs), or never existed", taskRun, namespace)}
	}
	if err != nil {
		return nil, err
	}
	status, _ := tr["status"].(map[string]any)
	pod, _ := status["podName"].(string)
	if pod == "" {
		return nil, NotFound{fmt.Sprintf("TaskRun '%s' has no pod yet", taskRun)}
	}
	ctrs, err := c.Store.Containers(ctx, namespace, pod)
	if apierrors.IsNotFound(err) {
		return nil, NotFound{fmt.Sprintf("the pod of TaskRun '%s' (%s) is gone", taskRun, pod)}
	}
	if err != nil {
		return nil, err
	}
	if !slices.Contains(ctrs, ctr) {
		return nil, Invalid{fmt.Sprintf("pod %s has no container '%s' (it has %s)", pod, ctr, strings.Join(ctrs, ", "))}
	}
	rc, err := c.Store.Log(ctx, namespace, pod, ctr)
	switch {
	case apierrors.IsNotFound(err):
		return nil, NotFound{fmt.Sprintf("the pod of TaskRun '%s' (%s) is gone", taskRun, pod)}
	case apierrors.IsBadRequest(err):
		// The kubelet's "container ... is waiting to start": a step that hasn't run has no log yet.
		return nil, Invalid{fmt.Sprintf("no log for %s yet: %s", ctr, err.Error())}
	}
	return rc, err
}
