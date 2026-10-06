/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func newGatewayClass(name, controllerName string) *gatewayapiv1.GatewayClass {
	return &gatewayapiv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gatewayapiv1.GatewayClassSpec{ControllerName: gatewayapiv1.GatewayController(controllerName)},
	}
}

func TestNewGatewayClasses(t *testing.T) {
	owned := newGatewayClass("afd", ControllerName)
	other := newGatewayClass("nginx", "example.com/nginx-controller")

	client := newTestAPIClient(t, runtime.Object(owned), runtime.Object(other))
	ob, stop := testKrtOptions(t)

	classes := NewGatewayClasses(client, ob)
	runClient(client, stop)

	got := waitForKey(t, classes, "afd")
	if got.Name != "afd" {
		t.Errorf("NewGatewayClasses()[afd].Name = %q, want afd", got.Name)
	}

	waitForNoKey(t, classes, "nginx")
}
