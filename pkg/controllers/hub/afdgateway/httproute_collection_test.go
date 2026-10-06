/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
)

func newLBService(ns, name, host string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{Hostname: host}},
			},
		},
	}
}

func namePtr(n string) *gatewayapiv1.ObjectName { v := gatewayapiv1.ObjectName(n); return &v }
func nsPtr(n string) *gatewayapiv1.Namespace    { v := gatewayapiv1.Namespace(n); return &v }

func newHTTPRoute(ns, name, gwNS, gwName string, hostnames []string, backendRefs ...gatewayapiv1.HTTPBackendRef) *gatewayapiv1.HTTPRoute {
	parent := gatewayapiv1.ParentReference{Name: gatewayapiv1.ObjectName(gwName)}
	if gwNS != ns {
		parent.Namespace = nsPtr(gwNS)
	}
	var hs []gatewayapiv1.Hostname
	for _, h := range hostnames {
		hs = append(hs, gatewayapiv1.Hostname(h))
	}
	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{ParentRefs: []gatewayapiv1.ParentReference{parent}},
			Hostnames:       hs,
			Rules: []gatewayapiv1.HTTPRouteRule{
				{BackendRefs: backendRefs},
			},
		},
	}
}

func newBackendRef(ns, name string) gatewayapiv1.HTTPBackendRef {
	br := gatewayapiv1.HTTPBackendRef{
		BackendRef: gatewayapiv1.BackendRef{
			BackendObjectReference: gatewayapiv1.BackendObjectReference{Name: gatewayapiv1.ObjectName(name)},
		},
	}
	if ns != "" {
		br.Namespace = nsPtr(ns)
	}
	return br
}

// setupRoutes builds the full NewGatewayClasses -> NewEndpoints ->
// NewReferenceGrantEdges -> NewRoutes pipeline seeded with objs, and
// returns the synced AFDRoute collection.
func setupRoutes(t *testing.T, objs ...runtime.Object) krt.Collection[AFDRoute] {
	t.Helper()
	client := newTestAPIClient(t, objs...)
	ob, stop := testKrtOptions(t)

	services := krt.WrapClient(kclient.New[*corev1.Service](client), ob.ToOptions("test-services")...)
	classes := NewGatewayClasses(client, ob)
	endpoints := NewEndpoints(client, ob, classes, "default-rg")
	refGrants, refGrantIdx := NewReferenceGrantEdges(client, ob)
	routes := NewRoutes(client, ob, endpoints, services, refGrants, refGrantIdx)
	runClient(client, stop)
	return routes
}

func TestNewRoutes(t *testing.T) {
	t.Run("resolves a same-namespace LoadBalancer backendRef", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")
		svc := newLBService("ns1", "svc1", "lb.example.com")
		route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("", "svc1"))

		routes := setupRoutes(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(svc), runtime.Object(route))

		got := waitForKey(t, routes, "ns1/route1/www.contoso.com")
		if len(got.Origins) != 1 || got.Origins[0].HostName != "lb.example.com" {
			t.Errorf("Origins = %+v, want one origin with hostname lb.example.com", got.Origins)
		}
		if len(got.MissingBackendRefs) != 0 {
			t.Errorf("MissingBackendRefs = %v, want none", got.MissingBackendRefs)
		}
	})

	t.Run("missing service produces a MissingBackendRefs entry", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")
		route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("", "does-not-exist"))

		routes := setupRoutes(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(route))

		got := waitForKey(t, routes, "ns1/route1/www.contoso.com")
		if len(got.Origins) != 0 {
			t.Errorf("Origins = %+v, want none", got.Origins)
		}
		if len(got.MissingBackendRefs) != 1 {
			t.Errorf("MissingBackendRefs = %v, want exactly one entry", got.MissingBackendRefs)
		}
	})

	t.Run("ClusterIP service produces a MissingBackendRefs entry", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "svc1"},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
		}
		route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("", "svc1"))

		routes := setupRoutes(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(svc), runtime.Object(route))

		got := waitForKey(t, routes, "ns1/route1/www.contoso.com")
		if len(got.MissingBackendRefs) != 1 {
			t.Errorf("MissingBackendRefs = %v, want exactly one entry for a ClusterIP service", got.MissingBackendRefs)
		}
	})

	t.Run("cross-namespace backendRef denied without a ReferenceGrant", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")
		svc := newLBService("ns2", "svc1", "lb.example.com")
		route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("ns2", "svc1"))

		routes := setupRoutes(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(svc), runtime.Object(route))

		got := waitForKey(t, routes, "ns1/route1/www.contoso.com")
		if len(got.Origins) != 0 {
			t.Errorf("Origins = %+v, want none (cross-namespace ref should be denied)", got.Origins)
		}
		if len(got.DeniedCrossNSRefs) != 1 {
			t.Errorf("DeniedCrossNSRefs = %v, want exactly one entry", got.DeniedCrossNSRefs)
		}
	})

	t.Run("cross-namespace backendRef allowed by a ReferenceGrant", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")
		svc := newLBService("ns2", "svc1", "lb.example.com")
		route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("ns2", "svc1"))
		grant := newReferenceGrant("ns2", "allow-ns1", "ns1", "HTTPRoute", "Service", "")

		routes := setupRoutes(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(svc), runtime.Object(route), runtime.Object(grant))

		got := waitForKey(t, routes, "ns1/route1/www.contoso.com")
		if len(got.Origins) != 1 || got.Origins[0].HostName != "lb.example.com" {
			t.Errorf("Origins = %+v, want one origin with hostname lb.example.com", got.Origins)
		}
		if len(got.DeniedCrossNSRefs) != 0 {
			t.Errorf("DeniedCrossNSRefs = %v, want none", got.DeniedCrossNSRefs)
		}
	})

	t.Run("one AFDRoute per matched Gateway listener hostname", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com", "api.contoso.com")
		svc := newLBService("ns1", "svc1", "lb.example.com")
		route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("", "svc1"))

		routes := setupRoutes(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(svc), runtime.Object(route))

		waitForKey(t, routes, "ns1/route1/www.contoso.com")
		waitForKey(t, routes, "ns1/route1/api.contoso.com")
		waitForListLen(t, routes, 2)
	})

	t.Run("route.Spec.Hostnames restricts which listener hostnames are matched", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com", "api.contoso.com")
		svc := newLBService("ns1", "svc1", "lb.example.com")
		route := newHTTPRoute("ns1", "route1", "ns1", "gw1", []string{"www.contoso.com"}, newBackendRef("", "svc1"))

		routes := setupRoutes(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(svc), runtime.Object(route))

		waitForKey(t, routes, "ns1/route1/www.contoso.com")
		waitForListLen(t, routes, 1)
	})

	t.Run("route with a parentRef to a Gateway of an unowned class is ignored", func(t *testing.T) {
		otherGC := newGatewayClass("nginx", "example.com/nginx")
		gw := newGateway("ns1", "gw1", "nginx", nil, "www.contoso.com")
		svc := newLBService("ns1", "svc1", "lb.example.com")
		route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("", "svc1"))

		routes := setupRoutes(t, runtime.Object(otherGC), runtime.Object(gw), runtime.Object(svc), runtime.Object(route))

		time.Sleep(testPollInterval * 5)
		if got := routes.List(); len(got) != 0 {
			t.Errorf("routes.List() = %+v, want none", got)
		}
	})
}
