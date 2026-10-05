/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"strings"

	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"k8s.io/apimachinery/pkg/types"

	"go.goms.io/fleet-networking/pkg/apiclient"
	"go.goms.io/fleet-networking/pkg/common/krtutil"
)

// NewEndpoints wraps the cluster's Gateway resources into a collection of
// AFDEndpoint: the Profile/Endpoint/CustomDomain-level AFD configuration
// each Gateway needs. Gateways whose GatewayClass is not owned by this
// controller (see NewGatewayClasses) are filtered out.
func NewEndpoints(c apiclient.Client, ob krtutil.KrtOptions, classes krt.Collection[AFDGatewayClass], defaultResourceGroup string) krt.Collection[AFDEndpoint] {
	gateways := krt.WrapClient(kclient.New[*gatewayapiv1.Gateway](c), ob.ToOptions("gateways")...)

	return krt.NewCollection(gateways, func(kctx krt.HandlerContext, gw *gatewayapiv1.Gateway) *AFDEndpoint {
		if krt.FetchOne(kctx, classes, krt.FilterKey(string(gw.Spec.GatewayClassName))) == nil {
			return nil
		}

		rg := strings.TrimSpace(gw.Annotations[AnnotationResourceGroup])
		if rg == "" {
			rg = defaultResourceGroup
		}
		sku := strings.TrimSpace(gw.Annotations[AnnotationSKU])
		if sku == "" {
			sku = DefaultSKU
		}

		seen := map[string]bool{}
		var domains []CustomDomainSpec
		for _, l := range gw.Spec.Listeners {
			if l.Hostname == nil || *l.Hostname == "" {
				continue
			}
			host := string(*l.Hostname)
			if seen[host] {
				continue
			}
			seen[host] = true
			domains = append(domains, CustomDomainSpec{
				Hostname:      host,
				SanitizedName: customDomainResourceName(host),
			})
		}

		return &AFDEndpoint{
			NamespacedName: types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name},
			ResourceGroup:  rg,
			SKU:            sku,
			ProfileName:    sanitizeAFDName(gw.Namespace + "-" + gw.Name),
			EndpointName:   sanitizeAFDName(gw.Namespace + "-" + gw.Name),
			CustomDomains:  domains,
		}
	}, ob.ToOptions("afd-endpoints")...)
}
