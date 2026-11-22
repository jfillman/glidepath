package gateverdict

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	pipelineRuns = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}
	taskRuns     = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}
)

// KubeStore is the Store on the cluster: PipelineRuns and TaskRuns through the dynamic client, pods and the release records
// through the typed one. The record patch is made as glidepath-api's own ServiceAccount (glidepath-app's Role
// glidepath-api-gate-ledger: configmaps patch in the app's -cicd namespace; the control plane's gate-ledger-writes policy
// limits it to the ledger keys of an existing release record).
type KubeStore struct {
	Dyn dynamic.Interface
	CS  kubernetes.Interface
}

func notFound(err error, what string) error {
	if apierrors.IsNotFound(err) {
		return NotFound{what + " not found"}
	}
	return err
}

func (k KubeStore) PipelineRun(ctx context.Context, ns, name string) (map[string]any, error) {
	u, err := k.Dyn.Resource(pipelineRuns).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, notFound(err, "PipelineRun "+ns+"/"+name)
	}
	return u.Object, nil
}

func (k KubeStore) TaskRun(ctx context.Context, ns, name string) (map[string]any, error) {
	u, err := k.Dyn.Resource(taskRuns).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, notFound(err, "TaskRun "+ns+"/"+name)
	}
	return u.Object, nil
}

func (k KubeStore) TaskRuns(ctx context.Context, ns, pipelineRun string) ([]map[string]any, error) {
	l, err := k.Dyn.Resource(taskRuns).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: labelPipelineRun + "=" + pipelineRun})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(l.Items))
	for _, it := range l.Items {
		out = append(out, it.Object)
	}
	return out, nil
}

func (k KubeStore) Pod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	p, err := k.CS.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, notFound(err, "pod "+ns+"/"+name)
	}
	return p, nil
}

func (k KubeStore) Records(ctx context.Context, ns string) ([]corev1.ConfigMap, error) {
	l, err := k.CS.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{LabelSelector: recordLabel})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (k KubeStore) PatchRecord(ctx context.Context, ns, name, rv string, data map[string]string) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": rv}, "data": data})
	if err != nil {
		return err
	}
	_, err = k.CS.CoreV1().ConfigMaps(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{FieldManager: "glidepath-api-gate-verdict"})
	if apierrors.IsConflict(err) {
		return ErrConflict
	}
	return err
}
