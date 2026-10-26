// Package apps maps an app name to the namespace its pipelines run in: <type>-<app>-cicd
// (docs/admin/concepts.md), app- or infra-. The namespace's creation time is when the app as it
// exists now began: Tekton Results keeps 30 days of records keyed by namespace, so an app deleted and
// created again under the same name would otherwise inherit the old one's runs.
package apps

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Types, in the order they're tried.
var Types = []string{"app", "infra"}

type App struct {
	Name      string
	Namespace string
	Created   time.Time
}

// NotFound is returned for a valid name with no -cicd namespace.
type NotFound struct{ Name string }

func (e NotFound) Error() string { return fmt.Sprintf("no app '%s': neither app-%[1]s-cicd nor infra-%[1]s-cicd exists", e.Name) }

// Invalid is returned for a name that can't be an app.
type Invalid struct{ Name string }

func (e Invalid) Error() string { return fmt.Sprintf("invalid app name '%s'", e.Name) }

type Resolver struct {
	CS  kubernetes.Interface
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	app *App
	at  time.Time
}

func (r *Resolver) Resolve(ctx context.Context, app string) (*App, error) {
	if !name.MatchString(app) {
		return nil, Invalid{app}
	}
	r.mu.Lock()
	if c, ok := r.cache[app]; ok && time.Since(c.at) < r.TTL {
		r.mu.Unlock()
		return c.app, nil
	}
	r.mu.Unlock()
	for _, t := range Types {
		ns := fmt.Sprintf("%s-%s-cicd", t, app)
		obj, err := r.CS.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		a := &App{Name: app, Namespace: ns, Created: obj.CreationTimestamp.Time}
		r.mu.Lock()
		if r.cache == nil {
			r.cache = map[string]cached{}
		}
		r.cache[app] = cached{a, time.Now()}
		r.mu.Unlock()
		return a, nil
	}
	return nil, NotFound{app}
}
