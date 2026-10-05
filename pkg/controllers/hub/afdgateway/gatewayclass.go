/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"

	"go.goms.io/fleet-networking/pkg/apiclient"
	"go.goms.io/fleet-networking/pkg/common/krtutil"
)

// NewGatewayClasses wraps the cluster's GatewayClass resources into a
// collection of just the classes this controller owns (spec.ControllerName
// == ControllerName), keyed by class name so NewEndpoints can cheaply check
// whether a given Gateway belongs to this controller via krt.FetchOne.
func NewGatewayClasses(c apiclient.Client, ob krtutil.KrtOptions) krt.Collection[AFDGatewayClass] {
	classes := krt.WrapClient(kclient.New[*gatewayapiv1.GatewayClass](c), ob.ToOptions("gatewayclasses")...)
	return krt.NewCollection(classes, func(_ krt.HandlerContext, gc *gatewayapiv1.GatewayClass) *AFDGatewayClass {
		if string(gc.Spec.ControllerName) != ControllerName {
			return nil
		}
		return &AFDGatewayClass{Name: gc.Name}
	}, ob.ToOptions("afd-gatewayclasses")...)
}
