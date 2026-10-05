/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armdeploymentstacks"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/ptr"

	"go.goms.io/fleet-networking/pkg/apiclient"
	"go.goms.io/fleet-networking/pkg/common/krtutil"
)

const (
	// deploymentWorkerCount is the number of worker goroutines processing
	// queued endpoint/routing deployments concurrently.
	deploymentWorkerCount = 4
)

// Reconciler translates Gateway API intent (GatewayClass, Gateway,
// HTTPRoute, ReferenceGrant) into Azure Front Door configuration, applied
// as two ARM Deployment Stacks per Gateway (see endpoint_template.go,
// routing_template.go, and aggregate.go).
type Reconciler struct {
	client           apiclient.Client
	deploymentClient *armdeploymentstacks.Client

	gateways krt.Collection[*gatewayapiv1.Gateway]
	routes   krt.Collection[*gatewayapiv1.HTTPRoute]

	endpointDeployments krt.Collection[EndpointDeployment]
	routingDeployments  krt.Collection[RoutingDeployment]

	// endpointQueue/routingQueue decouple krt's reactive transforms from
	// the slow, blocking Azure deployment-stack calls, same pattern as
	// pkg/controllers/hub/globalserviceexport.
	endpointQueue workqueue.TypedRateLimitingInterface[string]
	routingQueue  workqueue.TypedRateLimitingInterface[string]
}

// NewReconciler builds the afdgateway controller. dc is reused from the
// same *armdeploymentstacks.Client the globalserviceexport controller
// uses; defaultResourceGroup is used for any Gateway that does not set
// AnnotationResourceGroup.
func NewReconciler(c apiclient.Client, dc *armdeploymentstacks.Client, defaultResourceGroup string) *Reconciler {
	ob := krtutil.NewKrtOptions(make(chan struct{}), new(krt.DebugHandler))

	services := krt.WrapClient(kclient.New[*corev1.Service](c), ob.ToOptions("afd-services")...)
	classes := NewGatewayClasses(c, ob)
	endpoints := NewEndpoints(c, ob, classes, defaultResourceGroup)
	refGrants, refGrantIdx := NewReferenceGrantEdges(c, ob)
	routes := NewRoutes(c, ob, endpoints, services, refGrants, refGrantIdx)
	endpointDeployments, routingDeployments := NewDeployments(ob, endpoints, routes)

	r := &Reconciler{
		client:              c,
		deploymentClient:    dc,
		gateways:            krt.WrapClient(kclient.New[*gatewayapiv1.Gateway](c), ob.ToOptions("afd-gateways-raw")...),
		routes:              krt.WrapClient(kclient.New[*gatewayapiv1.HTTPRoute](c), ob.ToOptions("afd-httproutes-raw")...),
		endpointDeployments: endpointDeployments,
		routingDeployments:  routingDeployments,
		endpointQueue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		routingQueue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
	}

	endpointDeployments.RegisterBatch(func(events []krt.Event[EndpointDeployment]) {
		for _, e := range events {
			if e.Event == controllers.EventDelete {
				r.deleteStack(e.Latest().Endpoint.ResourceGroup, "afd-endpoint-"+e.Latest().GatewayKeySafe())
				continue
			}
			r.endpointQueue.Add(e.Latest().ResourceName())
		}
	}, true)

	routingDeployments.RegisterBatch(func(events []krt.Event[RoutingDeployment]) {
		for _, e := range events {
			if e.Event == controllers.EventDelete {
				r.deleteStack(e.Latest().Endpoint.ResourceGroup, "afd-routing-"+e.Latest().GatewayKeySafe())
				continue
			}
			r.routingQueue.Add(e.Latest().ResourceName())
		}
	}, true)

	return r
}

// Start runs the deployment worker pools and the underlying krt client
// informers. It implements controller-runtime's manager.Runnable, matching
// globalserviceexport.Reconciler's Start method.
func (r *Reconciler) Start(ctx context.Context) error {
	for i := 0; i < deploymentWorkerCount; i++ {
		go r.runWorker(ctx, r.endpointQueue, r.processEndpointKey)
		go r.runWorker(ctx, r.routingQueue, r.processRoutingKey)
	}
	go func() {
		<-ctx.Done()
		r.endpointQueue.ShutDown()
		r.routingQueue.ShutDown()
	}()

	r.client.RunAndWait(ctx.Done())
	return nil
}

func (r *Reconciler) runWorker(ctx context.Context, q workqueue.TypedRateLimitingInterface[string], process func(context.Context, string) error) {
	for {
		key, shutdown := q.Get()
		if shutdown {
			return
		}
		if err := process(ctx, key); err != nil {
			klog.ErrorS(err, "Failed to process queued AFD deployment", "key", key)
		}
		q.Done(key)
		q.Forget(key)
	}
}

// GatewayKeySafe returns a GatewayKey with '/' replaced so it is usable as
// (part of) an ARM Deployment Stack name.
func (d EndpointDeployment) GatewayKeySafe() string { return sanitizeAFDName(d.GatewayKey) }

// GatewayKeySafe returns a GatewayKey with '/' replaced so it is usable as
// (part of) an ARM Deployment Stack name.
func (d RoutingDeployment) GatewayKeySafe() string { return sanitizeAFDName(d.GatewayKey) }

func (r *Reconciler) processEndpointKey(ctx context.Context, key string) error {
	dep := r.endpointDeployments.GetKey(key)
	if dep == nil {
		return nil
	}
	e := dep.Endpoint
	stackName := "afd-endpoint-" + dep.GatewayKeySafe()

	hostnames := make([]string, 0, len(e.CustomDomains))
	for _, d := range e.CustomDomains {
		hostnames = append(hostnames, d.Hostname)
	}

	outputs, err := r.writeStack(ctx, e.ResourceGroup, stackName, endpointTemplateInline, map[string]any{
		"profileName":           e.ProfileName,
		"endpointName":          e.EndpointName,
		"sku":                   e.SKU,
		"customDomainHostnames": hostnames,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to deploy AFD endpoint stack", "gateway", dep.GatewayKey)
		r.setGatewayProgrammed(ctx, dep.GatewayKey, false, "AFDEndpointDeploymentFailed", err.Error())
		return err
	}

	endpointHostName, _ := outputs["endpointHostName"].(string)
	r.setGatewayProgrammed(ctx, dep.GatewayKey, true, "Programmed", "AFD endpoint host: "+endpointHostName)
	return nil
}

func (r *Reconciler) processRoutingKey(ctx context.Context, key string) error {
	dep := r.routingDeployments.GetKey(key)
	if dep == nil {
		return nil
	}
	stackName := "afd-routing-" + dep.GatewayKeySafe()

	var originGroups, origins, ruleSets, rules, routeParams []map[string]any
	seenOG, seenRS := map[string]bool{}, map[string]bool{}
	for _, rt := range dep.Routes {
		if !seenOG[rt.OriginGroupName] {
			seenOG[rt.OriginGroupName] = true
			originGroups = append(originGroups, map[string]any{"name": rt.OriginGroupName})
		}
		for _, o := range rt.Origins {
			origins = append(origins, map[string]any{
				"originGroupName": rt.OriginGroupName,
				"name":            o.Name,
				"hostName":        o.HostName,
				"httpPort":        o.HTTPPort,
				"httpsPort":       o.HTTPSPort,
				"weight":          o.Weight,
				"priority":        o.Priority,
			})
		}
		if !seenRS[rt.RuleSetName] {
			seenRS[rt.RuleSetName] = true
			ruleSets = append(ruleSets, map[string]any{"name": rt.RuleSetName})
		}
		for _, rule := range rt.Rules {
			rules = append(rules, map[string]any{
				"ruleSetName":     rt.RuleSetName,
				"name":            rule.Name,
				"order":           rule.Order,
				"matchConditions": toBicepConditions(rule.MatchConditions),
				"actions":         toBicepActions(rule.Actions),
			})
		}
		routeParams = append(routeParams, map[string]any{
			"name":             rt.Name,
			"customDomainName": rt.CustomDomain,
			"originGroupName":  rt.OriginGroupName,
			"ruleSetName":      rt.RuleSetName,
			"patternsToMatch":  rt.PatternsToMatch,
		})
	}

	_, err := r.writeStack(ctx, dep.Endpoint.ResourceGroup, stackName, routingTemplateInline, map[string]any{
		"profileName":  dep.Endpoint.ProfileName,
		"endpointName": dep.Endpoint.EndpointName,
		"originGroups": originGroups,
		"origins":      origins,
		"ruleSets":     ruleSets,
		"rules":        rules,
		"routes":       routeParams,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to deploy AFD routing stack", "gateway", dep.GatewayKey)
	}
	r.setRouteStatuses(ctx, dep.Routes, err)
	return err
}

func toBicepConditions(cs []MatchCondition) []map[string]any {
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{
			"kind":     c.Kind,
			"operator": c.Operator,
			"selector": c.Selector,
			"values":   c.Values,
			"negate":   c.Negate,
		})
	}
	return out
}

func toBicepActions(as []RuleAction) []map[string]any {
	out := make([]map[string]any, 0, len(as))
	for _, a := range as {
		out = append(out, map[string]any{
			"kind":             a.Kind,
			"headerName":       a.HeaderName,
			"headerValue":      a.HeaderValue,
			"headerOp":         a.HeaderOp,
			"redirectScheme":   ptr.OrEmpty(a.RedirectScheme),
			"redirectHostname": ptr.OrEmpty(a.RedirectHostname),
			"redirectPath":     ptr.OrEmpty(a.RedirectPath),
			"redirectStatus":   ptr.OrDefault(a.RedirectStatus, 0),
			"rewriteHostname":  ptr.OrEmpty(a.RewriteHostname),
			"rewritePrefix":    ptr.OrEmpty(a.RewritePrefix),
		})
	}
	return out
}

// writeStack creates/updates an ARM Deployment Stack named stackName in rg
// from templateJSON and params, auto-pruning any resources this controller
// previously created there but no longer declares (ActionOnUnmanage ==
// Delete), and returns the deployment's outputs.
func (r *Reconciler) writeStack(ctx context.Context, rg, stackName, templateJSON string, params map[string]any) (map[string]any, error) {
	template := make(map[string]any)
	if err := json.Unmarshal([]byte(templateJSON), &template); err != nil {
		return nil, err
	}
	deployParams := make(map[string]*armdeploymentstacks.DeploymentParameter, len(params))
	for k, v := range params {
		deployParams[k] = &armdeploymentstacks.DeploymentParameter{Value: v}
	}
	denyMode := armdeploymentstacks.DenySettingsModeNone
	p, err := r.deploymentClient.BeginCreateOrUpdateAtResourceGroup(ctx, rg, stackName, armdeploymentstacks.DeploymentStack{
		Properties: &armdeploymentstacks.DeploymentStackProperties{
			ActionOnUnmanage: &armdeploymentstacks.ActionOnUnmanage{
				Resources:        ptr.Of(armdeploymentstacks.DeploymentStacksDeleteDetachEnumDelete),
				ResourceGroups:   ptr.Of(armdeploymentstacks.DeploymentStacksDeleteDetachEnumDetach),
				ManagementGroups: ptr.Of(armdeploymentstacks.DeploymentStacksDeleteDetachEnumDetach),
			},
			DenySettings: &armdeploymentstacks.DenySettings{Mode: &denyMode},
			Template:     template,
			Parameters:   deployParams,
		},
	}, nil)
	if err != nil {
		return nil, err
	}
	res, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	outputs, _ := res.DeploymentStack.Properties.Outputs.(map[string]any)
	resolved := make(map[string]any, len(outputs))
	for k, v := range outputs {
		if m, ok := v.(map[string]any); ok {
			resolved[k] = m["value"]
		}
	}
	return resolved, nil
}

func (r *Reconciler) deleteStack(rg, stackName string) {
	p, err := r.deploymentClient.BeginDeleteAtResourceGroup(context.Background(), rg, stackName, nil)
	if err != nil {
		klog.ErrorS(err, "Failed to start AFD stack deletion", "stack", stackName)
		return
	}
	if _, err := p.PollUntilDone(context.Background(), nil); err != nil {
		var respErr *azcore.ResponseError
		if ok := isNotFound(err, &respErr); ok {
			return
		}
		klog.ErrorS(err, "Failed to delete AFD stack", "stack", stackName)
	}
}

func isNotFound(err error, respErr **azcore.ResponseError) bool {
	var re *azcore.ResponseError
	if ok := asResponseError(err, &re); ok {
		*respErr = re
		return re.StatusCode == 404
	}
	return false
}

func asResponseError(err error, target **azcore.ResponseError) bool {
	re, ok := err.(*azcore.ResponseError)
	if ok {
		*target = re
	}
	return ok
}

// setGatewayProgrammed sets the Gateway's "Programmed" status condition.
func (r *Reconciler) setGatewayProgrammed(ctx context.Context, gatewayKey string, ok bool, reason, message string) {
	parts, err := splitNamespacedName(gatewayKey)
	if err != nil {
		klog.ErrorS(err, "Invalid gateway key", "key", gatewayKey)
		return
	}
	status := v1.ConditionFalse
	if ok {
		status = v1.ConditionTrue
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		gw, getErr := r.client.GatewayAPI().GatewayV1().Gateways(parts.Namespace).Get(ctx, parts.Name, v1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		cond := v1.Condition{
			Type:               string(gatewayapiv1.GatewayConditionProgrammed),
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: gw.Generation,
			LastTransitionTime: v1.Now(),
		}
		gw.Status.Conditions = mergeCondition(gw.Status.Conditions, cond)
		_, updateErr := r.client.GatewayAPI().GatewayV1().Gateways(parts.Namespace).UpdateStatus(ctx, gw, v1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		klog.ErrorS(err, "Failed to update Gateway status", "gateway", gatewayKey)
	}
}

// setRouteStatuses sets each HTTPRoute's Accepted/ResolvedRefs parent
// conditions from its AFDRoute translation results.
func (r *Reconciler) setRouteStatuses(ctx context.Context, routes []AFDRoute, deployErr error) {
	byRoute := map[string][]AFDRoute{}
	for _, rt := range routes {
		ns, name, _, err := splitRouteKey(rt.Key)
		if err != nil {
			continue
		}
		byRoute[ns+"/"+name] = append(byRoute[ns+"/"+name], rt)
	}
	for nsName, rts := range byRoute {
		parts, err := splitNamespacedName(nsName)
		if err != nil {
			continue
		}
		r.setOneRouteStatus(ctx, parts.Namespace, parts.Name, rts, deployErr)
	}
}

func (r *Reconciler) setOneRouteStatus(ctx context.Context, namespace, name string, rts []AFDRoute, deployErr error) {
	accepted, acceptedMsg := true, "Route accepted"
	resolved, resolvedMsg := true, "All backend references resolved"

	for _, rt := range rts {
		if len(rt.UnsupportedFilters) > 0 {
			accepted, acceptedMsg = false, fmt.Sprintf("unsupported filters: %v", rt.UnsupportedFilters)
		}
		if len(rt.MissingBackendRefs) > 0 {
			resolved, resolvedMsg = false, fmt.Sprintf("unresolved backendRefs: %v", rt.MissingBackendRefs)
		}
		if len(rt.DeniedCrossNSRefs) > 0 {
			resolved, resolvedMsg = false, fmt.Sprintf("denied cross-namespace backendRefs: %v", rt.DeniedCrossNSRefs)
		}
	}
	if deployErr != nil {
		accepted, acceptedMsg = false, "AFD deployment failed: "+deployErr.Error()
	}

	acceptedStatus, resolvedStatus := v1.ConditionTrue, v1.ConditionTrue
	if !accepted {
		acceptedStatus = v1.ConditionFalse
	}
	if !resolved {
		resolvedStatus = v1.ConditionFalse
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		route, getErr := r.client.GatewayAPI().GatewayV1().HTTPRoutes(namespace).Get(ctx, name, v1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		for i := range route.Status.Parents {
			route.Status.Parents[i].Conditions = mergeCondition(route.Status.Parents[i].Conditions, v1.Condition{
				Type: string(gatewayapiv1.RouteConditionAccepted), Status: acceptedStatus,
				Reason: "AFDGatewayController", Message: acceptedMsg,
				ObservedGeneration: route.Generation, LastTransitionTime: v1.Now(),
			})
			route.Status.Parents[i].Conditions = mergeCondition(route.Status.Parents[i].Conditions, v1.Condition{
				Type: string(gatewayapiv1.RouteConditionResolvedRefs), Status: resolvedStatus,
				Reason: "AFDGatewayController", Message: resolvedMsg,
				ObservedGeneration: route.Generation, LastTransitionTime: v1.Now(),
			})
		}
		_, updateErr := r.client.GatewayAPI().GatewayV1().HTTPRoutes(namespace).UpdateStatus(ctx, route, v1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		klog.ErrorS(err, "Failed to update HTTPRoute status", "namespace", namespace, "name", name)
	}
}

// mergeCondition replaces any existing condition of the same Type with
// cond, or appends it; other condition types are left untouched.
func mergeCondition(conds []v1.Condition, cond v1.Condition) []v1.Condition {
	for i, c := range conds {
		if c.Type == cond.Type {
			if c.Status == cond.Status && c.Reason == cond.Reason && c.Message == cond.Message {
				return conds // no-op: avoid an unnecessary UpdateStatus call
			}
			conds[i] = cond
			return conds
		}
	}
	return append(conds, cond)
}

type namespacedName struct {
	Namespace, Name string
}

func splitNamespacedName(key string) (namespacedName, error) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return namespacedName{Namespace: key[:i], Name: key[i+1:]}, nil
		}
	}
	return namespacedName{}, fmt.Errorf("invalid namespaced name %q", key)
}

// splitRouteKey splits an AFDRoute.Key ("namespace/name/hostname") back
// into its parts.
func splitRouteKey(key string) (namespace, name, hostname string, err error) {
	first := -1
	second := -1
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			if first == -1 {
				first = i
			} else {
				second = i
				break
			}
		}
	}
	if first == -1 || second == -1 {
		return "", "", "", fmt.Errorf("invalid route key %q", key)
	}
	return key[:first], key[first+1 : second], key[second+1:], nil
}
