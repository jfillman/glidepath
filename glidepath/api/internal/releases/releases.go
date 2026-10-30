// Package releases reads an app's release records: one release-tracking-<chain-id> ConfigMap per
// release to an upper environment, in the app's -cicd namespace (docs/admin/release-state.md).
// The shape, {name, data}, is the one Tower's readReleaseTracking returns, so Tower can shadow it.
// The fleet reads (ListAll, Events) are what Tower's Ops Wall and drift view read through its own
// Kubernetes proxy: every record cluster-wide with its namespace and labels, and the ReleaseStalled /
// ReleaseDrift Events Glidepath's sweeper writes about them.
package releases

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// ListEnv returns the app's records for one environment and cluster: the query Tower's glidepathRollback.ts
// readRecords makes (labels hangar.io/env and hangar.io/cluster). env and cluster must be valid label values; the
// caller checks.
func (r Reader) ListEnv(ctx context.Context, namespace, app, env, cluster string) ([]Record, error) {
	list, err := r.CS.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "hangar.io/subcomponent=release-tracking,hangar.io/app=" + app + ",hangar.io/env=" + env + ",hangar.io/cluster=" + cluster,
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

// FleetRecord is a record with where it lives: the ConfigMap fields Tower's toReleaseRecord maps (it
// falls back to the hangar.io/app, env and cluster labels when data lacks them).
type FleetRecord struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
	Data      map[string]string `json:"data"`
}

// ListAll returns every release record in namespace ("" = every namespace); with app, only that app's.
func (r Reader) ListAll(ctx context.Context, namespace, app string) ([]FleetRecord, error) {
	sel := "hangar.io/subcomponent=release-tracking"
	if app != "" {
		sel += ",hangar.io/app=" + app
	}
	list, err := r.CS.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	out := []FleetRecord{}
	for _, cm := range list.Items {
		out = append(out, FleetRecord{Name: cm.Name, Namespace: cm.Namespace, Labels: data(cm.Labels), Data: data(cm.Data)})
	}
	return out, nil
}

// EventReasons are the Events Glidepath writes about a record: the sweeper's stalled and drift checks
// (docs/admin/release-state.md).
var EventReasons = []string{"ReleaseStalled", "ReleaseDrift"}

// Event is one of them, as Tower's toReleaseEvent maps it.
type Event struct {
	Reason     string `json:"reason"`
	Namespace  string `json:"namespace"`
	RecordName string `json:"recordName"`
	Message    string `json:"message"`
	At         string `json:"at,omitempty"`
}

// Events returns the EventReasons Events about a release record in namespace ("" = every namespace):
// one list per reason (a field selector, so the API server filters), then only those whose involvedObject
// is a release-tracking- ConfigMap.
func (r Reader) Events(ctx context.Context, namespace string) ([]Event, error) {
	out := []Event{}
	for _, reason := range EventReasons {
		list, err := r.CS.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=" + reason})
		if err != nil {
			return nil, err
		}
		for _, e := range list.Items {
			// The selector already says so; checked again so the answer never depends on it.
			if e.Reason != reason || e.InvolvedObject.Kind != "ConfigMap" || !strings.HasPrefix(e.InvolvedObject.Name, Prefix) {
				continue
			}
			ns := e.InvolvedObject.Namespace
			if ns == "" {
				ns = e.Namespace
			}
			out = append(out, Event{Reason: e.Reason, Namespace: ns, RecordName: e.InvolvedObject.Name, Message: e.Message, At: eventAt(e)})
		}
	}
	return out, nil
}

// eventAt is when the event last happened: lastTimestamp, else eventTime (events.k8s.io writers), else
// firstTimestamp, else when the Event was created; "" when none is set.
func eventAt(e corev1.Event) string {
	var t time.Time
	switch {
	case !e.LastTimestamp.IsZero():
		t = e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		t = e.EventTime.Time
	case !e.FirstTimestamp.IsZero():
		t = e.FirstTimestamp.Time
	case !e.CreationTimestamp.IsZero():
		t = e.CreationTimestamp.Time
	default:
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
