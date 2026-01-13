// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package globalserviceexport

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armdeploymentstacks"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"go.goms.io/fleet-networking/api/v1alpha1"
	"go.goms.io/fleet-networking/pkg/apiclient"
	ac "go.goms.io/fleet-networking/pkg/applyconfigurations/api/v1alpha1"
	"go.goms.io/fleet-networking/pkg/common/krtutil"
	"go.goms.io/fleet-networking/pkg/common/objectmeta"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/ptr"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	acv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/klog/v2"
)

const (
	globalAnnotation        = "globalLBName"
	targetGatewayAnnotation = "targetGateway"
	fieldManagerName        = "globalserviceexport-controller"
)

type parameters struct {
	Name          string `json:"name"`
	rg            string
	Backends      []string `json:"backends"`
	targetGateway string
	serviceName   string
	namespace     string
}

// ResourceName implements krt.ResourceNamer.
func (p parameters) ResourceName() string {
	return fmt.Sprintf("%s.%s", p.rg, p.Name)
}

type output struct {
	publicGlobalIPAddress string
	targetGateway         string
	serviceName           string
	namespace             string
}

func (p output) ResourceName() string {
	return fmt.Sprintf("%s.%s", p.namespace, p.serviceName)
}

var (
	_ krt.ResourceNamer = parameters{}
	_ krt.ResourceNamer = output{}
)

type Reconciler struct {
	client apiclient.Client

	resourceGroupName string // default resource group name to create public IP address
	deploymentClient  *armdeploymentstacks.Client
	resourceClient    *armresources.Client
}

func NewReconciler(c apiclient.Client, dc *armdeploymentstacks.Client, rc *armresources.Client, defaultRG string) *Reconciler {
	ob := krtutil.NewKrtOptions(make(chan struct{}), new(krt.DebugHandler))

	mclbs := krt.WrapClient(kclient.New[*v1alpha1.MultiClusterLoadBalancer](c), ob.ToOptions("multiclusterloadbalancers")...)
	ises := krt.WrapClient(kclient.New[*v1alpha1.InternalServiceExport](c), ob.ToOptions("internalserviceexports")...)
	r := &Reconciler{
		client:            c,
		resourceClient:    rc,
		deploymentClient:  dc,
		resourceGroupName: defaultRG,
	}

	iseIndex := krt.NewIndex(ises, "internal service export by service reference name", func(export *v1alpha1.InternalServiceExport) []NameKey {
		return []NameKey{{
			Named: krt.Named{
				Name:      export.Spec.ServiceReference.Name,
				Namespace: export.Spec.ServiceReference.Namespace,
			},
		}}
	})

	outputs := krt.NewCollection(mclbs, func(kctx krt.HandlerContext, mclb *v1alpha1.MultiClusterLoadBalancer) *output {
		targetGateway := mclb.Annotations[targetGatewayAnnotation]
		internalServiceExports := krt.Fetch(kctx, ises, krt.FilterIndex(iseIndex, NewNameKey(mclb)))
		rg := strings.TrimSpace(mclb.Annotations[objectmeta.ServiceAnnotationLoadBalancerResourceGroup])
		if len(rg) < 1 {
			rg = r.resourceGroupName
		}
		if mclb.DeletionTimestamp != nil {
			err := r.deleteDeployment(mclb.Name, rg)
			if err != nil {
				klog.ErrorS(err, "Failed to delete global service deployment", "name", mclb.Name, "namespace", mclb.Namespace)
				kctx.DiscardResult()
				return nil
			}
			// remove finalizer
			r.RemoveFinalizer(mclb)
			if err != nil {
				klog.ErrorS(err, "Failed to remove mclb finalizer", "name", mclb.Name, "namespace", mclb.Namespace)
				kctx.DiscardResult()
			}
			return nil
		}
		out := &parameters{
			Name:          fmt.Sprintf("%s-%s", mclb.Namespace, mclb.Name),
			rg:            rg,
			targetGateway: targetGateway,
			serviceName:   mclb.Name,
			namespace:     mclb.Namespace,
		}
		for _, ise := range internalServiceExports {
			// for each public ip, get frontend config id
			pip, err := r.resourceClient.GetByID(context.Background(), *ise.Spec.PublicIPResourceID, "2024-10-01", &armresources.ClientGetByIDOptions{})
			if err != nil {
				klog.ErrorS(err, "No Public IP found for InternalServiceExport", "name", ise.Spec.ServiceReference.NamespacedName)
				r.ApplyStatusInvalid(mclb, ise.Spec.ServiceReference.NamespacedName)
				kctx.DiscardResult()
				return nil
			}

			ipConfig := pip.Properties.(map[string]interface{})["ipConfiguration"].(map[string]interface{})
			cfgID, _ := ipConfig["id"].(string)
			out.Backends = append(out.Backends, cfgID)
		}
		r.ApplyStatusValid(mclb)
		// Add finalizer if not present
		go r.ApplyFinalizer(mclb)

		// TODO: this blocks for way too long, and needs to be factored out.
		// TODO: this seems to run repeatedly, why?
		ipAddress, err := r.writeDeployment(*out)
		if err != nil {
			klog.ErrorS(err, "Failed to deploy global load balancer", "name", out.Name)
			r.ApplyStatusFailed(mclb)
			// TODO: write event to mclb with error details
			kctx.DiscardResult()
		}
		go r.ApplyStatusDeployed(mclb, len(out.Backends), ipAddress)
		return &output{
			publicGlobalIPAddress: ipAddress,
			targetGateway:         out.targetGateway,
			serviceName:           out.serviceName,
			namespace:             out.namespace,
		}
	})
	krt.NewCollection(outputs, func(kctx krt.HandlerContext, out output) *string {
		// annotate the service or gw with the global ip
		patchStr := fmt.Sprintf(`{"metadata":{"annotations":{"service.beta.kubernetes.io/azure-additional-public-ips": %q}}}`, out.publicGlobalIPAddress)
		var err error
		if out.targetGateway == "" {
			_, err = r.client.Kube().CoreV1().Services(out.namespace).Patch(context.Background(), out.serviceName, types.MergePatchType,
				[]byte(patchStr), v1.PatchOptions{})
		} else {
			_, err = r.client.GatewayAPI().GatewayV1beta1().Gateways(out.namespace).Patch(context.Background(), out.targetGateway, types.MergePatchType,
				[]byte(patchStr), v1.PatchOptions{})
		}
		if err != nil {
			klog.ErrorS(err, "Failed to annotate target", "name", out.serviceName, "gateway", out.targetGateway, "error", err)
			kctx.DiscardResult()
		}
		return nil
	})
	return r
}

func (r *Reconciler) Start(ctx context.Context) error {
	r.client.RunAndWait(ctx.Done())
	return nil
}

func (r *Reconciler) deleteDeployment(name, rg string) error {
	p, err := r.deploymentClient.BeginDeleteAtResourceGroup(context.Background(), rg, name, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(context.Background(), nil)
	if respErr, ok := err.(*azcore.ResponseError); ok {
		if respErr.StatusCode == 404 {
			// already deleted
			return nil
		}
	}
	return err
}

func (r *Reconciler) writeDeployment(params parameters) (string, error) {
	template := make(map[string]interface{})
	if err := json.Unmarshal([]byte(templateInline), &template); err != nil {
		return "", err
	}
	foo := armdeploymentstacks.DenySettingsModeNone
	p, err := r.deploymentClient.BeginCreateOrUpdateAtResourceGroup(context.Background(), params.rg, params.Name, armdeploymentstacks.DeploymentStack{
		Properties: &armdeploymentstacks.DeploymentStackProperties{
			ActionOnUnmanage: &armdeploymentstacks.ActionOnUnmanage{
				Resources:        ptr.Of(armdeploymentstacks.DeploymentStacksDeleteDetachEnumDelete),
				ResourceGroups:   ptr.Of(armdeploymentstacks.DeploymentStacksDeleteDetachEnumDetach),
				ManagementGroups: ptr.Of(armdeploymentstacks.DeploymentStacksDeleteDetachEnumDetach),
			},
			DenySettings: &armdeploymentstacks.DenySettings{
				Mode: &foo,
			},
			Template: template,
			Parameters: map[string]*armdeploymentstacks.DeploymentParameter{
				"name": {
					Value: params.Name,
				},
				"backends": {
					Value: params.Backends,
				},
			},
		},
	}, nil)
	if err != nil {
		return "", err
	}
	res, err := p.PollUntilDone(context.Background(), nil)
	log.Default().Printf("Deployment result: %v\n", res)
	outputs := res.DeploymentStack.Properties.Outputs.(map[string]interface{})
	ipAddress := outputs["publicGlobalIPAddress"].(map[string]interface{})["value"].(string)
	return ipAddress, err
}

func (r *Reconciler) RemoveFinalizer(mclb *v1alpha1.MultiClusterLoadBalancer) error {
	index := -1
	for i, f := range mclb.Finalizers {
		if f == "mclb" {
			index = i
			break
		}
	}
	if index == -1 {
		// finalizer not found
		return nil
	}
	_, err := r.client.Networking().NetworkingV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).Patch(
		context.Background(), mclb.Name, types.JSONPatchType, []byte(fmt.Sprintf("[ { \"op\": \"remove\", \"path\": \"/metadata/finalizers/%d\" } ]", index)),
		v1.PatchOptions{},
	)
	if err != nil {
		klog.ErrorS(err, "Failed to remove finalizer from MultiClusterLoadBalancer", "name", mclb.Name, "namespace", mclb.Namespace)
	}
	return err
}

func (r *Reconciler) ApplyFinalizer(mclb *v1alpha1.MultiClusterLoadBalancer) error {
	x := ac.MultiClusterLoadBalancer(mclb.Name, mclb.Namespace).
		WithFinalizers("mclb")
	_, err := r.client.Networking().NetworkingV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).Apply(
		context.Background(),
		x,
		v1.ApplyOptions{
			FieldManager: fieldManagerName,
		},
	)
	if err != nil {
		klog.ErrorS(err, "Failed to apply finalizer to MultiClusterLoadBalancer", "name", mclb.Name, "namespace", mclb.Namespace)
	}
	return err
}

func conditionExists(conditions []v1.Condition, condType string, status *v1.ConditionStatus) (v1.Condition, bool) {
	for _, c := range conditions {
		if c.Type == condType && string(c.Status) == string(*status) {
			return c, true
		}
	}
	return v1.Condition{}, false
}

func (r *Reconciler) ApplyStatusDeployed(mclb *v1alpha1.MultiClusterLoadBalancer, i int, ipAddress string) {
	statusAC := ac.MultiClusterLoadBalancerStatus().WithLoadBalancer(corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{
			{
				IP: ipAddress,
			},
		},
	})
	statusAC.WithConditions(processConditions(mclb.Status.Conditions, mclb.Generation, getValidCondition(), getDeployedCondition(i))...)
	_, err := r.client.Networking().NetworkingV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).ApplyStatus(context.Background(),
		ac.MultiClusterLoadBalancer(mclb.Name, mclb.Namespace).WithStatus(
			statusAC,
		),
		v1.ApplyOptions{
			FieldManager: fieldManagerName,
		},
	)
	if err != nil {
		klog.ErrorS(err, "Failed to apply deployed status to MultiClusterLoadBalancer", "name", mclb.Name, "namespace", mclb.Namespace)
	}
}

func (r *Reconciler) ApplyStatusFailed(mclb *v1alpha1.MultiClusterLoadBalancer) {
	statusAC := ac.MultiClusterLoadBalancerStatus()
	statusAC.WithConditions(processConditions(mclb.Status.Conditions, mclb.Generation, getInvalidCondition(mclb.Name), getDeploFailedCondition())...)
	_, err := r.client.Networking().NetworkingV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).ApplyStatus(context.Background(),
		ac.MultiClusterLoadBalancer(mclb.Name, mclb.Namespace).WithStatus(
			statusAC,
		),
		v1.ApplyOptions{
			FieldManager: fieldManagerName,
		},
	)
	if err != nil {
		klog.ErrorS(err, "Failed to apply failed status to MultiClusterLoadBalancer", "name", mclb.Name, "namespace", mclb.Namespace)
	}
}

func (r *Reconciler) ApplyStatusInvalid(mclb *v1alpha1.MultiClusterLoadBalancer, seName string) {
	statusAC := ac.MultiClusterLoadBalancerStatus()
	statusAC.WithConditions(processConditions(mclb.Status.Conditions, mclb.Generation, getInvalidCondition(seName))...)
	_, err := r.client.Networking().NetworkingV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).ApplyStatus(context.Background(),
		ac.MultiClusterLoadBalancer(mclb.Name, mclb.Namespace).WithStatus(
			statusAC,
		),
		v1.ApplyOptions{
			FieldManager: fieldManagerName,
		},
	)
	if err != nil {
		klog.ErrorS(err, "Failed to apply invalid status to MultiClusterLoadBalancer", "name", mclb.Name, "namespace", mclb.Namespace)
	}
}

func (r *Reconciler) ApplyStatusValid(mclb *v1alpha1.MultiClusterLoadBalancer) {
	statusAC := ac.MultiClusterLoadBalancerStatus()
	statusAC.WithConditions(processConditions(mclb.Status.Conditions, mclb.Generation, getValidCondition())...)
	_, err := r.client.Networking().NetworkingV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).ApplyStatus(context.Background(),
		ac.MultiClusterLoadBalancer(mclb.Name, mclb.Namespace).WithStatus(
			statusAC,
		),
		v1.ApplyOptions{
			FieldManager: fieldManagerName,
		},
	)
	if err != nil {
		klog.ErrorS(err, "Failed to apply valid status to MultiClusterLoadBalancer", "name", mclb.Name, "namespace", mclb.Namespace)
	}
}

func processConditions(existing []v1.Condition, generation int64, newConds ...*acv1.ConditionApplyConfiguration) []*acv1.ConditionApplyConfiguration {
	for _, cond := range newConds {
		cond.WithObservedGeneration(generation)
		if prior, exists := conditionExists(existing, *cond.Type, cond.Status); !exists {
			cond.WithLastTransitionTime(v1.Now())
		} else {
			cond.WithLastTransitionTime(prior.LastTransitionTime)
		}
	}
	return newConds
}

// This is boilerplate we'd like to get rid of when istio 1.29 ships in Feb 26.
type NameKey struct {
	krt.Named
}

func (n NameKey) String() string {
	return fmt.Sprintf("%s/%s", n.Namespace, n.Name)
}

func NewNameKey(o v1.Object) NameKey {
	return NameKey{
		Named: krt.NewNamed(o),
	}
}

func getValidCondition() *acv1.ConditionApplyConfiguration {
	return acv1.Condition().WithType("Valid").WithStatus(v1.ConditionTrue).WithMessage("multicluster load balancer is valid.").WithReason("IsValid")
}

func getInvalidCondition(seName string) *acv1.ConditionApplyConfiguration {
	return acv1.Condition().WithType("Valid").WithStatus(v1.ConditionFalse).
		WithReason("PublicIPNotFound").WithMessage(
		fmt.Sprintf("No Public IP found for InternalServiceExport %s", seName))
}

func getDeployedCondition(numBackends int) *acv1.ConditionApplyConfiguration {
	return acv1.Condition().WithType("Deployed").WithStatus(v1.ConditionTrue).WithMessage(
		fmt.Sprintf("multicluster load balancer deployed successfully with %d backends.", numBackends)).WithReason("DeploymentSucceeded")
}

func getDeploFailedCondition() *acv1.ConditionApplyConfiguration {
	return acv1.Condition().WithType("Deployed").WithStatus(v1.ConditionFalse).
		WithReason("DeploymentFailed").WithMessage(
		"Failed to deploy global load balancer")
}
