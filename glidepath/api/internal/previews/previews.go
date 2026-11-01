// Package previews reads an app's preview environments (docs/admin/ephemeral-environments.md): the Argo CD
// Applications its `<app>-pr-envs` ApplicationSet generates for every PR labelled `preview`, each deploying that PR's
// own image into `app-<app>-pr-<number>`. Read only: a preview is started and ended by the label, never here.
package previews

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// GVRApplications is Argo CD's Application.
var GVRApplications = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// Preview is one preview environment as Tower and agents read it.
type Preview struct {
	PR          int    `json:"pr"`
	PRURL       string `json:"prUrl,omitempty"`
	Application string `json:"application"`
	Namespace   string `json:"namespace"`
	Image       string `json:"image,omitempty"`
	HeadSha     string `json:"headSha,omitempty"`
	// Argo CD's view: sync (Synced, OutOfSync, Unknown) and health (Healthy, Progressing, Degraded, Missing, ...).
	Sync        string `json:"sync"`
	Health      string `json:"health"`
	HealthSince string `json:"healthSince,omitempty"`
	CreatedAt   string `json:"createdAt"`
	// The app's Service in the preview namespace, when it has one, and the port-forward that reaches it.
	Service     string `json:"service,omitempty"`
	Port        int32  `json:"port,omitempty"`
	PortForward string `json:"portForward,omitempty"`
}

// Client reads previews with glidepath-api's own identity (list on applications.argoproj.io, get on Services).
type Client struct {
	Dyn   dynamic.Interface
	CS    kubernetes.Interface
	Owner string // the GitHub owner of the app repos, for the PR's URL; "" leaves it out
}

// List is every preview of app, the Applications in its -cicd namespace labelled hangar.io/ephemeral-env=true and
// hangar.io/app=<app>, newest PR first.
func (c *Client) List(ctx context.Context, namespace, app string) ([]Preview, error) {
	list, err := c.Dyn.Resource(GVRApplications).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "hangar.io/ephemeral-env=true,hangar.io/app=" + app,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Preview, 0, len(list.Items))
	for i := range list.Items {
		p := c.preview(&list.Items[i], app)
		if p.Namespace != "" {
			c.service(ctx, &p, app)
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Preview) int { return b.PR - a.PR })
	return out, nil
}

func (c *Client) preview(u *unstructured.Unstructured, app string) Preview {
	s := func(path ...string) string { v, _, _ := unstructured.NestedString(u.Object, path...); return v }
	p := Preview{
		Application: u.GetName(),
		Namespace:   s("spec", "destination", "namespace"),
		Sync:        or(s("status", "sync", "status"), "Unknown"),
		Health:      or(s("status", "health", "status"), "Unknown"),
		HealthSince: s("status", "health", "lastTransitionTime"),
		CreatedAt:   u.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(u.GetLabels()["hangar.io/env"], "pr-")); err == nil {
		p.PR = n
		if c.Owner != "" {
			p.PRURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", c.Owner, app, n)
		}
	}
	// The ApplicationSet's two sources: the chart, with the PR's image in its values, and the app repo at the PR's head.
	sources, _, _ := unstructured.NestedSlice(u.Object, "spec", "sources")
	for _, src := range sources {
		m, _ := src.(map[string]any)
		if repo, _, _ := unstructured.NestedString(m, "helm", "valuesObject", "release", "image", "repository"); repo != "" {
			tag, _, _ := unstructured.NestedString(m, "helm", "valuesObject", "release", "image", "tag")
			p.Image = repo + ":" + tag
		}
		if ref, _ := m["ref"].(string); ref == "appsrc" {
			p.HeadSha, _ = m["targetRevision"].(string)
		}
	}
	return p
}

// service fills in the app's Service in the preview namespace (the chart names it after the app). A preview whose
// Service is not there yet (still syncing) or cannot be read just has none.
func (c *Client) service(ctx context.Context, p *Preview, app string) {
	svc, err := c.CS.CoreV1().Services(p.Namespace).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		return
	}
	p.Service = svc.Name
	if len(svc.Spec.Ports) > 0 {
		p.Port = svc.Spec.Ports[0].Port
		p.PortForward = fmt.Sprintf("kubectl port-forward -n %s svc/%s %d:%d", p.Namespace, svc.Name, p.Port, p.Port)
	}
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
