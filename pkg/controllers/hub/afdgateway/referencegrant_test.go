/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayapiv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/istio/pkg/kube/krt"
)

func newReferenceGrant(ns, name string, fromNS, fromKind string, toKind, toName string) *gatewayapiv1beta1.ReferenceGrant {
	grant := &gatewayapiv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: gatewayapiv1beta1.ReferenceGrantSpec{
			From: []gatewayapiv1beta1.ReferenceGrantFrom{
				{Group: gatewayapiv1beta1.Group("gateway.networking.k8s.io"), Kind: gatewayapiv1beta1.Kind(fromKind), Namespace: gatewayapiv1beta1.Namespace(fromNS)},
			},
			To: []gatewayapiv1beta1.ReferenceGrantTo{
				{Kind: gatewayapiv1beta1.Kind(toKind)},
			},
		},
	}
	if toName != "" {
		n := gatewayapiv1beta1.ObjectName(toName)
		grant.Spec.To[0].Name = &n
	}
	return grant
}

// setupReferenceGrantEdges builds a synced NewReferenceGrantEdges
// collection + index seeded with the given objects.
func setupReferenceGrantEdges(t *testing.T, objs ...runtime.Object) (krt.Collection[referenceGrantEdge], krt.Index[string, referenceGrantEdge]) {
	t.Helper()
	client := newTestAPIClient(t, objs...)
	ob, stop := testKrtOptions(t)

	edges, idx := NewReferenceGrantEdges(client, ob)
	runClient(client, stop)
	return edges, idx
}

func TestNewReferenceGrantEdges(t *testing.T) {
	grant := newReferenceGrant("backend-ns", "allow-httproute", "frontend-ns", "HTTPRoute", "Service", "")
	edges, _ := setupReferenceGrantEdges(t, runtime.Object(grant))

	got := waitForListLen(t, edges, 1)
	want := referenceGrantEdge{
		fromNamespace: "frontend-ns",
		fromKind:      "HTTPRoute",
		toNamespace:   "backend-ns",
		toKind:        "Service",
		toName:        "",
	}
	if got[0] != want {
		t.Errorf("edge = %+v, want %+v", got[0], want)
	}
}

func TestIsReferenceAllowed(t *testing.T) {
	t.Run("same namespace is always allowed, even with no grants", func(t *testing.T) {
		edges, idx := setupReferenceGrantEdges(t)
		waitForListLen(t, edges, 0)
		if !isReferenceAllowed(krt.TestingDummyContext{}, edges, idx, "ns1", "HTTPRoute", "ns1", "Service", "svc") {
			t.Error("same-namespace reference should be allowed without any ReferenceGrant")
		}
	})

	t.Run("cross namespace denied without a matching grant", func(t *testing.T) {
		edges, idx := setupReferenceGrantEdges(t)
		waitForListLen(t, edges, 0)
		if isReferenceAllowed(krt.TestingDummyContext{}, edges, idx, "frontend-ns", "HTTPRoute", "backend-ns", "Service", "svc") {
			t.Error("cross-namespace reference should be denied with no ReferenceGrant")
		}
	})

	t.Run("cross namespace allowed by a wildcard-name grant", func(t *testing.T) {
		grant := newReferenceGrant("backend-ns", "allow-httproute", "frontend-ns", "HTTPRoute", "Service", "")
		edges, idx := setupReferenceGrantEdges(t, runtime.Object(grant))
		waitForListLen(t, edges, 1)
		if !isReferenceAllowed(krt.TestingDummyContext{}, edges, idx, "frontend-ns", "HTTPRoute", "backend-ns", "Service", "any-svc-name") {
			t.Error("expected wildcard-name ReferenceGrant to allow any Service name")
		}
	})

	t.Run("cross namespace allowed only for the named resource", func(t *testing.T) {
		grant := newReferenceGrant("backend-ns", "allow-httproute", "frontend-ns", "HTTPRoute", "Service", "svc-a")
		edges, idx := setupReferenceGrantEdges(t, runtime.Object(grant))
		waitForListLen(t, edges, 1)
		if !isReferenceAllowed(krt.TestingDummyContext{}, edges, idx, "frontend-ns", "HTTPRoute", "backend-ns", "Service", "svc-a") {
			t.Error("expected named ReferenceGrant to allow the named Service")
		}
		if isReferenceAllowed(krt.TestingDummyContext{}, edges, idx, "frontend-ns", "HTTPRoute", "backend-ns", "Service", "svc-b") {
			t.Error("expected named ReferenceGrant to deny a differently-named Service")
		}
	})

	t.Run("cross namespace denied when from-kind does not match", func(t *testing.T) {
		grant := newReferenceGrant("backend-ns", "allow-httproute", "frontend-ns", "HTTPRoute", "Service", "")
		edges, idx := setupReferenceGrantEdges(t, runtime.Object(grant))
		waitForListLen(t, edges, 1)
		if isReferenceAllowed(krt.TestingDummyContext{}, edges, idx, "frontend-ns", "GRPCRoute", "backend-ns", "Service", "svc") {
			t.Error("expected ReferenceGrant scoped to HTTPRoute to deny a GRPCRoute reference")
		}
	})
}
