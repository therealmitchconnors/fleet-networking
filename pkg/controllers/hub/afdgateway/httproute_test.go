/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"testing"

	"github.com/google/go-cmp/cmp"
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
