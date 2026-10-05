@description('Name of the Front Door profile (Microsoft.Cdn/profiles).')
param profileName string

@description('Name of the Front Door endpoint within the profile.')
param endpointName string

@description('Front Door SKU, e.g. Standard_AzureFrontDoor or Premium_AzureFrontDoor.')
param sku string = 'Standard_AzureFrontDoor'

@description('Custom domain hostnames (from Gateway listener hostnames) to bind to the endpoint, using AFD-managed TLS certificates.')
param customDomainHostnames array = []

resource profile 'Microsoft.Cdn/profiles@2024-02-01' = {
  name: profileName
  location: 'Global'
  sku: {
    name: sku
  }
}

resource endpoint 'Microsoft.Cdn/profiles/afdEndpoints@2024-02-01' = {
  parent: profile
  name: endpointName
  location: 'Global'
  properties: {
    enabledState: 'Enabled'
  }
}

resource customDomains 'Microsoft.Cdn/profiles/customDomains@2024-02-01' = [
  for hostname in customDomainHostnames: {
    parent: profile
    name: replace(replace(hostname, '.', '-'), '*', 'wildcard')
    properties: {
      hostName: hostname
      tlsSettings: {
        certificateType: 'ManagedCertificate'
        minimumTlsVersion: 'TLS12'
      }
    }
  }
]

output endpointHostName string = endpoint.properties.hostName
output profileResourceId string = profile.id
output endpointResourceId string = endpoint.id
