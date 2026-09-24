// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllertest

import (
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

type fakeCache struct {
	client   client.WithWatch
	scheme   *runtime.Scheme
	byObject map[client.Object]cache.ByObject

	mu        sync.Mutex
	informers map[schema.GroupVersionKind]toolscache.SharedIndexInformer
	started   map[schema.GroupVersionKind]bool
	stopCh    <-chan struct{}
}

var _ cache.Cache = (*fakeCache)(nil)

func newFakeCache(
	kubeClient client.WithWatch,
	scheme *runtime.Scheme,
	byObject map[client.Object]cache.ByObject,
) *fakeCache {
	return &fakeCache{
		client:    kubeClient,
		scheme:    scheme,
		byObject:  byObject,
		informers: make(map[schema.GroupVersionKind]toolscache.SharedIndexInformer),
		started:   make(map[schema.GroupVersionKind]bool),
	}
}

func (c *fakeCache) scopeFor(gvk schema.GroupVersionKind) (string, labels.Selector) {
	for object, byObject := range c.byObject {
		objectGVK, err := apiutil.GVKForObject(object, c.scheme)
		if err != nil || objectGVK != gvk {
			continue
		}
		if len(byObject.Namespaces) > 0 {
			for namespace, config := range byObject.Namespaces {
				selector := config.LabelSelector
				if selector == nil {
					selector = labels.Everything()
				}
				return namespace, selector
			}
		}
		selector := byObject.Label
		if selector == nil {
			selector = labels.Everything()
		}
		return metav1.NamespaceAll, selector
	}
	return metav1.NamespaceAll, labels.Everything()
}

func (c *fakeCache) informerForObject(object client.Object) (cache.Informer, error) {
	gvk, err := apiutil.GVKForObject(object, c.scheme)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if informer, ok := c.informers[gvk]; ok {
		return informer, nil
	}

	listGVK := gvk
	listGVK.Kind += "List"
	if _, err := c.newList(listGVK); err != nil {
		return nil, err
	}
	namespace, selector := c.scopeFor(gvk)
	listOptions := []client.ListOption{client.MatchingLabelsSelector{Selector: selector}}
	if namespace != metav1.NamespaceAll {
		listOptions = append(listOptions, client.InNamespace(namespace))
	}

	listWatch := &toolscache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, _ metav1.ListOptions) (runtime.Object, error) {
			list, err := c.newList(listGVK)
			if err != nil {
				return nil, err
			}
			if err := c.client.List(ctx, list, listOptions...); err != nil {
				return nil, err
			}
			return list, nil
		},
		WatchFuncWithContext: func(ctx context.Context, _ metav1.ListOptions) (watch.Interface, error) {
			list, err := c.newList(listGVK)
			if err != nil {
				return nil, err
			}
			return c.client.Watch(ctx, list, listOptions...)
		},
	}
	informer := toolscache.NewSharedIndexInformer(
		toolscache.ToListWatcherWithWatchListSemantics(listWatch, watchListUnsupported{}),
		object.DeepCopyObject(),
		0,
		toolscache.Indexers{toolscache.NamespaceIndex: toolscache.MetaNamespaceIndexFunc},
	)
	c.informers[gvk] = informer
	if c.stopCh != nil {
		c.started[gvk] = true
		go informer.Run(c.stopCh)
	}
	return informer, nil
}

// watchListUnsupported tells client-go to use List followed by Watch. The
// controller-runtime fake client does not emit the bookmark required by the
// API server's streaming WatchList protocol.
type watchListUnsupported struct{}

func (watchListUnsupported) IsWatchListSemanticsUnSupported() bool { return true }

func (c *fakeCache) newList(gvk schema.GroupVersionKind) (client.ObjectList, error) {
	object, err := c.scheme.New(gvk)
	if err != nil {
		return nil, err
	}
	list, ok := object.(client.ObjectList)
	if !ok {
		return nil, fmt.Errorf("scheme object %s is not a client.ObjectList", gvk)
	}
	return list, nil
}

func (c *fakeCache) GetInformer(
	ctx context.Context,
	object client.Object,
	_ ...cache.InformerGetOption,
) (cache.Informer, error) {
	return c.informerForObject(object)
}

func (c *fakeCache) GetInformerForKind(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	_ ...cache.InformerGetOption,
) (cache.Informer, error) {
	object, err := c.scheme.New(gvk)
	if err != nil {
		return nil, err
	}
	clientObject, ok := object.(client.Object)
	if !ok {
		return nil, fmt.Errorf("scheme object %s is not a client.Object", gvk)
	}
	return c.informerForObject(clientObject)
}

func (c *fakeCache) RemoveInformer(context.Context, client.Object) error { return nil }

func (c *fakeCache) Start(ctx context.Context) error {
	c.mu.Lock()
	c.stopCh = ctx.Done()
	informers := c.unstartedInformers()
	c.mu.Unlock()

	for _, informer := range informers {
		go informer.Run(ctx.Done())
	}
	<-ctx.Done()
	return nil
}

func (c *fakeCache) WaitForCacheSync(ctx context.Context) bool {
	c.mu.Lock()
	if c.stopCh == nil {
		c.stopCh = ctx.Done()
	}
	stop := c.stopCh
	toStart := c.unstartedInformers()
	informers := c.snapshotInformers()
	c.mu.Unlock()

	for _, informer := range toStart {
		go informer.Run(stop)
	}
	synced := make([]toolscache.InformerSynced, 0, len(informers))
	for _, informer := range informers {
		synced = append(synced, informer.HasSynced)
	}
	return toolscache.WaitForCacheSync(ctx.Done(), synced...)
}

// unstartedInformers returns and marks every informer that has not been run.
// Callers must hold c.mu.
func (c *fakeCache) unstartedInformers() []toolscache.SharedIndexInformer {
	informers := make([]toolscache.SharedIndexInformer, 0, len(c.informers))
	for gvk, informer := range c.informers {
		if c.started[gvk] {
			continue
		}
		c.started[gvk] = true
		informers = append(informers, informer)
	}
	return informers
}

func (c *fakeCache) snapshotInformers() []toolscache.SharedIndexInformer {
	informers := make([]toolscache.SharedIndexInformer, 0, len(c.informers))
	for _, informer := range c.informers {
		informers = append(informers, informer)
	}
	return informers
}

func (c *fakeCache) IndexField(
	ctx context.Context,
	object client.Object,
	field string,
	extractValue client.IndexerFunc,
) error {
	informer, err := c.GetInformer(ctx, object)
	if err != nil {
		return err
	}
	return informer.AddIndexers(toolscache.Indexers{
		field: func(raw any) ([]string, error) {
			clientObject, ok := raw.(client.Object)
			if !ok {
				return nil, fmt.Errorf("cached object %T is not a client.Object", raw)
			}
			return extractValue(clientObject), nil
		},
	})
}

func (c *fakeCache) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	return c.client.Get(ctx, key, object, opts...)
}

func (c *fakeCache) List(
	ctx context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	return c.client.List(ctx, list, opts...)
}
