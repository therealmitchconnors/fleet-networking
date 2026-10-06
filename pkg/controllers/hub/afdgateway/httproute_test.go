/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func strPtr(s string) *string { return &s }

func TestToMatchConditions(t *testing.T) {
	tests := []struct {
		name  string
		match gatewayapiv1.HTTPRouteMatch
		want  []MatchCondition
	}{
		{
			name: "exact path",
			match: gatewayapiv1.HTTPRouteMatch{
				Path: &gatewayapiv1.HTTPPathMatch{
					Type:  ptrPathMatchType(gatewayapiv1.PathMatchExact),
					Value: strPtr("/foo"),
				},
			},
			want: []MatchCondition{{Kind: "UrlPath", Operator: "Equal", Values: []string{"/foo"}}},
		},
		{
			name: "prefix path defaults",
			match: gatewayapiv1.HTTPRouteMatch{
				Path: &gatewayapiv1.HTTPPathMatch{Value: strPtr("/bar")},
			},
			want: []MatchCondition{{Kind: "UrlPath", Operator: "BeginsWith", Values: []string{"/bar"}}},
		},
		{
			name: "regex path",
			match: gatewayapiv1.HTTPRouteMatch{
				Path: &gatewayapiv1.HTTPPathMatch{
					Type:  ptrPathMatchType(gatewayapiv1.PathMatchRegularExpression),
					Value: strPtr("^/api/.*"),
				},
			},
			want: []MatchCondition{{Kind: "UrlPath", Operator: "RegEx", Values: []string{"^/api/.*"}}},
		},
		{
			name: "header and query and method combined",
			match: gatewayapiv1.HTTPRouteMatch{
				Headers: []gatewayapiv1.HTTPHeaderMatch{
					{Name: "x-version", Value: "v2"},
				},
				QueryParams: []gatewayapiv1.HTTPQueryParamMatch{
					{Name: "debug", Value: "true"},
				},
				Method: ptrMethod(gatewayapiv1.HTTPMethodPost),
			},
			want: []MatchCondition{
				{Kind: "RequestHeader", Operator: "Equal", Selector: "x-version", Values: []string{"v2"}},
				{Kind: "QueryString", Operator: "Equal", Selector: "debug", Values: []string{"true"}},
				{Kind: "RequestMethod", Operator: "Equal", Values: []string{"POST"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toMatchConditions(tt.match)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("toMatchConditions() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func ptrPathMatchType(t gatewayapiv1.PathMatchType) *gatewayapiv1.PathMatchType { return &t }
func ptrMethod(m gatewayapiv1.HTTPMethod) *gatewayapiv1.HTTPMethod              { return &m }

func TestToRuleActions(t *testing.T) {
	filters := []gatewayapiv1.HTTPRouteFilter{
		{
			Type: gatewayapiv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayapiv1.HTTPHeaderFilter{
				Set:    []gatewayapiv1.HTTPHeader{{Name: "x-set", Value: "1"}},
				Add:    []gatewayapiv1.HTTPHeader{{Name: "x-add", Value: "2"}},
				Remove: []string{"x-remove"},
			},
		},
		{
			Type: gatewayapiv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayapiv1.HTTPRequestRedirectFilter{
				Scheme:     strPtr("https"),
				StatusCode: intPtr(301),
			},
		},
		{
			Type: gatewayapiv1.HTTPRouteFilterRequestMirror,
		},
	}

	actions, unsupported := toRuleActions(filters)

	wantActions := []RuleAction{
		{Kind: "ModifyRequestHeader", HeaderName: "x-set", HeaderValue: "1", HeaderOp: "Overwrite"},
		{Kind: "ModifyRequestHeader", HeaderName: "x-add", HeaderValue: "2", HeaderOp: "Append"},
		{Kind: "ModifyRequestHeader", HeaderName: "x-remove", HeaderOp: "Delete"},
		{Kind: "UrlRedirect", RedirectScheme: strPtr("https"), RedirectStatus: int32Ptr(301)},
	}
	if diff := cmp.Diff(wantActions, actions); diff != "" {
		t.Errorf("toRuleActions() actions mismatch (-want +got):\n%s", diff)
	}
	if len(unsupported) != 1 {
		t.Errorf("toRuleActions() expected exactly 1 unsupported filter (RequestMirror), got %v", unsupported)
	}
}

func intPtr(i int) *int       { return &i }
func int32Ptr(i int32) *int32 { return &i }

func TestHostnameMatches(t *testing.T) {
	tests := []struct {
		pattern, candidate string
		want               bool
	}{
		{"www.contoso.com", "www.contoso.com", true},
		{"*.contoso.com", "www.contoso.com", true},
		{"*.contoso.com", "contoso.com", false},
		{"www.contoso.com", "other.contoso.com", false},
		{"", "anything.com", true},
	}
	for _, tt := range tests {
		if got := hostnameMatches(tt.pattern, tt.candidate); got != tt.want {
			t.Errorf("hostnameMatches(%q, %q) = %v, want %v", tt.pattern, tt.candidate, got, tt.want)
		}
	}
}

func TestCustomDomainResourceName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"www.contoso.com", "www-contoso-com"},
		{"*.contoso.com", "wildcard-contoso-com"},
	}
	for _, tt := range tests {
		if got := customDomainResourceName(tt.in); got != tt.want {
			t.Errorf("customDomainResourceName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeAFDName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"foo/bar", "foo-bar"},
		{"default/my-gateway", "default-my-gateway"},
		{"foo.bar_baz", "foo-bar_baz"},
		{"***", "default"},
		{"-leading-and-trailing-", "leading-and-trailing"},
		{"", "default"},
	}
	for _, tt := range tests {
		if got := sanitizeAFDName(tt.in); got != tt.want {
			t.Errorf("sanitizeAFDName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	// Names longer than 80 chars are truncated.
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	if got := sanitizeAFDName(long); len(got) != 80 {
		t.Errorf("sanitizeAFDName(long) len = %d, want 80", len(got))
	}
}

func TestResourceNamers(t *testing.T) {
	if got, want := (AFDGatewayClass{Name: "afd"}).ResourceName(), "afd"; got != want {
		t.Errorf("AFDGatewayClass.ResourceName() = %q, want %q", got, want)
	}
	ep := AFDEndpoint{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "gw"}}
	if got, want := ep.ResourceName(), "ns/gw"; got != want {
		t.Errorf("AFDEndpoint.ResourceName() = %q, want %q", got, want)
	}
	route := AFDRoute{Key: "ns/route/www.contoso.com"}
	if got, want := route.ResourceName(), "ns/route/www.contoso.com"; got != want {
		t.Errorf("AFDRoute.ResourceName() = %q, want %q", got, want)
	}
}

func TestMatchingHostnames(t *testing.T) {
	endpoint := AFDEndpoint{
		CustomDomains: []CustomDomainSpec{
			{Hostname: "www.contoso.com", SanitizedName: "www-contoso-com"},
			{Hostname: "*.fabrikam.com", SanitizedName: "wildcard-fabrikam-com"},
		},
	}

	tests := []struct {
		name      string
		hostnames []gatewayapiv1.Hostname
		want      []string // Hostname field of expected CustomDomainSpec matches
	}{
		{
			name:      "no route hostnames returns every listener hostname",
			hostnames: nil,
			want:      []string{"www.contoso.com", "*.fabrikam.com"},
		},
		{
			name:      "exact route hostname matches exact listener",
			hostnames: []gatewayapiv1.Hostname{"www.contoso.com"},
			want:      []string{"www.contoso.com"},
		},
		{
			name:      "exact route hostname matches wildcard listener",
			hostnames: []gatewayapiv1.Hostname{"api.fabrikam.com"},
			want:      []string{"*.fabrikam.com"},
		},
		{
			name:      "non-matching route hostname yields nothing",
			hostnames: []gatewayapiv1.Hostname{"other.example.com"},
			want:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := &gatewayapiv1.HTTPRoute{Spec: gatewayapiv1.HTTPRouteSpec{Hostnames: tt.hostnames}}
			got := matchingHostnames(endpoint, route)
			var gotHostnames []string
			for _, d := range got {
				gotHostnames = append(gotHostnames, d.Hostname)
			}
			if diff := cmp.Diff(tt.want, gotHostnames); diff != "" {
				t.Errorf("matchingHostnames() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestServiceToOrigin(t *testing.T) {
	lbService := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{Hostname: "lb.example.com"}},
			},
		},
	}

	t.Run("resolves hostname, default weight/port", func(t *testing.T) {
		got, err := serviceToOrigin(lbService, gatewayapiv1.HTTPBackendRef{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := &WeightedOrigin{
			Name:      "ns-svc-443",
			HostName:  "lb.example.com",
			HTTPPort:  80,
			HTTPSPort: 443,
			Weight:    1,
			Priority:  1,
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("serviceToOrigin() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("honors explicit weight and port", func(t *testing.T) {
		br := gatewayapiv1.HTTPBackendRef{
			BackendRef: gatewayapiv1.BackendRef{
				BackendObjectReference: gatewayapiv1.BackendObjectReference{Port: portPtr(8443)},
				Weight:                 int32Ptr(50),
			},
		}
		got, err := serviceToOrigin(lbService, br)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Weight != 50 || got.HTTPSPort != 8443 {
			t.Errorf("serviceToOrigin() = %+v, want weight=50 httpsPort=8443", got)
		}
	})

	t.Run("falls back to IP when hostname unset", func(t *testing.T) {
		svc := lbService
		svc.Status = corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: "1.2.3.4"}},
			},
		}
		got, err := serviceToOrigin(svc, gatewayapiv1.HTTPBackendRef{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.HostName != "1.2.3.4" {
			t.Errorf("serviceToOrigin() hostname = %q, want 1.2.3.4", got.HostName)
		}
	})

	t.Run("rejects non-LoadBalancer service", func(t *testing.T) {
		svc := lbService
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		if _, err := serviceToOrigin(svc, gatewayapiv1.HTTPBackendRef{}); err == nil {
			t.Error("expected error for ClusterIP service, got nil")
		}
	})

	t.Run("rejects LoadBalancer with no ingress", func(t *testing.T) {
		svc := lbService
		svc.Status = corev1.ServiceStatus{}
		if _, err := serviceToOrigin(svc, gatewayapiv1.HTTPBackendRef{}); err == nil {
			t.Error("expected error for service with no LoadBalancer ingress, got nil")
		}
	})
}

func portPtr(p int) *gatewayapiv1.PortNumber {
	port := gatewayapiv1.PortNumber(p)
	return &port
}
