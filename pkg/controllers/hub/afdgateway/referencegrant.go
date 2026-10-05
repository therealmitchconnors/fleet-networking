/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"fmt"

	gatewayapiv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"

	"go.goms.io/fleet-networking/pkg/apiclient"
	"go.goms.io/fleet-networking/pkg/common/krtutil"
)

// referenceGrantEdge is one (from-namespace, from-kind, to-namespace,
// to-kind, to-name-or-wildcard) tuple permitted by a ReferenceGrant. A
// single ReferenceGrant typically expands into several edges (its
// spec.From and spec.To are both lists, combined pairwise).
type referenceGrantEdge struct {
	fromNamespace string
	fromKind      string
	toNamespace   string
	toKind        string
	// toName is the specific resource name this edge permits, or "" if the
	// ReferenceGrant permits all resources of toKind in toNamespace.
	toName string
}

// ResourceName implements krt.ResourceNamer. It is never looked up by name
// directly (only via the index below), but krt.Collection requires one.
func (e referenceGrantEdge) ResourceName() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", e.fromNamespace, e.fromKind, e.toNamespace, e.toKind, e.toName)
}

var _ krt.ResourceNamer = referenceGrantEdge{}

// referenceGrantIndexKey is what callers look up edges by: the (toNamespace,
// toKind, fromNamespace) a cross-namespace backendRef wants to use.
type referenceGrantIndexKey struct {
	toNamespace   string
	toKind        string
	fromNamespace string
}

func (k referenceGrantIndexKey) Key() string {
	return fmt.Sprintf("%s/%s/%s", k.toNamespace, k.toKind, k.fromNamespace)
}

// NewReferenceGrantEdges wraps the cluster's ReferenceGrant resources into a
// flat collection of permitted (from, to) edges, suitable for an
// isReferenceAllowed lookup via the returned krt.Index.
func NewReferenceGrantEdges(c apiclient.Client, ob krtutil.KrtOptions) (krt.Collection[referenceGrantEdge], krt.Index[string, referenceGrantEdge]) {
	grants := krt.WrapClient(kclient.New[*gatewayapiv1beta1.ReferenceGrant](c), ob.ToOptions("referencegrants")...)

	edges := krt.NewManyCollection(grants, func(_ krt.HandlerContext, grant *gatewayapiv1beta1.ReferenceGrant) []referenceGrantEdge {
		var out []referenceGrantEdge
		for _, from := range grant.Spec.From {
			for _, to := range grant.Spec.To {
				name := ""
				if to.Name != nil {
					name = string(*to.Name)
				}
				out = append(out, referenceGrantEdge{
					fromNamespace: string(from.Namespace),
					fromKind:      string(from.Kind),
					toNamespace:   grant.Namespace,
					toKind:        string(to.Kind),
					toName:        name,
				})
			}
		}
		return out
	}, ob.ToOptions("referencegrant-edges")...)

	idx := krt.NewIndex(edges, "by target", func(e referenceGrantEdge) []string {
		return []string{referenceGrantIndexKey{toNamespace: e.toNamespace, toKind: e.toKind, fromNamespace: e.fromNamespace}.Key()}
	})
	return edges, idx
}

// isReferenceAllowed reports whether a reference from a resource of kind
// fromKind in fromNamespace to a resource of kind toKind named toName in
// toNamespace is permitted, either because the namespaces match (no grant
// needed) or because a matching ReferenceGrant edge exists.
func isReferenceAllowed(kctx krt.HandlerContext, edges krt.Collection[referenceGrantEdge], idx krt.Index[string, referenceGrantEdge], fromNamespace, fromKind, toNamespace, toKind, toName string) bool {
	if fromNamespace == toNamespace {
		return true
	}
	key := referenceGrantIndexKey{toNamespace: toNamespace, toKind: toKind, fromNamespace: fromNamespace}.Key()
	for _, e := range krt.Fetch(kctx, edges, krt.FilterIndex(idx, key)) {
		if e.fromKind != fromKind {
			continue
		}
		if e.toName == "" || e.toName == toName {
			return true
		}
	}
	return false
}
