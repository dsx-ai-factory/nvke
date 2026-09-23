// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dsx-ai-factory/nke/agent/internal/k8s"
)

func TestConfigureDynamicClientRESTConfig_setsRewriteRateLimits(t *testing.T) {
	cfg := &rest.Config{}

	k8s.ConfigureDynamicClientRESTConfigForTest(cfg)

	assert.Equal(t, float32(1000), cfg.QPS)
	assert.Equal(t, 1000, cfg.Burst)
}

func TestRewriteEncryptableAPIObjects_secrets(t *testing.T) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns1"},
		StringData: map[string]string{"k": "v"},
	}
	client := fake.NewSimpleDynamicClient(clientscheme.Scheme, secret)
	k8s.SetDynamicClientForTest(client)
	t.Cleanup(func() { k8s.SetDynamicClientForTest(nil) })

	err := k8s.RewriteEncryptableAPIObjects(context.Background(), []string{"secrets"})
	require.NoError(t, err)
}

func TestRewriteEncryptableAPIObjects_persistentVolume(t *testing.T) {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1"},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{v1.ResourceStorage: resource.MustParse("1Gi")},
		},
	}
	client := fake.NewSimpleDynamicClient(clientscheme.Scheme, pv)
	k8s.SetDynamicClientForTest(client)
	t.Cleanup(func() { k8s.SetDynamicClientForTest(nil) })

	err := k8s.RewriteEncryptableAPIObjects(context.Background(), []string{"persistentvolumes"})
	require.NoError(t, err)
}

func TestRewriteEncryptableAPIObjects_retriesTooManyRequestsOnGet(t *testing.T) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns1"},
		StringData: map[string]string{"k": "v"},
	}
	client := fake.NewSimpleDynamicClient(clientscheme.Scheme, secret)
	var getAttempts int32
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if atomic.AddInt32(&getAttempts, 1) == 1 {
			return true, nil, apierrors.NewTooManyRequests("apiserver rate limited GET", 0)
		}
		return false, nil, nil
	})
	k8s.SetDynamicClientForTest(client)
	t.Cleanup(func() { k8s.SetDynamicClientForTest(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := k8s.RewriteEncryptableAPIObjects(ctx, []string{"secrets"})
	require.NoError(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&getAttempts))
}

func TestRewriteEncryptableAPIObjects_retriesTooManyRequestsOnUpdate(t *testing.T) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns1"},
		StringData: map[string]string{"k": "v"},
	}
	client := fake.NewSimpleDynamicClient(clientscheme.Scheme, secret)
	var updateAttempts int32
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if atomic.AddInt32(&updateAttempts, 1) == 1 {
			return true, nil, apierrors.NewTooManyRequests("apiserver rate limited UPDATE", 0)
		}
		return false, nil, nil
	})
	k8s.SetDynamicClientForTest(client)
	t.Cleanup(func() { k8s.SetDynamicClientForTest(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := k8s.RewriteEncryptableAPIObjects(ctx, []string{"secrets"})
	require.NoError(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&updateAttempts))
}

func TestRewriteEncryptableAPIObjects_unknownKind(t *testing.T) {
	client := fake.NewSimpleDynamicClient(clientscheme.Scheme)
	k8s.SetDynamicClientForTest(client)
	t.Cleanup(func() { k8s.SetDynamicClientForTest(nil) })

	err := k8s.RewriteEncryptableAPIObjects(context.Background(), []string{"not-a-plural"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown encryptable resource")
}

func TestRewriteEncryptableAPIObjects_rewritesObjectsInBoundedParallel(t *testing.T) {
	workerCount := k8s.EncryptRewriteWorkersForTest()
	totalObjects := workerCount * 2
	client := newBlockingRewriteDynamicClient(totalObjects, workerCount)
	k8s.SetDynamicClientForTest(client)
	t.Cleanup(func() { k8s.SetDynamicClientForTest(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- k8s.RewriteEncryptableAPIObjects(ctx, []string{"secrets"})
	}()

	select {
	case <-client.saturationSeen:
	case <-time.After(2 * time.Second):
		client.releaseGets()
		require.Failf(t, "rewrite did not saturate worker pool", "max concurrent GETs: %d", atomic.LoadInt32(&client.maxInFlight))
	}

	// Pause briefly after saturationSeen so RewriteEncryptableAPIObjects keeps
	// workers saturated before releaseGets unblocks GETs. The 100ms delay is
	// intentionally short but timing-sensitive in CI; do not remove or shorten it
	// without preserving this saturation window.
	time.Sleep(100 * time.Millisecond)
	client.releaseGets()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "rewrite did not finish after releasing blocked GETs")
	}

	assert.Equal(t, int32(totalObjects), atomic.LoadInt32(&client.updateCount))
	assert.GreaterOrEqual(t, atomic.LoadInt32(&client.maxInFlight), int32(2))
	assert.LessOrEqual(t, int(atomic.LoadInt32(&client.maxInFlight)), workerCount)
}

type blockingRewriteDynamicClient struct {
	totalObjects   int
	workerCount    int
	release        chan struct{}
	releaseOnce    sync.Once
	saturationSeen chan struct{}
	saturationOnce sync.Once
	inFlight       int32
	maxInFlight    int32
	updateCount    int32
}

func newBlockingRewriteDynamicClient(totalObjects, workerCount int) *blockingRewriteDynamicClient {
	return &blockingRewriteDynamicClient{
		totalObjects:   totalObjects,
		workerCount:    workerCount,
		release:        make(chan struct{}),
		saturationSeen: make(chan struct{}),
	}
}

func (c *blockingRewriteDynamicClient) Resource(_ schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &blockingRewriteResource{client: c}
}

func (c *blockingRewriteDynamicClient) releaseGets() {
	c.releaseOnce.Do(func() {
		close(c.release)
	})
}

type blockingRewriteResource struct {
	client    *blockingRewriteDynamicClient
	namespace string
}

func (r *blockingRewriteResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &blockingRewriteResource{client: r.client, namespace: namespace}
}

func (r *blockingRewriteResource) List(_ context.Context, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	list := &unstructured.UnstructuredList{}
	for i := range r.client.totalObjects {
		list.Items = append(list.Items, *secretObject(fmt.Sprintf("secret-%03d", i), "ns1"))
	}
	return list, nil
}

func (r *blockingRewriteResource) Get(ctx context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	inFlight := atomic.AddInt32(&r.client.inFlight, 1)
	defer atomic.AddInt32(&r.client.inFlight, -1)

	for {
		maxInFlight := atomic.LoadInt32(&r.client.maxInFlight)
		if inFlight <= maxInFlight || atomic.CompareAndSwapInt32(&r.client.maxInFlight, maxInFlight, inFlight) {
			break
		}
	}
	if int(inFlight) >= r.client.workerCount {
		r.client.saturationOnce.Do(func() {
			close(r.client.saturationSeen)
		})
	}

	select {
	case <-r.client.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return secretObject(name, r.namespace), nil
}

func (r *blockingRewriteResource) Update(_ context.Context, obj *unstructured.Unstructured, _ metav1.UpdateOptions, _ ...string) (*unstructured.Unstructured, error) {
	atomic.AddInt32(&r.client.updateCount, 1)
	return obj, nil
}

func secretObject(name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("Secret")
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func (*blockingRewriteResource) Create(context.Context, *unstructured.Unstructured, metav1.CreateOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, errors.New("not implemented")
}

func (*blockingRewriteResource) UpdateStatus(context.Context, *unstructured.Unstructured, metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	return nil, errors.New("not implemented")
}

func (*blockingRewriteResource) Delete(context.Context, string, metav1.DeleteOptions, ...string) error {
	return errors.New("not implemented")
}

func (*blockingRewriteResource) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	return errors.New("not implemented")
}

func (*blockingRewriteResource) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, errors.New("not implemented")
}

func (*blockingRewriteResource) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, errors.New("not implemented")
}

func (*blockingRewriteResource) Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, errors.New("not implemented")
}

func (*blockingRewriteResource) ApplyStatus(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	return nil, errors.New("not implemented")
}
