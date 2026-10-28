package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glidepath/api/internal/apps"
	"github.com/glidepath/api/internal/results"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWarmReadsEveryAppOnce(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/apis/results.tekton.dev/v1alpha2/parents/")
		mu.Lock()
		asked[strings.SplitN(p, "/", 2)[0]]++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer stub.Close()
	ns := func(n string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n, CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))}}
	}
	cs := fake.NewSimpleClientset(ns("app-shop-cicd"), ns("app-cart-cicd"), ns("infra-bus-cicd"), ns("app-shop-dev"), ns("kube-system"))
	warm(context.Background(), cs, &apps.Resolver{CS: cs, TTL: time.Minute}, &results.Client{Base: stub.URL, HTTP: stub.Client()}, time.Now)
	if len(asked) != 2 || asked["app-shop-cicd"] != 1 || asked["app-cart-cicd"] != 1 {
		t.Fatalf("warm-up should read each app -cicd namespace once, read %v", asked)
	}
}
