@description('Name of the existing Front Door profile (deployed by endpoint.bicep).')
param profileName string

@description('Name of the existing Front Door endpoint within the profile.')
param endpointName string

@description('One entry per AFD Origin Group (one per HTTPRoute).')
param originGroups array = []
// { name: string }

@description('One entry per Origin. Flattened so each origin carries its parent origin-group name.')
param origins array = []
// { originGroupName: string, name: string, hostName: string, httpPort: int, httpsPort: int, weight: int, priority: int }

@description('One entry per AFD Rule Set (one per HTTPRoute).')
param ruleSets array = []
// { name: string }

@description('One entry per Rule. Flattened so each rule carries its parent rule-set name.')
param rules array = []
// { ruleSetName: string, name: string, order: int,
//   matchConditions: [{ kind, operator, selector, values: string[], negate: bool }],
//   actions: [{ kind, headerName, headerValue, headerOp, redirectScheme, redirectHostname, redirectPath,
//               redirectStatus, rewriteHostname, rewritePrefix }] }

@description('One entry per AFD Route (one per matched HTTPRoute hostname).')
param routes array = []
// { name: string, customDomainName: string, originGroupName: string, ruleSetName: string, patternsToMatch: string[] }

resource originGroups_res 'Microsoft.Cdn/profiles/originGroups@2024-02-01' = [
  for og in originGroups: {
    name: '${profileName}/${og.name}'
    properties: {
      loadBalancingSettings: {
        sampleSize: 4
        successfulSamplesRequired: 3
      }
      healthProbeSettings: {
        probePath: '/'
        probeRequestType: 'HEAD'
        probeProtocol: 'Http'
        probeIntervalInSeconds: 100
      }
      sessionAffinityState: 'Disabled'
    }
  }
]

resource origins_res 'Microsoft.Cdn/profiles/originGroups/origins@2024-02-01' = [
  for o in origins: {
    name: '${profileName}/${o.originGroupName}/${o.name}'
    properties: {
      hostName: o.hostName
      httpPort: o.httpPort
      httpsPort: o.httpsPort
      originHostHeader: o.hostName
      weight: o.weight
      priority: o.priority
      enabledState: 'Enabled'
    }
    dependsOn: [
      originGroups_res[indexOf(originGroups, first(filter(originGroups, og => og.name == o.originGroupName)))]
    ]
  }
]

resource ruleSets_res 'Microsoft.Cdn/profiles/ruleSets@2024-02-01' = [
  for rs in ruleSets: {
    name: '${profileName}/${rs.name}'
  }
]

resource rules_res 'Microsoft.Cdn/profiles/ruleSets/rules@2024-02-01' = [
  for r in rules: {
    name: '${profileName}/${r.ruleSetName}/${r.name}'
    properties: {
      order: r.order
      conditions: [
        for c in r.matchConditions: {
          name: c.kind
          parameters: {
            typeName: '${c.kind}MatchConditionParameters'
            operator: c.operator
            negateCondition: c.negate
            matchValues: c.values
            selector: c.selector
          }
        }
      ]
      actions: [
        for a in r.actions: {
          name: a.kind
          parameters: {
            typeName: '${a.kind}ActionParameters'
            headerAction: a.headerOp
            headerName: a.headerName
            value: a.headerValue
            destinationProtocol: a.redirectScheme
            customHostname: (a.redirectHostname ?? a.rewriteHostname)
            customPath: (a.redirectPath ?? a.rewritePrefix)
            redirectType: a.redirectStatus
          }
        }
      ]
      matchProcessingBehavior: 'Continue'
    }
    dependsOn: [
      ruleSets_res[indexOf(ruleSets, first(filter(ruleSets, rs => rs.name == r.ruleSetName)))]
    ]
  }
]

resource routes_res 'Microsoft.Cdn/profiles/afdEndpoints/routes@2024-02-01' = [
  for route in routes: {
    name: '${profileName}/${endpointName}/${route.name}'
    properties: {
      customDomains: [
        {
          id: resourceId('Microsoft.Cdn/profiles/customDomains', profileName, route.customDomainName)
        }
      ]
      originGroup: {
        id: resourceId('Microsoft.Cdn/profiles/originGroups', profileName, route.originGroupName)
      }
      ruleSets: [
        {
          id: resourceId('Microsoft.Cdn/profiles/ruleSets', profileName, route.ruleSetName)
        }
      ]
      supportedProtocols: [
        'Http'
        'Https'
      ]
      patternsToMatch: route.patternsToMatch
      forwardingProtocol: 'MatchRequest'
      linkToDefaultDomain: 'Disabled'
      httpsRedirect: 'Disabled'
    }
    dependsOn: [
      originGroups_res[indexOf(originGroups, first(filter(originGroups, og => og.name == route.originGroupName)))]
      ruleSets_res[indexOf(ruleSets, first(filter(ruleSets, rs => rs.name == route.ruleSetName)))]
    ]
  }
]
