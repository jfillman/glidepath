package changekind

import (
	"context"
	"encoding/json"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// KubeStore is the Store on the cluster, written as glidepath-api's own ServiceAccount: glidepath-app's Roles
// glidepath-api-change-kind (configmaps create) and glidepath-api-gate-ledger (configmaps patch) in the app's -cicd namespace.
// The control plane's admission policies narrow both: change-kind-writes lets only glidepath-api create or add a key to a
// change-kind-pr-<n> ConfigMap (and never change or remove one), and gate-ledger-writes lets it add changeKind.<sha12> to a
// release record once.
type KubeStore struct {
	CS kubernetes.Interface
}

const recordLabel = "hangar.io/subcomponent=release-tracking"

func objectMeta(name, app string, n int) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Labels: map[string]string{
		labelComponent: componentValue,
		labelApp:       app,
		labelPR:        strconv.Itoa(n),
	}}
}

func (k KubeStore) Get(ctx context.Context, ns, name string) (*corev1.ConfigMap, error) {
	cm, err := k.CS.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return cm, err
}

func (k KubeStore) Create(ctx context.Context, ns string, cm *corev1.ConfigMap) error {
	_, err := k.CS.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{FieldManager: "glidepath-api-change-kind"})
	if apierrors.IsAlreadyExists(err) {
		return ErrExists
	}
	return err
}

func (k KubeStore) Patch(ctx context.Context, ns, name, rv string, data map[string]string) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": rv}, "data": data})
	if err != nil {
		return err
	}
	_, err = k.CS.CoreV1().ConfigMaps(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{FieldManager: "glidepath-api-change-kind"})
	if apierrors.IsConflict(err) {
		return ErrConflict
	}
	return err
}

func (k KubeStore) Records(ctx context.Context, ns string) ([]corev1.ConfigMap, error) {
	l, err := k.CS.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{LabelSelector: recordLabel})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}
