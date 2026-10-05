/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

// routingTemplateInline is the ARM template compiled from routing.bicep via:
//
//	az bicep build --file routing.bicep --outfile routing.json
//
// It deploys the faster-changing, per-Gateway routing resources (Origin
// Groups, Origins, Rule Sets, Rules, and Routes derived from the Gateway's
// HTTPRoutes) as a separate ARM Deployment Stack from endpointTemplateInline,
// so HTTPRoute churn doesn't force redeploying Profile/Endpoint/CustomDomain
// state. Regenerate it with the same command whenever routing.bicep changes;
// do not hand-edit the JSON below.
var routingTemplateInline = `{
  "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "metadata": {
    "_generator": {
      "name": "bicep",
      "version": "0.37.4.10188",
      "templateHash": "4413557979345629084"
    }
  },
  "parameters": {
    "profileName": {
      "type": "string",
      "metadata": {
        "description": "Name of the existing Front Door profile (deployed by endpoint.bicep)."
      }
    },
    "endpointName": {
      "type": "string",
      "metadata": {
        "description": "Name of the existing Front Door endpoint within the profile."
      }
    },
    "originGroups": {
      "type": "array",
      "defaultValue": [],
      "metadata": {
        "description": "One entry per AFD Origin Group (one per HTTPRoute)."
      }
    },
    "origins": {
      "type": "array",
      "defaultValue": [],
      "metadata": {
        "description": "One entry per Origin. Flattened so each origin carries its parent origin-group name."
      }
    },
    "ruleSets": {
      "type": "array",
      "defaultValue": [],
      "metadata": {
        "description": "One entry per AFD Rule Set (one per HTTPRoute)."
      }
    },
    "rules": {
      "type": "array",
      "defaultValue": [],
      "metadata": {
        "description": "One entry per Rule. Flattened so each rule carries its parent rule-set name."
      }
    },
    "routes": {
      "type": "array",
      "defaultValue": [],
      "metadata": {
        "description": "One entry per AFD Route (one per matched HTTPRoute hostname)."
      }
    }
  },
  "resources": [
    {
      "copy": {
        "name": "originGroups_res",
        "count": "[length(parameters('originGroups'))]"
      },
      "type": "Microsoft.Cdn/profiles/originGroups",
      "apiVersion": "2024-02-01",
      "name": "[format('{0}/{1}', parameters('profileName'), parameters('originGroups')[copyIndex()].name)]",
      "properties": {
        "loadBalancingSettings": {
          "sampleSize": 4,
          "successfulSamplesRequired": 3
        },
        "healthProbeSettings": {
          "probePath": "/",
          "probeRequestType": "HEAD",
          "probeProtocol": "Http",
          "probeIntervalInSeconds": 100
        },
        "sessionAffinityState": "Disabled"
      }
    },
    {
      "copy": {
        "name": "origins_res",
        "count": "[length(parameters('origins'))]"
      },
      "type": "Microsoft.Cdn/profiles/originGroups/origins",
      "apiVersion": "2024-02-01",
      "name": "[format('{0}/{1}/{2}', parameters('profileName'), parameters('origins')[copyIndex()].originGroupName, parameters('origins')[copyIndex()].name)]",
      "properties": {
        "hostName": "[parameters('origins')[copyIndex()].hostName]",
        "httpPort": "[parameters('origins')[copyIndex()].httpPort]",
        "httpsPort": "[parameters('origins')[copyIndex()].httpsPort]",
        "originHostHeader": "[parameters('origins')[copyIndex()].hostName]",
        "weight": "[parameters('origins')[copyIndex()].weight]",
        "priority": "[parameters('origins')[copyIndex()].priority]",
        "enabledState": "Enabled"
      },
      "dependsOn": [
        "[resourceId('Microsoft.Cdn/profiles/originGroups', split(format('{0}/{1}', parameters('profileName'), parameters('originGroups')[indexOf(parameters('originGroups'), first(filter(parameters('originGroups'), lambda('og', equals(lambdaVariables('og').name, parameters('origins')[copyIndex()].originGroupName)))))].name), '/')[0], split(format('{0}/{1}', parameters('profileName'), parameters('originGroups')[indexOf(parameters('originGroups'), first(filter(parameters('originGroups'), lambda('og', equals(lambdaVariables('og').name, parameters('origins')[copyIndex()].originGroupName)))))].name), '/')[1])]"
      ]
    },
    {
      "copy": {
        "name": "ruleSets_res",
        "count": "[length(parameters('ruleSets'))]"
      },
      "type": "Microsoft.Cdn/profiles/ruleSets",
      "apiVersion": "2024-02-01",
      "name": "[format('{0}/{1}', parameters('profileName'), parameters('ruleSets')[copyIndex()].name)]"
    },
    {
      "copy": {
        "name": "rules_res",
        "count": "[length(parameters('rules'))]"
      },
      "type": "Microsoft.Cdn/profiles/ruleSets/rules",
      "apiVersion": "2024-02-01",
      "name": "[format('{0}/{1}/{2}', parameters('profileName'), parameters('rules')[copyIndex()].ruleSetName, parameters('rules')[copyIndex()].name)]",
      "properties": {
        "copy": [
          {
            "name": "conditions",
            "count": "[length(parameters('rules')[copyIndex()].matchConditions)]",
            "input": {
              "name": "[parameters('rules')[copyIndex()].matchConditions[copyIndex('conditions')].kind]",
              "parameters": {
                "typeName": "[format('{0}MatchConditionParameters', parameters('rules')[copyIndex()].matchConditions[copyIndex('conditions')].kind)]",
                "operator": "[parameters('rules')[copyIndex()].matchConditions[copyIndex('conditions')].operator]",
                "negateCondition": "[parameters('rules')[copyIndex()].matchConditions[copyIndex('conditions')].negate]",
                "matchValues": "[parameters('rules')[copyIndex()].matchConditions[copyIndex('conditions')].values]",
                "selector": "[parameters('rules')[copyIndex()].matchConditions[copyIndex('conditions')].selector]"
              }
            }
          },
          {
            "name": "actions",
            "count": "[length(parameters('rules')[copyIndex()].actions)]",
            "input": {
              "name": "[parameters('rules')[copyIndex()].actions[copyIndex('actions')].kind]",
              "parameters": {
                "typeName": "[format('{0}ActionParameters', parameters('rules')[copyIndex()].actions[copyIndex('actions')].kind)]",
                "headerAction": "[parameters('rules')[copyIndex()].actions[copyIndex('actions')].headerOp]",
                "headerName": "[parameters('rules')[copyIndex()].actions[copyIndex('actions')].headerName]",
                "value": "[parameters('rules')[copyIndex()].actions[copyIndex('actions')].headerValue]",
                "destinationProtocol": "[parameters('rules')[copyIndex()].actions[copyIndex('actions')].redirectScheme]",
                "customHostname": "[coalesce(parameters('rules')[copyIndex()].actions[copyIndex('actions')].redirectHostname, parameters('rules')[copyIndex()].actions[copyIndex('actions')].rewriteHostname)]",
                "customPath": "[coalesce(parameters('rules')[copyIndex()].actions[copyIndex('actions')].redirectPath, parameters('rules')[copyIndex()].actions[copyIndex('actions')].rewritePrefix)]",
                "redirectType": "[parameters('rules')[copyIndex()].actions[copyIndex('actions')].redirectStatus]"
              }
            }
          }
        ],
        "order": "[parameters('rules')[copyIndex()].order]",
        "matchProcessingBehavior": "Continue"
      },
      "dependsOn": [
        "[resourceId('Microsoft.Cdn/profiles/ruleSets', split(format('{0}/{1}', parameters('profileName'), parameters('ruleSets')[indexOf(parameters('ruleSets'), first(filter(parameters('ruleSets'), lambda('rs', equals(lambdaVariables('rs').name, parameters('rules')[copyIndex()].ruleSetName)))))].name), '/')[0], split(format('{0}/{1}', parameters('profileName'), parameters('ruleSets')[indexOf(parameters('ruleSets'), first(filter(parameters('ruleSets'), lambda('rs', equals(lambdaVariables('rs').name, parameters('rules')[copyIndex()].ruleSetName)))))].name), '/')[1])]"
      ]
    },
    {
      "copy": {
        "name": "routes_res",
        "count": "[length(parameters('routes'))]"
      },
      "type": "Microsoft.Cdn/profiles/afdEndpoints/routes",
      "apiVersion": "2024-02-01",
      "name": "[format('{0}/{1}/{2}', parameters('profileName'), parameters('endpointName'), parameters('routes')[copyIndex()].name)]",
      "properties": {
        "customDomains": [
          {
            "id": "[resourceId('Microsoft.Cdn/profiles/customDomains', parameters('profileName'), parameters('routes')[copyIndex()].customDomainName)]"
          }
        ],
        "originGroup": {
          "id": "[resourceId('Microsoft.Cdn/profiles/originGroups', parameters('profileName'), parameters('routes')[copyIndex()].originGroupName)]"
        },
        "ruleSets": [
          {
            "id": "[resourceId('Microsoft.Cdn/profiles/ruleSets', parameters('profileName'), parameters('routes')[copyIndex()].ruleSetName)]"
          }
        ],
        "supportedProtocols": [
          "Http",
          "Https"
        ],
        "patternsToMatch": "[parameters('routes')[copyIndex()].patternsToMatch]",
        "forwardingProtocol": "MatchRequest",
        "linkToDefaultDomain": "Disabled",
        "httpsRedirect": "Disabled"
      },
      "dependsOn": [
        "[resourceId('Microsoft.Cdn/profiles/originGroups', split(format('{0}/{1}', parameters('profileName'), parameters('originGroups')[indexOf(parameters('originGroups'), first(filter(parameters('originGroups'), lambda('og', equals(lambdaVariables('og').name, parameters('routes')[copyIndex()].originGroupName)))))].name), '/')[0], split(format('{0}/{1}', parameters('profileName'), parameters('originGroups')[indexOf(parameters('originGroups'), first(filter(parameters('originGroups'), lambda('og', equals(lambdaVariables('og').name, parameters('routes')[copyIndex()].originGroupName)))))].name), '/')[1])]",
        "[resourceId('Microsoft.Cdn/profiles/ruleSets', split(format('{0}/{1}', parameters('profileName'), parameters('ruleSets')[indexOf(parameters('ruleSets'), first(filter(parameters('ruleSets'), lambda('rs', equals(lambdaVariables('rs').name, parameters('routes')[copyIndex()].ruleSetName)))))].name), '/')[0], split(format('{0}/{1}', parameters('profileName'), parameters('ruleSets')[indexOf(parameters('ruleSets'), first(filter(parameters('ruleSets'), lambda('rs', equals(lambdaVariables('rs').name, parameters('routes')[copyIndex()].ruleSetName)))))].name), '/')[1])]"
      ]
    }
  ]
}`
