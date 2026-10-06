/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package afdgateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	"go.goms.io/fleet-networking/pkg/apiclient"
	"go.goms.io/fleet-networking/pkg/common/krtutil"
	"go.goms.io/fleet-networking/pkg/generated/clientset/internalclientset"
	fakenetworkingclientset "go.goms.io/fleet-networking/pkg/generated/clientset/internalclientset/fake"
)

const (
	testPollTimeout  = 5 * time.Second
	testPollInterval = 20 * time.Millisecond
)

var registerTypesOnce sync.Once

// testAPIClient implements apiclient.Client, backed by istio's fake
// kube.Client (for core/Gateway API resources) and the generated fake
// networking clientset (for fleet-networking CRDs, unused by this package
// but required to satisfy the interface). Mirrors
// pkg/controllers/hub/globalserviceexport/controller_test.go's helper of
// the same shape.
type testAPIClient struct {
	kube.Client
	networking internalclientset.Interface
}

func (c *testAPIClient) Core() kube.Client                       { return c.Client }
func (c *testAPIClient) Networking() internalclientset.Interface { return c.networking }

var _ apiclient.Client = (*testAPIClient)(nil)

// newTestAPIClient builds a fake apiclient.Client seeded with the given
// Kubernetes objects (core and/or Gateway API types).
func newTestAPIClient(t *testing.T, objs ...runtime.Object) *testAPIClient {
	t.Helper()
	registerTypesOnce.Do(apiclient.RegisterTypes)
	return &testAPIClient{
		Client:     kube.NewFakeClient(objs...),
		networking: fakenetworkingclientset.NewClientset(),
	}
}

// testKrtOptions returns krtutil.KrtOptions bound to a stop channel that is
// closed automatically via t.Cleanup, and the raw stop channel (krt options
// want a <-chan struct{}, some call sites below want to close()/select on
// the same channel directly).
func testKrtOptions(t *testing.T) (krtutil.KrtOptions, chan struct{}) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	return krtutil.NewKrtOptions(stop, new(krt.DebugHandler)), stop
}

// runClient starts the fake client's informers in the background; it
// mirrors how the real Reconciler.Start drives client.RunAndWait.
func runClient(c kube.Client, stop <-chan struct{}) {
	go c.RunAndWait(stop)
}

// waitForKey polls collection for key until present, failing the test after
// testPollTimeout.
func waitForKey[T any](t *testing.T, collection krt.Collection[T], key string) T {
	t.Helper()
	var got *T
	err := wait.PollUntilContextTimeout(context.Background(), testPollInterval, testPollTimeout, true, func(context.Context) (bool, error) {
		got = collection.GetKey(key)
		return got != nil, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for key %q in collection: %v", key, err)
	}
	return *got
}

// waitForNoKey polls collection and fails the test if key is ever present
// within a short grace period; used to assert something is filtered out
// rather than merely not-yet-synced.
func waitForNoKey[T any](t *testing.T, collection krt.Collection[T], key string) {
	t.Helper()
	time.Sleep(testPollInterval * 5)
	if got := collection.GetKey(key); got != nil {
		t.Fatalf("expected no entry for key %q, got %+v", key, got)
	}
}

// waitForListLen polls collection.List() until it has exactly n elements,
// failing the test after testPollTimeout.
func waitForListLen[T any](t *testing.T, collection krt.Collection[T], n int) []T {
	t.Helper()
	var got []T
	err := wait.PollUntilContextTimeout(context.Background(), testPollInterval, testPollTimeout, true, func(context.Context) (bool, error) {
		got = collection.List()
		return len(got) == n, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for collection to have %d elements, got %d: %v", n, len(got), err)
	}
	return got
}
