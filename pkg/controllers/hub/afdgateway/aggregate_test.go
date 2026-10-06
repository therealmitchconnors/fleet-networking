/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
)

// setupDeployments builds the full pipeline (GatewayClasses -> Endpoints ->
// ReferenceGrantEdges -> Routes -> Deployments) seeded with objs, and
// returns the synced EndpointDeployment/RoutingDeployment collections.
func setupDeployments(t *testing.T, objs ...runtime.Object) (krt.Collection[EndpointDeployment], krt.Collection[RoutingDeployment]) {
	t.Helper()
	client := newTestAPIClient(t, objs...)
	ob, stop := testKrtOptions(t)

	services := krt.WrapClient(kclient.New[*corev1.Service](client), ob.ToOptions("test-services")...)
	classes := NewGatewayClasses(client, ob)
	endpoints := NewEndpoints(client, ob, classes, "default-rg")
	refGrants, refGrantIdx := NewReferenceGrantEdges(client, ob)
	routes := NewRoutes(client, ob, endpoints, services, refGrants, refGrantIdx)
	endpointDeployments, routingDeployments := NewDeployments(ob, endpoints, routes)
	runClient(client, stop)
	return endpointDeployments, routingDeployments
}

func TestNewDeployments(t *testing.T) {
	t.Run("endpoint deployment is produced for every Gateway, even with no routes", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")

		endpointDeployments, routingDeployments := setupDeployments(t, runtime.Object(gc), runtime.Object(gw))

		ed := waitForKey(t, endpointDeployments, "ns1/gw1")
		if ed.GatewayKey != "ns1/gw1" {
			t.Errorf("GatewayKey = %q, want ns1/gw1", ed.GatewayKey)
		}

		rd := waitForKey(t, routingDeployments, "ns1/gw1")
		if len(rd.Routes) != 0 {
			t.Errorf("Routes = %+v, want none", rd.Routes)
		}
	})

	t.Run("routing deployment aggregates all routes matched to the Gateway", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com", "api.contoso.com")
		svc := newLBService("ns1", "svc1", "lb.example.com")
		route1 := newHTTPRoute("ns1", "route1", "ns1", "gw1", []string{"www.contoso.com"}, newBackendRef("", "svc1"))
		route2 := newHTTPRoute("ns1", "route2", "ns1", "gw1", []string{"api.contoso.com"}, newBackendRef("", "svc1"))

		_, routingDeployments := setupDeployments(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(svc), runtime.Object(route1), runtime.Object(route2))

		rd := waitForKey(t, routingDeployments, "ns1/gw1")
		if len(rd.Routes) != 2 {
			t.Fatalf("Routes = %+v, want exactly 2", rd.Routes)
		}
	})

	t.Run("routes bound to other Gateways are not aggregated", func(t *testing.T) {
		gc := newGatewayClass("afd", ControllerName)
		gw1 := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")
		gw2 := newGateway("ns1", "gw2", "afd", nil, "other.contoso.com")
		svc := newLBService("ns1", "svc1", "lb.example.com")
		route := newHTTPRoute("ns1", "route1", "ns1", "gw2", nil, newBackendRef("", "svc1"))

		_, routingDeployments := setupDeployments(t, runtime.Object(gc), runtime.Object(gw1), runtime.Object(gw2), runtime.Object(svc), runtime.Object(route))

		rd1 := waitForKey(t, routingDeployments, "ns1/gw1")
		if len(rd1.Routes) != 0 {
			t.Errorf("gw1 Routes = %+v, want none", rd1.Routes)
		}
		rd2 := waitForKey(t, routingDeployments, "ns1/gw2")
		if len(rd2.Routes) != 1 {
			t.Errorf("gw2 Routes = %+v, want exactly 1", rd2.Routes)
		}
	})
}
