/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"

	"go.goms.io/fleet-networking/pkg/apiclient"
	"go.goms.io/fleet-networking/pkg/common/krtutil"
)

// toMatchConditions converts one HTTPRouteMatch into the AFD Rule Set
// conditions that must all be true (ANDed) for the match to apply. It is a
// pure function so it can be unit tested without a krt/Kubernetes fixture.
func toMatchConditions(m gatewayapiv1.HTTPRouteMatch) []MatchCondition {
	var out []MatchCondition

	if m.Path != nil && m.Path.Value != nil {
		op := "BeginsWith"
		switch derefPathType(m.Path.Type) {
		case gatewayapiv1.PathMatchExact:
			op = "Equal"
		case gatewayapiv1.PathMatchRegularExpression:
			op = "RegEx"
		}
		out = append(out, MatchCondition{Kind: "UrlPath", Operator: op, Values: []string{*m.Path.Value}})
	}

	for _, h := range m.Headers {
		op := "Equal"
		if derefHeaderType(h.Type) == gatewayapiv1.HeaderMatchRegularExpression {
			op = "RegEx"
		}
		out = append(out, MatchCondition{Kind: "RequestHeader", Operator: op, Selector: string(h.Name), Values: []string{h.Value}})
	}

	for _, q := range m.QueryParams {
		op := "Equal"
		if q.Type != nil && *q.Type == gatewayapiv1.QueryParamMatchRegularExpression {
			op = "RegEx"
		}
		out = append(out, MatchCondition{Kind: "QueryString", Operator: op, Selector: string(q.Name), Values: []string{q.Value}})
	}

	if m.Method != nil {
		out = append(out, MatchCondition{Kind: "RequestMethod", Operator: "Equal", Values: []string{string(*m.Method)}})
	}

	return out
}

func derefPathType(t *gatewayapiv1.PathMatchType) gatewayapiv1.PathMatchType {
	if t == nil {
		return gatewayapiv1.PathMatchPathPrefix
	}
	return *t
}

func derefHeaderType(t *gatewayapiv1.HeaderMatchType) gatewayapiv1.HeaderMatchType {
	if t == nil {
		return gatewayapiv1.HeaderMatchExact
	}
	return *t
}

// toRuleActions converts an HTTPRouteRule's filters into AFD Rule actions.
// It returns the actions it could translate plus a list of human-readable
// descriptions of any filters it could not (e.g. RequestMirror, which AFD
// has no equivalent for); callers surface the latter via HTTPRoute status.
func toRuleActions(filters []gatewayapiv1.HTTPRouteFilter) ([]RuleAction, []string) {
	var actions []RuleAction
	var unsupported []string

	for _, f := range filters {
		switch f.Type {
		case gatewayapiv1.HTTPRouteFilterRequestHeaderModifier:
			actions = append(actions, headerModifierActions("ModifyRequestHeader", f.RequestHeaderModifier)...)
		case gatewayapiv1.HTTPRouteFilterResponseHeaderModifier:
			actions = append(actions, headerModifierActions("ModifyResponseHeader", f.ResponseHeaderModifier)...)
		case gatewayapiv1.HTTPRouteFilterRequestRedirect:
			if rr := f.RequestRedirect; rr != nil {
				a := RuleAction{Kind: "UrlRedirect"}
				a.RedirectScheme = rr.Scheme
				if rr.Hostname != nil {
					s := string(*rr.Hostname)
					a.RedirectHostname = &s
				}
				if rr.Path != nil && rr.Path.ReplaceFullPath != nil {
					a.RedirectPath = rr.Path.ReplaceFullPath
				}
				if rr.StatusCode != nil {
					code := int32(*rr.StatusCode)
					a.RedirectStatus = &code
				}
				actions = append(actions, a)
			}
		case gatewayapiv1.HTTPRouteFilterURLRewrite:
			if rw := f.URLRewrite; rw != nil {
				a := RuleAction{Kind: "UrlRewrite"}
				if rw.Hostname != nil {
					s := string(*rw.Hostname)
					a.RewriteHostname = &s
				}
				if rw.Path != nil && rw.Path.Type == gatewayapiv1.PrefixMatchHTTPPathModifier && rw.Path.ReplacePrefixMatch != nil {
					a.RewritePrefix = rw.Path.ReplacePrefixMatch
				}
				actions = append(actions, a)
			}
		default:
			unsupported = append(unsupported, fmt.Sprintf("filter type %s is not supported by the AFD gateway controller", f.Type))
		}
	}
	return actions, unsupported
}

func headerModifierActions(kind string, hf *gatewayapiv1.HTTPHeaderFilter) []RuleAction {
	if hf == nil {
		return nil
	}
	var out []RuleAction
	for _, h := range hf.Set {
		out = append(out, RuleAction{Kind: kind, HeaderName: string(h.Name), HeaderValue: h.Value, HeaderOp: "Overwrite"})
	}
	for _, h := range hf.Add {
		out = append(out, RuleAction{Kind: kind, HeaderName: string(h.Name), HeaderValue: h.Value, HeaderOp: "Append"})
	}
	for _, name := range hf.Remove {
		out = append(out, RuleAction{Kind: kind, HeaderName: name, HeaderOp: "Delete"})
	}
	return out
}

// hostnameMatches reports whether listener hostname pattern (possibly
// "*.example.com") matches candidate.
func hostnameMatches(pattern, candidate string) bool {
	if pattern == "" || pattern == candidate {
		return true
	}
	if len(pattern) > 1 && pattern[0] == '*' {
		suffix := pattern[1:] // ".example.com"
		return len(candidate) > len(suffix) && candidate[len(candidate)-len(suffix):] == suffix
	}
	return false
}

// matchingHostnames returns the listener hostnames (from endpoint's
// CustomDomains) that this HTTPRoute applies to: the intersection with
// route.Spec.Hostnames when the route restricts hostnames, otherwise every
// listener hostname on the Gateway.
func matchingHostnames(endpoint AFDEndpoint, route *gatewayapiv1.HTTPRoute) []CustomDomainSpec {
	if len(route.Spec.Hostnames) == 0 {
		return endpoint.CustomDomains
	}
	var out []CustomDomainSpec
	for _, d := range endpoint.CustomDomains {
		for _, h := range route.Spec.Hostnames {
			if hostnameMatches(string(h), d.Hostname) || hostnameMatches(d.Hostname, string(h)) {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// NewRoutes wraps the cluster's HTTPRoute resources into a collection of
// AFDRoute, one per (HTTPRoute, matched Gateway listener hostname) pair.
//
// Known v1 simplifications (documented rather than solved, to keep this
// controller's first version bounded in scope):
//   - All HTTPRouteRule.BackendRefs across all rules of a route are merged
//     into a single AFD Origin Group per hostname; AFD does not support
//     selecting a different origin per Rule Set rule without the more
//     advanced RouteConfigurationOverride action, which this controller
//     does not yet generate.
//   - BackendRefs must point to a Service of type LoadBalancer; the
//     Service's external IP/hostname is used as the AFD origin. ClusterIP
//     Services are not reachable from AFD without Azure Private Link
//     (Premium SKU only) and are rejected with a MissingBackendRefs status.
func NewRoutes(c apiclient.Client, ob krtutil.KrtOptions, endpoints krt.Collection[AFDEndpoint], services krt.Collection[*corev1.Service], refGrants krt.Collection[referenceGrantEdge], refGrantIdx krt.Index[string, referenceGrantEdge]) krt.Collection[AFDRoute] {
	routes := krt.WrapClient(kclient.New[*gatewayapiv1.HTTPRoute](c), ob.ToOptions("httproutes")...)

	return krt.NewManyCollection(routes, func(kctx krt.HandlerContext, route *gatewayapiv1.HTTPRoute) []AFDRoute {
		var out []AFDRoute
		for _, parent := range route.Spec.ParentRefs {
			if parent.Kind != nil && *parent.Kind != "Gateway" {
				continue
			}
			gwNS := route.Namespace
			if parent.Namespace != nil {
				gwNS = string(*parent.Namespace)
			}
			gwKey := types.NamespacedName{Namespace: gwNS, Name: string(parent.Name)}
			endpointPtr := krt.FetchOne(kctx, endpoints, krt.FilterKey(gwKey.String()))
			if endpointPtr == nil {
				continue
			}
			endpoint := *endpointPtr

			var origins []WeightedOrigin
			var missingBackendRefs, deniedRefs []string
			var allUnsupported []string
			var rules []AFDRule
			order := int32(0)

			for ruleIdx, rule := range route.Spec.Rules {
				actions, unsupported := toRuleActions(rule.Filters)
				allUnsupported = append(allUnsupported, unsupported...)

				matches := rule.Matches
				if len(matches) == 0 {
					matches = []gatewayapiv1.HTTPRouteMatch{{}}
				}
				for matchIdx, m := range matches {
					rules = append(rules, AFDRule{
						Name:            fmt.Sprintf("rule-%d-%d", ruleIdx, matchIdx),
						Order:           order + 1, // AFD reserves order 0 for the default rule
						MatchConditions: toMatchConditions(m),
						Actions:         actions,
					})
					order++
				}

				for _, br := range rule.BackendRefs {
					ns := route.Namespace
					if br.Namespace != nil {
						ns = string(*br.Namespace)
					}
					kind := "Service"
					if br.Kind != nil {
						kind = string(*br.Kind)
					}
					if kind != "Service" {
						missingBackendRefs = append(missingBackendRefs, fmt.Sprintf("backendRef %s/%s: kind %s is not supported", ns, br.Name, kind))
						continue
					}
					if ns != route.Namespace && !isReferenceAllowed(kctx, refGrants, refGrantIdx, route.Namespace, "HTTPRoute", ns, kind, string(br.Name)) {
						deniedRefs = append(deniedRefs, fmt.Sprintf("backendRef %s/%s: no ReferenceGrant permits this cross-namespace reference", ns, br.Name))
						continue
					}
					svcPtr := krt.FetchOne(kctx, services, krt.FilterKey(types.NamespacedName{Namespace: ns, Name: string(br.Name)}.String()))
					if svcPtr == nil {
						missingBackendRefs = append(missingBackendRefs, fmt.Sprintf("backendRef %s/%s: service not found", ns, br.Name))
						continue
					}
					origin, err := serviceToOrigin(**svcPtr, br)
					if err != nil {
						missingBackendRefs = append(missingBackendRefs, fmt.Sprintf("backendRef %s/%s: %v", ns, br.Name, err))
						continue
					}
					origins = append(origins, *origin)
				}
			}

			for _, hostname := range matchingHostnames(endpoint, route) {
				key := fmt.Sprintf("%s/%s/%s", route.Namespace, route.Name, hostname.Hostname)
				base := sanitizeAFDName(route.Namespace + "-" + route.Name + "-" + hostname.SanitizedName)
				out = append(out, AFDRoute{
					Key:                key,
					GatewayKey:         gwKey.String(),
					Name:               base,
					CustomDomain:       hostname.SanitizedName,
					OriginGroupName:    base,
					RuleSetName:        base,
					PatternsToMatch:    []string{"/*"},
					Origins:            origins,
					Rules:              rules,
					UnsupportedFilters: allUnsupported,
					MissingBackendRefs: missingBackendRefs,
					DeniedCrossNSRefs:  deniedRefs,
				})
			}
		}
		return out
	}, ob.ToOptions("afd-routes")...)
}

// serviceToOrigin resolves backendRef (a Service) to an AFD Origin.
// Services must be of type LoadBalancer with an assigned external
// IP/hostname: AFD cannot reach ClusterIP Services directly.
func serviceToOrigin(svc corev1.Service, br gatewayapiv1.HTTPBackendRef) (*WeightedOrigin, error) {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return nil, fmt.Errorf("service type %s is not supported; AFD requires a LoadBalancer-type Service (or Azure Private Link, not yet implemented)", svc.Spec.Type)
	}
	var host string
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.Hostname != "" {
			host = ing.Hostname
			break
		}
		if ing.IP != "" {
			host = ing.IP
			break
		}
	}
	if host == "" {
		return nil, fmt.Errorf("service has no LoadBalancer ingress IP/hostname yet")
	}

	weight := int32(1)
	if br.Weight != nil {
		weight = *br.Weight
	}
	port := int32(443)
	if br.Port != nil {
		port = int32(*br.Port)
	}

	return &WeightedOrigin{
		Name:      sanitizeAFDName(svc.Namespace + "-" + svc.Name + "-" + strconv.Itoa(int(port))),
		HostName:  host,
		HTTPPort:  80,
		HTTPSPort: port,
		Weight:    weight,
		Priority:  1,
	}, nil
}
