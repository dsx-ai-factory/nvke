// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"
)

// KubernetesSourceOptions scopes the events delivered to a Kubernetes source.
// An empty namespace and a nil label selector match all objects.
type KubernetesSourceOptions struct {
	Namespace     string
	LabelSelector labels.Selector
}

// NewKubernetesSource watches Kubernetes objects and maps their create, update,
// and delete events to reconcile requests. mapFunc must not be nil.
//
// Namespace and label filtering is applied client-side to each mapped object.
// Configure the manager cache separately when the informer itself must be
// namespace- or label-scoped.
func NewKubernetesSource[object client.Object, request comparable](
	theCache cache.Cache,
	obj object,
	mapFunc handler.TypedMapFunc[object, request],
	opts KubernetesSourceOptions,
) crsource.TypedSyncingSource[request] {
	return crsource.TypedKind(theCache, obj, newKubernetesEventHandler(mapFunc, opts))
}

func newKubernetesEventHandler[object client.Object, request comparable](
	mapFunc handler.TypedMapFunc[object, request],
	opts KubernetesSourceOptions,
) handler.TypedEventHandler[object, request] {
	return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj object) []request {
		if !opts.matches(obj) {
			return nil
		}
		return mapFunc(ctx, obj)
	})
}

func (opts KubernetesSourceOptions) matches(object client.Object) bool {
	if opts.Namespace != "" && object.GetNamespace() != opts.Namespace {
		return false
	}
	selector := opts.LabelSelector
	if selector == nil {
		selector = labels.Everything()
	}
	return selector.Matches(labels.Set(object.GetLabels()))
}
