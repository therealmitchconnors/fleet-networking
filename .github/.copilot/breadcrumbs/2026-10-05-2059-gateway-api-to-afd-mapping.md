# Gateway API to Azure Front Door Mapping

## Requirements

1. Compare the reference docs for Kubernetes Gateway API (v1.6, Standard channel) and
   Azure Front Door (Standard/Premium) and produce a table showing how each Gateway API
   feature/concept is represented (or not) in AFD configuration.
2. For each Standard-channel Gateway API CRD (no experimental-channel resources), produce a
   Go code snippet showing how Istio's `krt` library (`istio.io/istio/pkg/kube/krt`, as
   already used in `pkg/controllers/hub/globalserviceexport`) could be used to write a
   controller that translates Gateway API intent into AFD configuration (via the Azure SDK,
   e.g. `armcdn`/Front Door management plane types).
3. Standard channel CRDs in v1.6: GatewayClass, Gateway, HTTPRoute, GRPCRoute, TCPRoute,
   UDPRoute, ReferenceGrant, BackendTLSPolicy.

## Additional comments from user

- "ok, let's turn this into a new controller. But skip GRPCRoute, TCPRoute, UDPRoute, and
  BackendTLS." -> scope narrowed to GatewayClass, Gateway, HTTPRoute, ReferenceGrant only.
- "how do we go from structs like AFDEndpoint to actual AFD config? won't the Azure SDK
  mean constantly diffing every field? ARM/Bicep? Terraform? other options?" -> led to the
  apply-strategy decision below (ARM Deployment Stacks, split per Gateway).

## Plan

### Phase 2: real controller implementation (this phase)

1. Confirm Gateway API Standard types (GatewayClass, Gateway, HTTPRoute v1;
   ReferenceGrant v1beta1) are natively supported by istio's `kclient.New[T]` (they are;
   no `apiclient.RegisterTypes()` work needed).
2. Decide the Azure apply strategy: raw armcdn SDK CRUD vs. ARM Deployment Stacks vs.
   external IaC. **Decision: ARM Deployment Stacks**, split into two stacks per Gateway
   (endpoint stack: Profile/AFDEndpoint/CustomDomains; routing stack: OriginGroups/
   Origins/RuleSets/Rules/Routes) so HTTPRoute churn never forces redeploying slow-changing
   cert/domain state. Mirrors `pkg/controllers/hub/globalserviceexport`'s existing pattern
   (same `*armdeploymentstacks.Client`, reused as-is).
3. Author the two ARM templates as real `.bicep` sources compiled via `az bicep build`
   (type-checked against the real Microsoft.Cdn schema) rather than hand-written JSON, then
   embed the compiled JSON as Go string constants (`endpoint_template.go`,
   `routing_template.go`), matching `globalserviceexport/template.go`'s `templateInline`
   convention.
4. Write the Go package `pkg/controllers/hub/afdgateway/`:
   - `types.go`: shared data model (AFDGatewayClass, AFDEndpoint, CustomDomainSpec,
     AFDRoute, WeightedOrigin, MatchCondition, RuleAction, AFDRule) + AFD-name sanitizing.
   - `gatewayclass.go`: filters GatewayClass by `spec.controllerName`.
   - `gateway.go`: Gateway -> AFDEndpoint (resource group/SKU from annotations, listener
     hostnames -> CustomDomainSpec using AFD-managed TLS certs).
   - `referencegrant.go`: ReferenceGrant -> flattened permitted (from,to) edges + krt.Index,
     used to authorize cross-namespace HTTPRoute backendRefs.
   - `httproute.go`: pure, unit-tested translation of HTTPRouteMatch -> AFD Rule Set match
     conditions, HTTPRouteFilter -> AFD Rule actions, plus the krt.NewManyCollection that
     resolves backendRefs (Service must be type LoadBalancer; ClusterIP unsupported without
     Private Link) into AFDRoute per matched Gateway listener hostname.
   - `aggregate.go`: joins AFDEndpoint with its AFDRoutes (by GatewayKey) into the two
     deployment-unit collections (EndpointDeployment, RoutingDeployment) actually applied.
   - `controller.go`: NewReconciler wiring, async workqueue + worker pool per stack type
     (decoupling blocking ARM calls from krt's reactive transforms, same pattern as
     globalserviceexport), writeStack/deleteStack, and Gateway/HTTPRoute status updates via
     Get + merge conditions + UpdateStatus + retry.RetryOnConflict (no generated
     applyconfigurations exist for upstream Gateway API types, unlike fleet's own CRDs).
5. Wire `afdgateway.NewReconciler` into `cmd/hub-net-controller-manager/main.go`, reusing
   the existing `dc *armdeploymentstacks.Client` and `cloudConfig.ResourceGroup`.
6. Make `sigs.k8s.io/gateway-api` a direct `go.mod` dependency (`go mod tidy`).
7. Unit tests for the pure translation functions (`toMatchConditions`, `toRuleActions`,
   `hostnameMatches`, `customDomainResourceName`).
8. Validate: `go build ./...`, `go vet ./...`, `go test ./pkg/controllers/hub/afdgateway/...`,
   `gofmt -l/-w`.

## Decisions

- Snippets are illustrative/pedagogical (not wired into a buildable package), following the
  style of the existing `globalserviceexport` controller (krt.WrapClient, krt.NewCollection,
  krt.NewManyCollection, krt.Fetch, krt.Index, ResourceNamer) so they are idiomatic for this
  repo once real AFD API types/wiring are added.
- TCPRoute/UDPRoute are included per the Standard-CRD requirement but documented as
  structurally unsupported by AFD (L7 HTTP(S)/anycast edge service only).
- **Controller scope** (phase 2): GatewayClass, Gateway, HTTPRoute, ReferenceGrant only.
  GRPCRoute, TCPRoute, UDPRoute, BackendTLSPolicy explicitly out of scope per user request.
- **Apply strategy**: ARM Deployment Stacks (not raw armcdn SDK CRUD, not external IaC).
  Rationale: ARM `PUT` is already declarative (no manual per-field diffing needed either
  way); the real gap raw SDK CRUD doesn't solve is *deletion/pruning* of orphaned child
  resources (e.g. a removed HTTPRoute's AFD Route/RuleSet/OriginGroup) -- Deployment Stacks'
  `ActionOnUnmanage.Resources = Delete` solves this automatically and matches the existing
  `globalserviceexport` pattern, so no new Azure client type or apply mechanism is
  introduced into the repo.
- **Two stacks per Gateway** (endpoint + routing), not one: isolates slow-changing
  Profile/Endpoint/CustomDomain/TLS-cert state from fast-changing, per-HTTPRoute routing
  state, so route churn never risks redeploying/recreating certs or the endpoint hostname.
- **No new CRD** for AFD-specific GatewayClass parameters: resource group and SKU are read
  from annotations on the Gateway object (`networking.fleet.azure.com/resource-group`,
  `networking.fleet.azure.com/sku`), consistent with the existing
  `objectmeta.ServiceAnnotationLoadBalancerResourceGroup` convention, avoiding new CRD
  scaffolding/deepcopy/CRD YAML for a `parametersRef`.
- **AFD-managed TLS certificates** (not customer Key Vault certs) for all custom domains in
  v1, to avoid complex cert/secret syncing; documented limitation, not solved here.
- **Known v1 simplification**: all `HTTPRouteRule.BackendRefs` across every rule of a route
  are merged into a single AFD Origin Group per matched hostname -- AFD cannot select a
  different origin per Rule Set rule without the more advanced `RouteConfigurationOverride`
  action, which this controller does not yet generate. Documented as a TODO, not solved.
- **Origin resolution**: BackendRefs must reference a `Service` of type `LoadBalancer`;
  its `status.loadBalancer.ingress[0].{hostname,ip}` becomes the AFD origin host. ClusterIP
  Services are rejected with a `MissingBackendRefs`-style HTTPRoute status message (AFD has
  no way to reach a private cluster IP without Azure Private Link, Premium SKU only, not
  implemented here).
- **Status updates**: Gateway API types have no generated `applyconfigurations` (unlike
  fleet's own CRDs), so status is written via Get + locally merge `metav1.Condition` list +
  `UpdateStatus()`, wrapped in `retry.RetryOnConflict`, matching typical upstream Gateway API
  controller conventions.
- **Bicep language workaround**: Bicep (0.37.4 in this sandbox) disallows a `for`-expression
  nested inside another `for`-expression/function argument (`BCP138`), even via an
  intermediate variable. Fixed by flattening origins/rules into independent top-level
  parameter arrays in Go (each entry carrying its parent's name, e.g.
  `{originGroupName, name, hostName, ...}`), so every ARM resource type loops over its own
  flat array with a single level of `for`, and parent/child resources are linked via
  `resourceId(...)`-based `dependsOn` (found via `filter()`/`first()`/`indexOf()` on the
  flat array) rather than symbolic `parent:` references.

## Implementation Details

See chat response (phase 1) for the full comparison table and illustrative per-CRD
snippets. Phase 2's real implementation lives in `pkg/controllers/hub/afdgateway/`:

- `types.go` -- shared data model + `sanitizeAFDName`/`customDomainResourceName` (the
  latter must exactly mirror `endpoint.bicep`'s hostname-to-resource-name transform, since
  `routing.bicep` looks custom domains up by that same name via `resourceId()`).
- `gatewayclass.go` / `gateway.go` / `referencegrant.go` / `httproute.go` -- krt
  collections translating each Gateway API type, per the Plan above.
- `aggregate.go` -- `EndpointDeployment` / `RoutingDeployment` join.
- `endpoint.bicep` + `endpoint_template.go` -- Profile/AFDEndpoint/CustomDomains (managed
  TLS certs), compiled via `az bicep build --file endpoint.bicep --outfile endpoint.json`.
- `routing.bicep` + `routing_template.go` -- OriginGroups/Origins/RuleSets/Rules/Routes,
  using the flattened-parameter-array pattern described above; compiled the same way.
- `controller.go` -- `NewReconciler`, dual workqueues/worker pools, `writeStack`/
  `deleteStack` (ARM Deployment Stack create/update/delete with auto-prune), and
  Gateway `Programmed` / HTTPRoute `Accepted`/`ResolvedRefs` status condition updates.
- `httproute_test.go` -- unit tests for `toMatchConditions`, `toRuleActions`,
  `hostnameMatches`, `customDomainResourceName`.

Wired into `cmd/hub-net-controller-manager/main.go` via `afdgateway.NewReconciler(client,
dc, cloudConfig.ResourceGroup)`, reusing the existing `*armdeploymentstacks.Client`.

## Changes Made

- Phase 1: added this breadcrumb file only (research/design).
- Phase 2: new package `pkg/controllers/hub/afdgateway/` (types.go, gatewayclass.go,
  gateway.go, referencegrant.go, httproute.go, httproute_test.go, aggregate.go,
  controller.go, endpoint.bicep, endpoint_template.go, routing.bicep, routing_template.go);
  `cmd/hub-net-controller-manager/main.go` updated to construct and `mgr.Add` the new
  reconciler; `sigs.k8s.io/gateway-api` promoted from indirect to direct in `go.mod`
  (via `go mod tidy`).

## Before/After Comparison

- Before: no AFD-specific controller existed; `global-ingress-complete` only had the
  `globalserviceexport` global-load-balancer controller (Traffic Manager / regional LB
  wiring), unrelated to Gateway API or Front Door.
- After: a new, independently buildable/testable controller translates Gateway API
  GatewayClass/Gateway/HTTPRoute/ReferenceGrant intent into Azure Front Door configuration
  via two ARM Deployment Stacks per Gateway. `go build ./...`, `go vet ./...`, and
  `go test ./pkg/controllers/hub/afdgateway/...` all pass.

## References

- Gateway API v1.6 standard channel resources (GatewayClass, Gateway, HTTPRoute, GRPCRoute,
  TCPRoute, UDPRoute, ReferenceGrant, BackendTLSPolicy) - kubernetes-sigs/gateway-api.
- `sigs.k8s.io/gateway-api` v1.4.0 (this repo's pinned version) -- used for actual Go types
  (`apis/v1` for GatewayClass/Gateway/HTTPRoute, `apis/v1beta1` for ReferenceGrant).
- Azure Front Door Standard/Premium concepts (profile, endpoint, origin group, origin,
  route, rule set, WAF policy, custom domain) - learn.microsoft.com/azure/frontdoor.
- Istio krt library (`pkg/kube/krt`) - istio/istio, and this repo's existing usage in
  `pkg/controllers/hub/globalserviceexport/controller.go` and `pkg/common/krtutil`.
- `pkg/controllers/hub/globalserviceexport/controller.go` + `template.go` -- the direct
  style/pattern reference for krt wiring, async queue+worker pattern, and ARM Deployment
  Stack usage (`writeDeployment`, `ActionOnUnmanage.Resources = Delete`).
- Azure Bicep CLI v0.37.4 (`az bicep build`) -- used to author and type-check
  `endpoint.bicep`/`routing.bicep` against the real `Microsoft.Cdn` ARM schema
  (api-version `2024-02-01`) before embedding the compiled JSON.

