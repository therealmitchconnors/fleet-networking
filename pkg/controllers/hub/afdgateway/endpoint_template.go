/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

// endpointTemplateInline is the ARM template compiled from endpoint.bicep via:
//
//	az bicep build --file endpoint.bicep --outfile endpoint.json
//
// It deploys the slow-changing, per-Gateway resources (Profile, AFD
// Endpoint, and one managed-TLS CustomDomain per listener hostname) as an
// ARM Deployment Stack. Regenerate it with the same command whenever
// endpoint.bicep changes; do not hand-edit the JSON below.
var endpointTemplateInline = `{
  "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "metadata": {
    "_generator": {
      "name": "bicep",
      "version": "0.37.4.10188",
      "templateHash": "17341680674329603369"
    }
  },
  "parameters": {
    "profileName": {
      "type": "string",
      "metadata": {
        "description": "Name of the Front Door profile (Microsoft.Cdn/profiles)."
      }
    },
    "endpointName": {
      "type": "string",
      "metadata": {
        "description": "Name of the Front Door endpoint within the profile."
      }
    },
    "sku": {
      "type": "string",
      "defaultValue": "Standard_AzureFrontDoor",
      "metadata": {
        "description": "Front Door SKU, e.g. Standard_AzureFrontDoor or Premium_AzureFrontDoor."
      }
    },
    "customDomainHostnames": {
      "type": "array",
      "defaultValue": [],
      "metadata": {
        "description": "Custom domain hostnames (from Gateway listener hostnames) to bind to the endpoint, using AFD-managed TLS certificates."
      }
    }
  },
  "resources": [
    {
      "type": "Microsoft.Cdn/profiles",
      "apiVersion": "2024-02-01",
      "name": "[parameters('profileName')]",
      "location": "Global",
      "sku": {
        "name": "[parameters('sku')]"
      }
    },
    {
      "type": "Microsoft.Cdn/profiles/afdEndpoints",
      "apiVersion": "2024-02-01",
      "name": "[format('{0}/{1}', parameters('profileName'), parameters('endpointName'))]",
      "location": "Global",
      "properties": {
        "enabledState": "Enabled"
      },
      "dependsOn": [
        "[resourceId('Microsoft.Cdn/profiles', parameters('profileName'))]"
      ]
    },
    {
      "copy": {
        "name": "customDomains",
        "count": "[length(parameters('customDomainHostnames'))]"
      },
      "type": "Microsoft.Cdn/profiles/customDomains",
      "apiVersion": "2024-02-01",
      "name": "[format('{0}/{1}', parameters('profileName'), replace(replace(parameters('customDomainHostnames')[copyIndex()], '.', '-'), '*', 'wildcard'))]",
      "properties": {
        "hostName": "[parameters('customDomainHostnames')[copyIndex()]]",
        "tlsSettings": {
          "certificateType": "ManagedCertificate",
          "minimumTlsVersion": "TLS12"
        }
      },
      "dependsOn": [
        "[resourceId('Microsoft.Cdn/profiles', parameters('profileName'))]"
      ]
    }
  ],
  "outputs": {
    "endpointHostName": {
      "type": "string",
      "value": "[reference(resourceId('Microsoft.Cdn/profiles/afdEndpoints', parameters('profileName'), parameters('endpointName')), '2024-02-01').hostName]"
    },
    "profileResourceId": {
      "type": "string",
      "value": "[resourceId('Microsoft.Cdn/profiles', parameters('profileName'))]"
    },
    "endpointResourceId": {
      "type": "string",
      "value": "[resourceId('Microsoft.Cdn/profiles/afdEndpoints', parameters('profileName'), parameters('endpointName'))]"
    }
  }
}`
