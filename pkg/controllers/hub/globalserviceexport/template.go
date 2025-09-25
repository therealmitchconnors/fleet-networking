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

var template = `
param name string = 'foo'
param backends array = []

var loadBalancers_mitchs_global_lb_name = '${name}-global-lb'
var publicIPAddresses_mitchs_global_ip_name = '${name}-global-ip'

var backendObjs = [for backend in backends: {
  name: last(split(backend, '/'))
  properties: {
    loadBalancerFrontendIPConfiguration: {
      id: backend
    }
  }
}]

resource publicIPAddresses_mitchs_global_ip_name_resource 'Microsoft.Network/publicIPAddresses@2024-07-01' = {
  name: publicIPAddresses_mitchs_global_ip_name
  location: 'eastus2'
  sku: {
    name: 'Standard'
    tier: 'Global'
  }
  properties: {
    publicIPAddressVersion: 'IPv4'
    publicIPAllocationMethod: 'Static'
  }
}

resource loadBalancers_mitchs_global_lb_name_resource 'Microsoft.Network/loadBalancers@2024-07-01' = {
  name: loadBalancers_mitchs_global_lb_name
  location: 'eastus2'
  sku: {
    name: 'Standard'
    tier: 'Global'
  }
  properties: {
    frontendIPConfigurations: [
      {
        name: 'mitchs-global-ip-config'
        properties: {
          publicIPAddress: {
            id: publicIPAddresses_mitchs_global_ip_name_resource.id
          }
        }
      }
    ]
    backendAddressPools: [
      {
        name: 'kubernetes-mc'
      }
    ]
    loadBalancingRules: [
      {
        name: 'tcp-80-k8s2'
        properties: {
          frontendIPConfiguration: {
            id: resourceId('Microsoft.Network/loadBalancers/frontendIPConfigurations', loadBalancers_mitchs_global_lb_name, 'mitchs-global-ip-config')
          }
          frontendPort: 80
          backendPort: 80
          enableFloatingIP: true
          idleTimeoutInMinutes: 4
          protocol: 'Tcp'
          backendAddressPools: [
            {
              id: resourceId('Microsoft.Network/loadBalancers/backendAddressPools', loadBalancers_mitchs_global_lb_name, 'kubernetes-mc')
            }
          ]
        }
      }
    ]
  }

  resource mybp 'backendAddressPools@2024-07-01' = {
    name: 'kubernetes-mc'
      properties: {
        loadBalancerBackendAddresses: backendObjs
      }
    }
}
`
