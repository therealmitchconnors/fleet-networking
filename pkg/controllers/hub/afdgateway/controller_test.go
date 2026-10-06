/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armdeploymentstacks"
	armdsfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armdeploymentstacks/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// errTestDeploymentFailed is a sentinel used to make
// fakeDeploymentStacksClient's create handler fail.
var errTestDeploymentFailed = errors.New("simulated deployment failure")

// fakeDeploymentStacksClient builds an *armdeploymentstacks.Client whose
// create/delete handlers are controlled by createErr/deleteErr; on success,
// the create handler returns endpointHostName as the "endpointHostName"
// deployment output (consumed only by the endpoint stack; the routing
// stack ignores it).
func fakeDeploymentStacksClient(t *testing.T, endpointHostName string, createErr, deleteErr error) *armdeploymentstacks.Client {
	t.Helper()
	srv := armdsfake.Server{
		BeginCreateOrUpdateAtResourceGroup: func(_ context.Context, _ string, _ string, _ armdeploymentstacks.DeploymentStack, _ *armdeploymentstacks.ClientBeginCreateOrUpdateAtResourceGroupOptions) (resp azfake.PollerResponder[armdeploymentstacks.ClientCreateOrUpdateAtResourceGroupResponse], errResp azfake.ErrorResponder) {
			if createErr != nil {
				errResp.SetResponseError(http.StatusInternalServerError, "DeploymentFailed")
				return
			}
			resp.SetTerminalResponse(http.StatusOK, armdeploymentstacks.ClientCreateOrUpdateAtResourceGroupResponse{
				DeploymentStack: armdeploymentstacks.DeploymentStack{
					Properties: &armdeploymentstacks.DeploymentStackProperties{
						Outputs: map[string]interface{}{
							"endpointHostName": map[string]interface{}{
								"value": endpointHostName,
							},
						},
					},
				},
			}, nil)
			return
		},
		BeginDeleteAtResourceGroup: func(_ context.Context, _ string, _ string, _ *armdeploymentstacks.ClientBeginDeleteAtResourceGroupOptions) (resp azfake.PollerResponder[armdeploymentstacks.ClientDeleteAtResourceGroupResponse], errResp azfake.ErrorResponder) {
			if deleteErr != nil {
				errResp.SetResponseError(http.StatusInternalServerError, "DeleteFailed")
				return
			}
			resp.SetTerminalResponse(http.StatusOK, armdeploymentstacks.ClientDeleteAtResourceGroupResponse{}, nil)
			return
		},
	}
	transport := armdsfake.NewServerTransport(&srv)
	client, err := armdeploymentstacks.NewClient("00000000-0000-0000-0000-000000000000", &azfake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{Transport: transport},
	})
	if err != nil {
		t.Fatalf("failed to create fake armdeploymentstacks client: %v", err)
	}
	return client
}

// runReconciler starts the reconciler's krt-backed informers and worker
// pools in the background and returns a cancel func to stop them at the
// end of the test.
func runReconciler(t *testing.T, r *Reconciler) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := r.Start(ctx); err != nil {
			t.Logf("reconciler exited: %v", err)
		}
	}()
	return cancel
}

// waitForGatewayCondition polls the named Gateway until it has a condition
// of condType with status wantStatus, failing the test after testPollTimeout.
func waitForGatewayCondition(t *testing.T, client *testAPIClient, namespace, name, condType string, wantStatus metav1.ConditionStatus) {
	t.Helper()
	var gw *gatewayapiv1.Gateway
	err := wait.PollUntilContextTimeout(context.Background(), testPollInterval, testPollTimeout, true, func(ctx context.Context) (bool, error) {
		var getErr error
		gw, getErr = client.GatewayAPI().GatewayV1().Gateways(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, nil //nolint:nilerr // object may not exist yet
		}
		for _, c := range gw.Status.Conditions {
			if c.Type == condType && c.Status == wantStatus {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for condition %s=%s on gateway %s/%s: %v (latest: %+v)", condType, wantStatus, namespace, name, err, gw)
	}
}

// waitForRouteCondition polls the named HTTPRoute until one of its parent
// statuses has a condition of condType with status wantStatus, failing the
// test after testPollTimeout.
func waitForRouteCondition(t *testing.T, client *testAPIClient, namespace, name, condType string, wantStatus metav1.ConditionStatus) {
	t.Helper()
	var route *gatewayapiv1.HTTPRoute
	err := wait.PollUntilContextTimeout(context.Background(), testPollInterval, testPollTimeout, true, func(ctx context.Context) (bool, error) {
		var getErr error
		route, getErr = client.GatewayAPI().GatewayV1().HTTPRoutes(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, nil //nolint:nilerr // object may not exist yet
		}
		for _, p := range route.Status.Parents {
			for _, c := range p.Conditions {
				if c.Type == condType && c.Status == wantStatus {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for condition %s=%s on route %s/%s: %v (latest: %+v)", condType, wantStatus, namespace, name, err, route)
	}
}

func TestReconciler_DeploysEndpointAndRoutingStacksAndSetsStatus(t *testing.T) {
	gc := newGatewayClass("afd", ControllerName)
	gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")
	svc := newLBService("ns1", "svc1", "lb.example.com")
	route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("", "svc1"))
	// The Gateway must have a parent-visible status.Parents entry for the
	// route status writer to have somewhere to write; seed it the way the
	// Gateway API control plane would (one parent status per parentRef).
	route.Status.Parents = []gatewayapiv1.RouteParentStatus{{
		ParentRef:      gatewayapiv1.ParentReference{Name: "gw1"},
		ControllerName: ControllerName,
	}}

	client := newTestAPIClient(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(svc), runtime.Object(route))
	dc := fakeDeploymentStacksClient(t, "endpoint.z01.azurefd.net", nil, nil)

	r := NewReconciler(client, dc, "default-rg")
	defer runReconciler(t, r)()

	waitForGatewayCondition(t, client, "ns1", "gw1", string(gatewayapiv1.GatewayConditionProgrammed), metav1.ConditionTrue)
	waitForRouteCondition(t, client, "ns1", "route1", string(gatewayapiv1.RouteConditionAccepted), metav1.ConditionTrue)
	waitForRouteCondition(t, client, "ns1", "route1", string(gatewayapiv1.RouteConditionResolvedRefs), metav1.ConditionTrue)
}

func TestReconciler_EndpointDeploymentFailureMarksGatewayNotProgrammed(t *testing.T) {
	gc := newGatewayClass("afd", ControllerName)
	gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")

	client := newTestAPIClient(t, runtime.Object(gc), runtime.Object(gw))
	dc := fakeDeploymentStacksClient(t, "", errTestDeploymentFailed, nil)

	r := NewReconciler(client, dc, "default-rg")
	defer runReconciler(t, r)()

	waitForGatewayCondition(t, client, "ns1", "gw1", string(gatewayapiv1.GatewayConditionProgrammed), metav1.ConditionFalse)
}

func TestReconciler_GatewayDeletionDeletesEndpointStack(t *testing.T) {
	gc := newGatewayClass("afd", ControllerName)
	gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")

	client := newTestAPIClient(t, runtime.Object(gc), runtime.Object(gw))

	deleted := make(chan string, 2)
	srv := armdsfake.Server{
		BeginCreateOrUpdateAtResourceGroup: func(_ context.Context, _ string, _ string, _ armdeploymentstacks.DeploymentStack, _ *armdeploymentstacks.ClientBeginCreateOrUpdateAtResourceGroupOptions) (resp azfake.PollerResponder[armdeploymentstacks.ClientCreateOrUpdateAtResourceGroupResponse], errResp azfake.ErrorResponder) {
			resp.SetTerminalResponse(http.StatusOK, armdeploymentstacks.ClientCreateOrUpdateAtResourceGroupResponse{
				DeploymentStack: armdeploymentstacks.DeploymentStack{
					Properties: &armdeploymentstacks.DeploymentStackProperties{
						Outputs: map[string]interface{}{
							"endpointHostName": map[string]interface{}{"value": "endpoint.z01.azurefd.net"},
						},
					},
				},
			}, nil)
			return
		},
		BeginDeleteAtResourceGroup: func(_ context.Context, _ string, stackName string, _ *armdeploymentstacks.ClientBeginDeleteAtResourceGroupOptions) (resp azfake.PollerResponder[armdeploymentstacks.ClientDeleteAtResourceGroupResponse], errResp azfake.ErrorResponder) {
			deleted <- stackName
			resp.SetTerminalResponse(http.StatusOK, armdeploymentstacks.ClientDeleteAtResourceGroupResponse{}, nil)
			return
		},
	}
	transport := armdsfake.NewServerTransport(&srv)
	dc, err := armdeploymentstacks.NewClient("00000000-0000-0000-0000-000000000000", &azfake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{Transport: transport},
	})
	if err != nil {
		t.Fatalf("failed to create fake armdeploymentstacks client: %v", err)
	}

	r := NewReconciler(client, dc, "default-rg")
	defer runReconciler(t, r)()

	waitForGatewayCondition(t, client, "ns1", "gw1", string(gatewayapiv1.GatewayConditionProgrammed), metav1.ConditionTrue)

	if err := client.GatewayAPI().GatewayV1().Gateways("ns1").Delete(context.Background(), "gw1", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("failed to delete gateway: %v", err)
	}

	wantStacks := map[string]bool{"afd-endpoint-ns1-gw1": false, "afd-routing-ns1-gw1": false}
	for i := 0; i < len(wantStacks); i++ {
		select {
		case stackName := <-deleted:
			if _, ok := wantStacks[stackName]; !ok {
				t.Errorf("unexpected deleted stack %q", stackName)
			}
			wantStacks[stackName] = true
		case <-time.After(testPollTimeout):
			t.Fatalf("timed out waiting for stacks to be deleted; seen so far: %+v", wantStacks)
		}
	}
	for stack, seen := range wantStacks {
		if !seen {
			t.Errorf("stack %q was never deleted", stack)
		}
	}
}

func TestReconciler_UnresolvedBackendRefMarksRouteNotResolved(t *testing.T) {
	gc := newGatewayClass("afd", ControllerName)
	gw := newGateway("ns1", "gw1", "afd", nil, "www.contoso.com")
	route := newHTTPRoute("ns1", "route1", "ns1", "gw1", nil, newBackendRef("", "does-not-exist"))
	route.Status.Parents = []gatewayapiv1.RouteParentStatus{{
		ParentRef:      gatewayapiv1.ParentReference{Name: "gw1"},
		ControllerName: ControllerName,
	}}

	client := newTestAPIClient(t, runtime.Object(gc), runtime.Object(gw), runtime.Object(route))
	dc := fakeDeploymentStacksClient(t, "endpoint.z01.azurefd.net", nil, nil)

	r := NewReconciler(client, dc, "default-rg")
	defer runReconciler(t, r)()

	waitForRouteCondition(t, client, "ns1", "route1", string(gatewayapiv1.RouteConditionResolvedRefs), metav1.ConditionFalse)
}
