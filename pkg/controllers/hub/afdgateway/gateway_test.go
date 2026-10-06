/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.goms.io/fleet-networking/pkg/common/krtutil"
	"istio.io/istio/pkg/kube/krt"
)

func newGateway(ns, name, className string, annotations map[string]string, hostnames ...string) *gatewayapiv1.Gateway {
	var listeners []gatewayapiv1.Listener
	for i, h := range hostnames {
		hn := gatewayapiv1.Hostname(h)
		listeners = append(listeners, gatewayapiv1.Listener{
			Name:     gatewayapiv1.SectionName("listener" + string(rune('0'+i))),
			Hostname: &hn,
		})
	}
	return &gatewayapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: annotations},
		Spec: gatewayapiv1.GatewaySpec{
			GatewayClassName: gatewayapiv1.ObjectName(className),
			Listeners:        listeners,
		},
	}
}

// setupEndpoints builds a NewGatewayClasses -> NewEndpoints pipeline seeded
// with the given objects and returns the synced AFDEndpoint collection.
func setupEndpoints(t *testing.T, defaultRG string, objs ...runtime.Object) (krt.Collection[AFDEndpoint], krtutil.KrtOptions) {
	t.Helper()
	client := newTestAPIClient(t, objs...)
	ob, stop := testKrtOptions(t)

	classes := NewGatewayClasses(client, ob)
	endpoints := NewEndpoints(client, ob, classes, defaultRG)
	runClient(client, stop)
	return endpoints, ob
}

func TestNewEndpoints(t *testing.T) {
	gc := newGatewayClass("afd", ControllerName)
	otherGC := newGatewayClass("nginx", "example.com/nginx")

	t.Run("gateway of owned class becomes an AFDEndpoint with defaults", func(t *testing.T) {
		gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com", "*.fabrikam.com")
		endpoints, _ := setupEndpoints(t, "default-rg", runtime.Object(gc), runtime.Object(gw))

		got := waitForKey(t, endpoints, "ns1/gw1")
		if got.ResourceGroup != "default-rg" {
			t.Errorf("ResourceGroup = %q, want default-rg", got.ResourceGroup)
		}
		if got.SKU != DefaultSKU {
			t.Errorf("SKU = %q, want %q", got.SKU, DefaultSKU)
		}
		want := []CustomDomainSpec{
			{Hostname: "www.contoso.com", SanitizedName: "www-contoso-com"},
			{Hostname: "*.fabrikam.com", SanitizedName: "wildcard-fabrikam-com"},
		}
		if diff := cmp.Diff(want, got.CustomDomains); diff != "" {
			t.Errorf("CustomDomains mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("annotations override resource group and SKU", func(t *testing.T) {
		gw := newGateway("ns1", "gw2", "afd", map[string]string{
			AnnotationResourceGroup: "custom-rg",
			AnnotationSKU:           "Premium_AzureFrontDoor",
		})
		endpoints, _ := setupEndpoints(t, "default-rg", runtime.Object(gc), runtime.Object(gw))

		got := waitForKey(t, endpoints, "ns1/gw2")
		if got.ResourceGroup != "custom-rg" {
			t.Errorf("ResourceGroup = %q, want custom-rg", got.ResourceGroup)
		}
		if got.SKU != "Premium_AzureFrontDoor" {
			t.Errorf("SKU = %q, want Premium_AzureFrontDoor", got.SKU)
		}
	})

	t.Run("gateway of a class this controller does not own is filtered out", func(t *testing.T) {
		gw := newGateway("ns1", "gw3", "nginx", nil, "www.contoso.com")
		endpoints, _ := setupEndpoints(t, "default-rg", runtime.Object(otherGC), runtime.Object(gw))

		waitForNoKey(t, endpoints, "ns1/gw3")
	})

	t.Run("duplicate listener hostnames are de-duplicated", func(t *testing.T) {
		gw := newGateway("ns1", "gw4", "afd", nil, "www.contoso.com", "www.contoso.com")
		endpoints, _ := setupEndpoints(t, "default-rg", runtime.Object(gc), runtime.Object(gw))

		got := waitForKey(t, endpoints, "ns1/gw4")
		if len(got.CustomDomains) != 1 {
			t.Errorf("CustomDomains = %+v, want exactly 1 (de-duplicated)", got.CustomDomains)
		}
	})
}
