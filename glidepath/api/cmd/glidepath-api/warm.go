package main

import (
	"context"
	"log"
	"regexp"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/results"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// warmWindow is the run history read for every app at start-up: Tower's widest everyday window (7 days).
const warmWindow = 7 * 24 * time.Hour

var cicdNamespace = regexp.MustCompile(`^app-([a-z0-9][a-z0-9-]*)-cicd$`)

// warm reads every app's recent runs once, in the background, so a replica's first answers after a restart are
// cached ones. Without it the first view=full call per app per replica was a cold bulk read that ran past Tower's
// 10 s timeout under load (2026-10-28, right after a deploy). One app at a time, so it adds no burst to Results;
// a failure only means that app's first call is cold, as before.
func warm(ctx context.Context, cs kubernetes.Interface, resolver *apps.Resolver, res *results.Client, now func() time.Time) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("glidepath-api: warm-up skipped: listing namespaces: %v", err)
		return
	}
	start, warmed := time.Now(), 0
	for _, ns := range list.Items {
		m := cicdNamespace.FindStringSubmatch(ns.Name)
		if m == nil {
			continue
		}
		app, err := resolver.Resolve(ctx, m[1])
		if err != nil {
			continue
		}
		t := time.Now()
		runs, err := res.Runs(ctx, app.Namespace, now().Add(-warmWindow), app.Created, 1000)
		if err != nil {
			log.Printf("glidepath-api: warm-up of %s failed after %s: %v", app.Name, time.Since(t).Round(time.Millisecond), err)
			continue
		}
		warmed++
		log.Printf("glidepath-api: warmed %s: %d runs in %s", app.Name, len(runs), time.Since(t).Round(time.Millisecond))
	}
	log.Printf("glidepath-api: warm-up done: %d apps in %s", warmed, time.Since(start).Round(time.Second))
}
