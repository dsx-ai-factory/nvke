// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"
)

// Reconciler reconciles DB record keys.
type Reconciler = reconcile.TypedReconciler[Key]

// Source is a source of events for DB record keys.
type Source = crsource.TypedSource[Key]

// Controller defines a DB reconciler and the sources that feed its work queue.
type Controller struct {
	Name       string
	Reconciler Reconciler
	Watches    []KubeWatch
	Sources    []Source
}

// KubeWatch maps selected Kubernetes object events to DB reconcile keys. When
// registered through AddController, Map receives a context containing a logger
// tagged with the controller name and object key.
type KubeWatch struct {
	Object        client.Object
	Namespace     string
	LabelSelector *metav1.LabelSelector
	Map           func(context.Context, client.Object) []Key
}
