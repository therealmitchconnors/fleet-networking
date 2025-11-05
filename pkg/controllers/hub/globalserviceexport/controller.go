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

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"go.goms.io/fleet-networking/api/v1alpha1"
	"go.goms.io/fleet-networking/api/v1beta1"
	"go.goms.io/fleet-networking/pkg/common/krtutil"
	"go.goms.io/fleet-networking/pkg/common/objectmeta"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/ptr"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

const globalAnnotation = "globalLBName"
const targetGatewayAnnotation = "targetGateway"

type parameters struct {
	name          string
	rg            string
	backends      []string
	targetGateway string
	serviceName   string
	namespace     string
}

// ResourceName implements krt.ResourceNamer.
func (p parameters) ResourceName() string {
	return fmt.Sprintf("%s.%s", p.rg, p.name)
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

var _ krt.ResourceNamer = parameters{}
var _ krt.ResourceNamer = output{}

type Reconciler struct {
	client kube.Client

	resourceGroupName string // default resource group name to create public IP address
	deploymentClient  *armresources.DeploymentsClient
	resourceClient    *armresources.Client
}

func NewReconciler(c kube.Client, dc *armresources.DeploymentsClient, rc *armresources.Client, defaultRG string) *Reconciler {
	ob := krtutil.NewKrtOptions(make(chan struct{}), new(krt.DebugHandler))

	ses := krt.WrapClient(kclient.New[*v1beta1.ServiceExport](c), ob.ToOptions("serviceexports")...)
	ises := krt.WrapClient(kclient.New[*v1alpha1.InternalServiceExport](c), ob.ToOptions("internalserviceexports")...)
	s, _ := v1beta1.SchemeBuilder.Build()
	v1alpha1.SchemeBuilder.AddToScheme(s)
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

	params := krt.NewCollection(ses, func(kctx krt.HandlerContext, export *v1beta1.ServiceExport) *parameters {
		name, ok := export.Annotations[globalAnnotation]
		if !ok {
			return nil
		}
		targetGateway := export.Annotations[targetGatewayAnnotation]
		internalServiceExports := krt.Fetch(kctx, ises, krt.FilterIndex(iseIndex, NewNameKey(export)))
		rg := strings.TrimSpace(export.Annotations[objectmeta.ServiceAnnotationLoadBalancerResourceGroup])
		if len(rg) < 1 {
			rg = r.resourceGroupName
		}
		out := &parameters{
			name:          name,
			rg:            rg,
			targetGateway: targetGateway,
			serviceName:   export.Name,
			namespace:     export.Namespace,
		}
		for _, ise := range internalServiceExports {
			// for each public ip, get frontend config id
			pip, err := r.resourceClient.GetByID(context.Background(), *ise.Spec.PublicIPResourceID, "2024-10-01", &armresources.ClientGetByIDOptions{})
			if err != nil {
				klog.ErrorS(err, "No Public IP found for InternalServiceExport", "name", ise.Spec.ServiceReference.NamespacedName)
				kctx.DiscardResult()
				return nil
			}

			ipConfig := pip.Properties.(map[string]interface{})["ipConfiguration"].(map[string]interface{})
			cfgID, _ := ipConfig["id"].(string)
			out.backends = append(out.backends, cfgID)
		}
		return out
	})
	outputs := krt.NewCollection(params, func(kctx krt.HandlerContext, param parameters) *output {
		ipAddress, err := r.writeDeployment(param)
		if err != nil {
			klog.ErrorS(err, "Failed to deploy global service", "name", param.name)
			kctx.DiscardResult()
		}
		return &output{
			publicGlobalIPAddress: ipAddress,
			targetGateway:         param.targetGateway,
			serviceName:           param.serviceName,
			namespace:             param.namespace,
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

func (r *Reconciler) writeDeployment(params parameters) (string, error) {
	template := make(map[string]interface{})
	if err := json.Unmarshal([]byte(templateInline), &template); err != nil {
		return "", err
	}
	p, err := r.deploymentClient.BeginCreateOrUpdate(context.Background(), params.rg, params.name, armresources.Deployment{
		Properties: &armresources.DeploymentProperties{
			Template:   template,
			Parameters: params,
			Mode:       ptr.Of(armresources.DeploymentModeIncremental),
		},
	}, nil)
	if err != nil {
		return "", err
	}
	res, err := p.PollUntilDone(context.Background(), nil)
	log.Default().Println("Deployment result: ", res)
	outputs := res.DeploymentExtended.Properties.Outputs.(map[string]interface{})
	ipAddress := outputs["publicGlobalIPAddress"].(map[string]interface{})["value"].(string)
	return ipAddress, err
}

// This is boilerplate we'd like to get rid of.
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
