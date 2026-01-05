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
	// ses := krt.WrapClient(kclient.New[*v1beta1.ServiceExport](c), ob.ToOptions("serviceexports")...)
	ises := krt.WrapClient(kclient.New[*v1alpha1.InternalServiceExport](c), ob.ToOptions("internalserviceexports")...)
	// s, _ := v1beta1.SchemeBuilder.Build()
	// v1alpha1.SchemeBuilder.AddToScheme(s)
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
			Name:          mclb.Name, // todo: generate unique name
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
				r.ApplyConditions(mclb, acv1.Condition().WithType("Valid").WithStatus(v1.ConditionFalse).
					WithReason("PublicIPNotFound").WithMessage(
					fmt.Sprintf("No Public IP found for InternalServiceExport %s", ise.Spec.ServiceReference.NamespacedName)))
				kctx.DiscardResult()
				return nil
			}

			ipConfig := pip.Properties.(map[string]interface{})["ipConfiguration"].(map[string]interface{})
			cfgID, _ := ipConfig["id"].(string)
			out.Backends = append(out.Backends, cfgID)
		}
		r.ApplyConditions(mclb, acv1.Condition().WithType("Valid").WithStatus(v1.ConditionTrue))
		// Add finalizer if not present
		go r.ApplyFinalizer(mclb)

		ipAddress, err := r.writeDeployment(*out)
		if err != nil {
			klog.ErrorS(err, "Failed to deploy global service", "name", out.Name)
			r.ApplyConditions(mclb, acv1.Condition().WithType("Deployed").WithStatus(v1.ConditionFalse).
				WithReason("DeploymentFailed").WithMessage(
				fmt.Sprintf("Failed to deploy global service %s", out.Name)))
			kctx.DiscardResult()
		}
		r.ApplyConditions(mclb, acv1.Condition().WithType("Deployed").WithStatus(v1.ConditionTrue))
		return &output{
			publicGlobalIPAddress: ipAddress,
			targetGateway:         out.targetGateway,
			serviceName:           out.serviceName,
			namespace:             out.namespace,
		}
		// TODO: write to status
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
	_, err := r.client.Networking().ApiV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).Patch(
		context.Background(), mclb.Name, types.JSONPatchType, []byte("[ { \"op\": \"remove\", \"path\": \"/metadata/finalizers/-\", \"value\": \"mclb\" } ]"),
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
	_, err := r.client.Networking().ApiV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).Apply(
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

func (r *Reconciler) ApplyConditions(mclb *v1alpha1.MultiClusterLoadBalancer, conditions ...*acv1.ConditionApplyConfiguration) error {
	for _, cond := range conditions {
		cond.WithObservedGeneration(mclb.Generation)
	}
	_, err := r.client.Networking().ApiV1alpha1().MultiClusterLoadBalancers(mclb.Namespace).ApplyStatus(context.Background(),
		ac.MultiClusterLoadBalancer(mclb.Name, mclb.Namespace).WithStatus(
			ac.MultiClusterLoadBalancerStatus().WithConditions(conditions...),
		),
		v1.ApplyOptions{
			FieldManager: fieldManagerName,
		},
	)
	return err
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
