package releases

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.FixedZone("PDT", -7*3600))

func ev(ns, name, reason, kind, object string) *corev1.Event {
	return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Reason: reason, Message: reason + " " + object, InvolvedObject: corev1.ObjectReference{Kind: kind, Name: object, Namespace: ns}}
}

func TestEventAt(t *testing.T) {
	ts := func(h int) metav1.Time { return metav1.NewTime(t0.Add(time.Duration(h) * time.Hour)) }
	cases := []struct {
		name string
		e    corev1.Event
		want string
	}{
		{"lastTimestamp first", corev1.Event{LastTimestamp: ts(4), EventTime: metav1.NewMicroTime(t0.Add(3 * time.Hour)), FirstTimestamp: ts(2),
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ts(1)}}, "2026-10-30T20:00:00Z"},
		{"then eventTime", corev1.Event{EventTime: metav1.NewMicroTime(t0.Add(3 * time.Hour)), FirstTimestamp: ts(2),
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ts(1)}}, "2026-10-30T19:00:00Z"},
		{"then firstTimestamp", corev1.Event{FirstTimestamp: ts(2), ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ts(1)}}, "2026-10-30T18:00:00Z"},
		{"then creationTimestamp", corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ts(1)}}, "2026-10-30T17:00:00Z"},
		{"none", corev1.Event{}, ""},
	}
	for _, c := range cases {
		if got := eventAt(c.e); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEvents(t *testing.T) {
	noNS := ev("app-cart-cicd", "e5", "ReleaseDrift", "ConfigMap", "release-tracking-cart-1")
	noNS.InvolvedObject.Namespace = ""
	cs := fake.NewSimpleClientset(
		ev("app-shop-cicd", "e1", "ReleaseStalled", "ConfigMap", "release-tracking-shop-1"),
		ev("app-shop-cicd", "e2", "ReleaseStalled", "ConfigMap", "some-other-configmap"),
		ev("app-shop-cicd", "e3", "ReleaseStalled", "Pod", "release-tracking-shop-1"),
		ev("app-shop-cicd", "e4", "BackOff", "ConfigMap", "release-tracking-shop-1"),
		noNS,
	)
	evs, err := Reader{CS: cs}.Events(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("a release reason about a release-tracking- ConfigMap only: %+v", evs)
	}
	if e := evs[0]; e.Reason != "ReleaseStalled" || e.Namespace != "app-shop-cicd" || e.RecordName != "release-tracking-shop-1" || e.Message != "ReleaseStalled release-tracking-shop-1" {
		t.Errorf("stalled: %+v", e)
	}
	if e := evs[1]; e.Reason != "ReleaseDrift" || e.Namespace != "app-cart-cicd" {
		t.Errorf("no involvedObject namespace falls back to the Event's: %+v", e)
	}
	var sels []string
	for _, a := range cs.Actions() {
		if l, ok := a.(k8stesting.ListAction); ok && a.GetResource().Resource == "events" {
			sels = append(sels, l.GetListRestrictions().Fields.String())
		}
	}
	if len(sels) != 2 || sels[0] != "reason=ReleaseStalled" || sels[1] != "reason=ReleaseDrift" {
		t.Fatalf("one list per reason, by field selector: %v", sels)
	}
	if evs, _ := (Reader{CS: cs}).Events(context.Background(), "app-cart-cicd"); len(evs) != 1 || evs[0].Namespace != "app-cart-cicd" {
		t.Fatalf("one namespace: %+v", evs)
	}
}

func TestGetClearanceJoin(t *testing.T) {
	cm := func(name string, d map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "app-shop-cicd", Name: name,
			Labels: map[string]string{"hangar.io/app": "shop", "hangar.io/subcomponent": "release-tracking"}}, Data: d}
	}
	cs := fake.NewSimpleClientset(
		cm("release-tracking-c1", map[string]string{"releaseId": "c1:hangar-prod/prod", "clearanceId": "chg-1", "clearanceTree": "sha256:abc"}),
		cm("release-tracking-c2", map[string]string{"releaseId": "c2:hangar-prod/prod"}),
	)
	r := Reader{CS: cs}
	got, err := r.Get(context.Background(), "app-shop-cicd", "shop", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Clearance == nil || got.Clearance.ID != "chg-1" || got.Clearance.Tree != "sha256:abc" {
		t.Fatalf("the clearance join: %+v", got.Clearance)
	}
	if got.Data["clearanceId"] != "chg-1" {
		t.Errorf("the raw data keeps the keys: %+v", got.Data)
	}
	none, err := r.Get(context.Background(), "app-shop-cicd", "shop", "c2")
	if err != nil {
		t.Fatal(err)
	}
	if none.Clearance != nil {
		t.Errorf("a release the gate never registered has no clearance: %+v", none.Clearance)
	}
	list, err := r.List(context.Background(), "app-shop-cicd", "shop")
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %+v", err, list)
	}
	for _, rec := range list {
		if (rec.Name == "release-tracking-c1") != (rec.Clearance != nil) {
			t.Errorf("list carries the join on c1 only: %s %+v", rec.Name, rec.Clearance)
		}
	}
}
