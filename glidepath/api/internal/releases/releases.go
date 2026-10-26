// Package releases reads an app's release records: one release-tracking-<chain-id> ConfigMap per
// release to an upper environment, in the app's -cicd namespace (docs/admin/release-state.md).
// The shape, {name, data}, is the one Tower's readReleaseTracking returns, so Tower can shadow it.
package releases

import (
	"context"
	"fmt"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const Prefix = "release-tracking-"

var chainID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,200}$`)

type Record struct {
	Name string            `json:"name"`
	Data map[string]string `json:"data"`
}

type NotFound struct{ What string }

func (e NotFound) Error() string { return e.What }

type Invalid struct{ What string }

func (e Invalid) Error() string { return e.What }

type Reader struct{ CS kubernetes.Interface }

func data(d map[string]string) map[string]string {
	if d == nil {
		return map[string]string{}
	}
	return d
}

// List returns every release record of the app, all environments.
func (r Reader) List(ctx context.Context, namespace, app string) ([]Record, error) {
	list, err := r.CS.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "hangar.io/subcomponent=release-tracking,hangar.io/app=" + app,
	})
	if err != nil {
		return nil, err
	}
	out := []Record{}
	for _, cm := range list.Items {
		out = append(out, Record{Name: cm.Name, Data: data(cm.Data)})
	}
	return out, nil
}

// Get returns the record for one chain id.
func (r Reader) Get(ctx context.Context, namespace, app, chain string) (*Record, error) {
	if !chainID.MatchString(chain) {
		return nil, Invalid{fmt.Sprintf("invalid chain id '%s'", chain)}
	}
	cm, err := r.CS.CoreV1().ConfigMaps(namespace).Get(ctx, Prefix+chain, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || (err == nil && cm.Labels["hangar.io/app"] != app) {
		return nil, NotFound{fmt.Sprintf("no release record %s%s for %s", Prefix, chain, app)}
	}
	if err != nil {
		return nil, err
	}
	return &Record{Name: cm.Name, Data: data(cm.Data)}, nil
}
