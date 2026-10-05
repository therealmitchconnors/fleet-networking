/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"istio.io/istio/pkg/kube/krt"

	"go.goms.io/fleet-networking/pkg/common/krtutil"
)

// EndpointDeployment is the desired state of one Gateway's "endpoint
// stack": the slow-changing Profile/AFDEndpoint/CustomDomain resources
// (see endpoint_template.go). It changes only when the Gateway's listener
// hostnames, resource group, or SKU change.
type EndpointDeployment struct {
	GatewayKey string
	Endpoint   AFDEndpoint
}

// ResourceName implements krt.ResourceNamer.
func (d EndpointDeployment) ResourceName() string { return d.GatewayKey }

var _ krt.ResourceNamer = EndpointDeployment{}

// RoutingDeployment is the desired state of one Gateway's "routing stack":
// the faster-changing OriginGroups/Origins/RuleSets/Rules/Routes resources
// (see routing_template.go), derived from the Gateway's matched HTTPRoutes.
// It is deployed as a separate ARM Deployment Stack from EndpointDeployment
// so HTTPRoute churn never forces redeploying Profile/Endpoint/CustomDomain
// state (and, conversely, so cert/domain changes don't touch routing).
type RoutingDeployment struct {
	GatewayKey string
	Endpoint   AFDEndpoint
	Routes     []AFDRoute
}

// ResourceName implements krt.ResourceNamer.
func (d RoutingDeployment) ResourceName() string { return d.GatewayKey }

var _ krt.ResourceNamer = RoutingDeployment{}

// NewDeployments joins endpoints with their matched routes (via routeIdx,
// keyed by AFDRoute.GatewayKey) into the two deployment-unit collections
// that are actually applied to Azure.
func NewDeployments(ob krtutil.KrtOptions, endpoints krt.Collection[AFDEndpoint], routes krt.Collection[AFDRoute]) (krt.Collection[EndpointDeployment], krt.Collection[RoutingDeployment]) {
	routeIdx := krt.NewIndex(routes, "by gateway", func(r AFDRoute) []string {
		return []string{r.GatewayKey}
	})

	endpointDeployments := krt.NewCollection(endpoints, func(_ krt.HandlerContext, e AFDEndpoint) *EndpointDeployment {
		return &EndpointDeployment{GatewayKey: e.NamespacedName.String(), Endpoint: e}
	}, ob.ToOptions("afd-endpoint-deployments")...)

	routingDeployments := krt.NewCollection(endpoints, func(kctx krt.HandlerContext, e AFDEndpoint) *RoutingDeployment {
		key := e.NamespacedName.String()
		matched := krt.Fetch(kctx, routes, krt.FilterIndex(routeIdx, key))
		return &RoutingDeployment{GatewayKey: key, Endpoint: e, Routes: matched}
	}, ob.ToOptions("afd-routing-deployments")...)

	return endpointDeployments, routingDeployments
}
