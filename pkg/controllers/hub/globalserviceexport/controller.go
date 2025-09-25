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
	"strings"

	"go.goms.io/fleet-networking/pkg/common/krtutil"

	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"go.goms.io/fleet-networking/api/v1alpha1"
	"go.goms.io/fleet-networking/api/v1beta1"
	"go.goms.io/fleet-networking/pkg/common/objectmeta"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/ptr"
	"k8s.io/klog/v2"
)

const globalAnnotation = "globalLBName"

type parameters struct {
	name     string
	rg       string
	backends []string
}

type Reconciler struct {
	client  kube.Client
	exports krt.Collection[*v1beta1.ServiceExport]
	// cww     client.WithWatch

	resourceGroupName string // default resource group name to create public IP address
	// LBClient          loadbalancerclient.Interface
	deploymentClient *armresources.DeploymentsClient
	resourceClient   *armresources.Client
}

func NewReconciler(c kube.Client, dc *armresources.DeploymentsClient, rc *armresources.Client, defaultRG string) *Reconciler {
	filter := kclient.Filter{}

	seInf := kclient.NewDelayedInformer[*v1beta1.ServiceExport](c, v1beta1.GroupVersion.WithResource("ServiceExport"), kubetypes.StandardInformer, filter)
	iseInf := kclient.NewDelayedInformer[*v1alpha1.InternalServiceExport](c, v1beta1.GroupVersion.WithResource("InternalServiceExport"), kubetypes.StandardInformer, filter)
	ob := krtutil.NewKrtOptions(make(chan struct{}), new(krt.DebugHandler))

	ses := krt.WrapClient(seInf, ob.ToOptions("serviceexports")...)
	ises := krt.WrapClient(iseInf, ob.ToOptions("internalserviceexports")...)
	s, _ := v1beta1.SchemeBuilder.Build()
	v1alpha1.SchemeBuilder.AddToScheme(s)
	r := &Reconciler{
		client:            c,
		exports:           ses,
		resourceClient:    rc,
		deploymentClient:  dc,
		resourceGroupName: defaultRG,
	}

	iseIndex := krt.NewIndex(ises, "internal service export by service reference name", func(export *v1alpha1.InternalServiceExport) []krt.Named {
		return []krt.Named{{
			Name:      export.Spec.ServiceReference.Name,
			Namespace: export.Spec.ServiceReference.Namespace,
		}}
	})

	params := krt.NewCollection(ses, func(kctx krt.HandlerContext, export *v1beta1.ServiceExport) *parameters {
		name, ok := export.Annotations[globalAnnotation]
		if !ok {
			return nil
		}
		internalServiceExports := krt.Fetch(kctx, ises, krt.FilterIndex(iseIndex, krt.NewNamed(export)))
		rg := strings.TrimSpace(export.Annotations[objectmeta.ServiceAnnotationLoadBalancerResourceGroup])
		if len(rg) < 1 {
			rg = r.resourceGroupName
		}
		out := &parameters{
			name: name,
			rg:   rg,
		}
		for _, ise := range internalServiceExports {
			// for each public ip, get frontend config id
			pip, err := r.resourceClient.GetByID(context.Background(), *ise.Spec.PublicIPResourceID, "", &armresources.ClientGetByIDOptions{})
			if err != nil {
				klog.ErrorS(err, "No Public IP found for InternalServiceExport", "name", ise.Spec.ServiceReference.NamespacedName)
				kctx.DiscardResult()
				return nil
			}
			props := pip.Properties.(armnetwork.PublicIPAddressPropertiesFormat)
			out.backends = append(out.backends, ptr.OrEmpty(props.IPConfiguration.ID))
		}
		return out
	})
	krt.NewCollection(params, func(kctx krt.HandlerContext, param parameters) *string {
		err := r.writeDeployment(param)
		if err != nil {
			klog.ErrorS(err, "Failed to deploy global service", "name", param.name)
			kctx.DiscardResult()
		}
		return nil
	})
	return r
}

func (r *Reconciler) writeDeployment(params parameters) error {
	p, err := r.deploymentClient.BeginCreateOrUpdate(context.Background(), params.rg, params.name, armresources.Deployment{
		Properties: &armresources.DeploymentProperties{
			Template:   template,
			Parameters: params,
		},
	}, nil)
	if err != nil {
		return err
	}
	_, err = p.PollUntilDone(context.Background(), nil)
	return err
}

// func buildGlobalLB(params parameters) *armnetwork.LoadBalancer {

// 	fip := &armnetwork.FrontendIPConfiguration{
// 		Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
// 			PublicIPAddress: &armnetwork.PublicIPAddress{
// 				SKU: &armnetwork.PublicIPAddressSKU{
// 					Name: ptr.Of(armnetwork.PublicIPAddressSKUNameStandard),
// 					Tier: ptr.Of(armnetwork.PublicIPAddressSKUTierGlobal),
// 				},
// 			},
// 		},
// 	}

// 	bp := &armnetwork.BackendAddressPool{
// 		Name: ptr.Of(params.name),
// 		Properties: &armnetwork.BackendAddressPoolPropertiesFormat{

// 		},
// 	}
// 	for _, id := range params.ids {
// 		bp.Properties.LoadBalancerBackendAddresses = append(bp.Properties.LoadBalancerBackendAddresses, &armnetwork.LoadBalancerBackendAddress{
// 			Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
// 				LoadBalancerFrontendIPConfiguration: &armnetwork.SubResource{
// 					ID: ptr.Of(id),
// 				},
// 			},
// 		})
// 	}

// 	out := &armnetwork.LoadBalancer{
// 		SKU: &armnetwork.LoadBalancerSKU{
// 			Name: ptr.Of(armnetwork.LoadBalancerSKUNameStandard),
// 			Tier: ptr.Of(armnetwork.LoadBalancerSKUTierGlobal),
// 		},
// 		Properties: &armnetwork.LoadBalancerPropertiesFormat{
// 			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
// 				fip,
// 			},
// 			BackendAddressPools: []*armnetwork.BackendAddressPool{
// 				bp,
// 			},
// 			LoadBalancingRules: []*armnetwork.LoadBalancingRule{
// 				{
// 					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
// 						FrontendIPConfiguration: &armnetwork.SubResource{
// 							ID: fip.ID,
// 						},
// 						FrontendPort:     ptr.Of(int32(80)),
// 						BackendPort:      ptr.Of(int32(80)),
// 						EnableFloatingIP: ptr.Of(true),
// 						Protocol:         ptr.Of(armnetwork.TransportProtocolTCP),
// 						BackendAddressPool: &armnetwork.SubResource{
// 							ID: bp.ID,
// 						},
// 					},
// 				},
// 			},
// 		},
// 	}
// 	return out
// }

// func (r *Reconciler) writeGlobalLB(lb *armnetwork.LoadBalancer, rg string) error {
// 	_, err := r.LBClient.CreateOrUpdate(context.Background(), rg, *lb.Name, *lb)
// 	return err
// }
