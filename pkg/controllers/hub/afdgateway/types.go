/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

// Package afdgateway features a controller that translates Kubernetes
// Gateway API Standard-channel resources (GatewayClass, Gateway, HTTPRoute,
// ReferenceGrant) into Azure Front Door (AFD) configuration. It is built
// with Istio's krt library: each Gateway API resource type is wrapped in a
// krt.Collection, and a chain of derived collections transforms that intent
// into the two ARM Deployment Stacks (see endpoint_template.go and
// routing_template.go) that are actually applied to Azure.
//
// GRPCRoute, TCPRoute, UDPRoute, and BackendTLSPolicy are intentionally out
// of scope for this controller (see the breadcrumb at
// .github/.copilot/breadcrumbs/2026-10-05-2059-gateway-api-to-afd-mapping.md
// for the full Gateway API -> AFD feature comparison and rationale).
package afdgateway

import (
	"fmt"
	"regexp"
	"strings"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pkg/kube/krt"
)

const (
	// ControllerName is the GatewayClass controllerName this controller
	// manages Gateways for.
	ControllerName = "networking.fleet.azure.com/afd-gateway"

	// AnnotationResourceGroup overrides the default Azure resource group a
	// Gateway's AFD Profile/Endpoint are deployed into. Set on the Gateway.
	AnnotationResourceGroup = "networking.fleet.azure.com/resource-group"
	// AnnotationSKU selects the Front Door SKU ("Standard_AzureFrontDoor" or
	// "Premium_AzureFrontDoor"). Set on the Gateway. Defaults to
	// DefaultSKU when unset.
	AnnotationSKU = "networking.fleet.azure.com/sku"

	// DefaultSKU is used when a Gateway does not set AnnotationSKU.
	DefaultSKU = "Standard_AzureFrontDoor"
)

// AFDGatewayClass is the (trivial) output of NewGatewayClasses: it exists
// only so NewEndpoints can efficiently check, via krt.FetchOne, whether a
// Gateway's GatewayClass is one this controller owns.
type AFDGatewayClass struct {
	Name string
}

// ResourceName implements krt.ResourceNamer.
func (c AFDGatewayClass) ResourceName() string { return c.Name }

var _ krt.ResourceNamer = AFDGatewayClass{}

// CustomDomainSpec is one Gateway listener hostname to be bound to the AFD
// endpoint as a Microsoft.Cdn/profiles/customDomains resource.
type CustomDomainSpec struct {
	// Hostname is the listener hostname, e.g. "www.contoso.com".
	Hostname string
	// SanitizedName is the AFD-resource-name-safe form of Hostname, used as
	// both the ARM resource name and the join key routes use to reference
	// their customDomain (see AFDRoute.CustomDomainName).
	SanitizedName string
}

// AFDEndpoint is the output of NewEndpoints: one Kubernetes Gateway maps to
// exactly one AFD Profile + one AFD Endpoint (+ one CustomDomain per
// distinct listener hostname).
type AFDEndpoint struct {
	NamespacedName types.NamespacedName
	ResourceGroup  string
	SKU            string
	ProfileName    string
	EndpointName   string
	CustomDomains  []CustomDomainSpec
}

// ResourceName implements krt.ResourceNamer.
func (e AFDEndpoint) ResourceName() string { return e.NamespacedName.String() }

var _ krt.ResourceNamer = AFDEndpoint{}

// WeightedOrigin is one HTTPRoute backendRef, resolved to a concrete
// origin hostname/port pair with its Gateway-API-derived weight.
type WeightedOrigin struct {
	Name      string
	HostName  string
	HTTPPort  int32
	HTTPSPort int32
	Weight    int32
	Priority  int32
}

// MatchCondition is one AFD Rule Set match condition, derived from an
// HTTPRouteMatch (path, header, query param, or method).
type MatchCondition struct {
	Kind     string // "Path" | "RequestHeader" | "QueryString" | "RequestMethod"
	Operator string // e.g. "Equal", "BeginsWith", "RegEx"
	Selector string // header/query-param name; empty for Path/RequestMethod
	Values   []string
	Negate   bool
}

// RuleAction is one AFD Rule action, derived from an HTTPRouteFilter.
type RuleAction struct {
	Kind string // "RequestHeaderModifier" | "ResponseHeaderModifier" | "RequestRedirect" | "URLRewrite"

	// RequestHeaderModifier / ResponseHeaderModifier
	HeaderName  string
	HeaderValue string
	HeaderOp    string // "Append" | "Overwrite" | "Delete"

	// RequestRedirect
	RedirectScheme   *string
	RedirectHostname *string
	RedirectPath     *string
	RedirectStatus   *int32

	// URLRewrite
	RewriteHostname *string
	RewritePrefix   *string
}

// AFDRule is one AFD Rule Set rule, derived from one HTTPRoute rule.
type AFDRule struct {
	Name            string
	Order           int32
	MatchConditions []MatchCondition
	Actions         []RuleAction
}

// AFDRoute is the output of NewHTTPRoutes: one (HTTPRoute, matched
// hostname) pair maps to one AFD Route, with its own Origin Group/Origins
// and Rule Set. GatewayKey joins back to the owning AFDEndpoint.
type AFDRoute struct {
	// Key uniquely identifies this AFDRoute: namespace/name/hostname of the
	// owning HTTPRoute.
	Key        string
	GatewayKey string // types.NamespacedName.String() of the parent Gateway

	Name            string // AFD route resource name
	CustomDomain    string // listener hostname this route is bound to
	OriginGroupName string
	RuleSetName     string
	PatternsToMatch []string
	Origins         []WeightedOrigin
	Rules           []AFDRule

	// Status inputs, consumed by the status-writer to set HTTPRoute
	// Accepted/ResolvedRefs parent conditions.
	UnsupportedFilters []string
	MissingBackendRefs []string
	DeniedCrossNSRefs  []string
}

// ResourceName implements krt.ResourceNamer.
func (r AFDRoute) ResourceName() string { return r.Key }

var _ krt.ResourceNamer = AFDRoute{}

// nameKey is a minimal (namespace, name) composite krt.Fetch key.
// Duplicated from pkg/controllers/hub/globalserviceexport/controller.go
// (see that file's comment: this is boilerplate krt.NewNamed will subsume
// once istio 1.29 ships).
type nameKey struct {
	krt.Named
}

func (n nameKey) String() string {
	return fmt.Sprintf("%s/%s", n.Namespace, n.Name)
}

func newNameKey(o v1.Object) nameKey {
	return nameKey{Named: krt.NewNamed(o)}
}

// afdNameRe matches characters AFD resource names cannot contain; anything
// else is replaced with '-'. AFD resource names must start/end with an
// alphanumeric and be <= 90 characters; the length cap is applied by the
// caller.
var afdNameRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitizeAFDName converts s into a legal AFD child-resource name.
func sanitizeAFDName(s string) string {
	s = afdNameRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if s == "" {
		s = "default"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// customDomainResourceName mirrors endpoint.bicep's
// `replace(replace(hostname, '.', '-'), '*', 'wildcard')` exactly: it must
// produce the same AFD customDomain resource name the endpoint stack
// actually deploys, since routing.bicep looks custom domains up by this
// name via resourceId(). Do not change this without updating endpoint.bicep
// in lockstep.
func customDomainResourceName(hostname string) string {
	s := strings.ReplaceAll(hostname, ".", "-")
	s = strings.ReplaceAll(s, "*", "wildcard")
	return s
}
