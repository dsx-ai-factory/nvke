// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"

	dbsource "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/source"
)

func SetupController(
	mgr manager.Manager,
	name string,
	reconciler reconcile.TypedReconciler[Key],
	sources ...crsource.TypedSource[Key],
) error {
	return setupController(mgr, name, reconciler, crcontroller.TypedOptions[Key]{}, sources...)
}

func setupController(
	mgr manager.Manager,
	name string,
	reconciler reconcile.TypedReconciler[Key],
	opts crcontroller.TypedOptions[Key],
	sources ...crsource.TypedSource[Key],
) error {
	if len(sources) == 0 {
		return fmt.Errorf("must specify Source for DB reconciler")
	}
	for _, source := range sources {
		if source == nil {
			return fmt.Errorf("must specify Source for DB reconciler")
		}
	}

	opts.Reconciler = reconciler
	ctrl, err := crcontroller.NewTyped[Key](name, mgr, opts)
	if err != nil {
		return err
	}

	for _, source := range sources {
		if err := ctrl.Watch(source); err != nil {
			return err
		}
	}
	return nil
}

// AddController registers a Controller with a controller-runtime manager.
// It's just syntactic sugar around SetupController.
func AddController(mgr manager.Manager, controller Controller) error {
	if controller.Workers < 0 {
		return fmt.Errorf("controller %q: Workers must not be negative", controller.Name)
	}
	if controller.ReconcileTimeout < 0 {
		return fmt.Errorf("controller %q: ReconcileTimeout must not be negative", controller.Name)
	}

	sources := append([]crsource.TypedSource[Key](nil), controller.Sources...)
	for index, watch := range controller.Watches {
		if watch.Object == nil {
			return fmt.Errorf("controller %q: Watches[%d].Object must not be nil", controller.Name, index)
		}
		if watch.Map == nil {
			return fmt.Errorf("controller %q: Watches[%d].Map must not be nil", controller.Name, index)
		}

		selector := labels.Everything()
		if watch.LabelSelector != nil {
			var err error
			selector, err = metav1.LabelSelectorAsSelector(watch.LabelSelector)
			if err != nil {
				return fmt.Errorf("controller %q: Watches[%d] invalid label selector: %w", controller.Name, index, err)
			}
		}

		mapFunc := watch.Map
		logger := mgr.GetLogger().WithValues("controller", controller.Name)
		sources = append(sources, dbsource.NewKubernetesSource(
			mgr.GetCache(),
			watch.Object,
			func(ctx context.Context, object client.Object) []Key {
				mapLog := logger.WithValues("mapObject", client.ObjectKeyFromObject(object))
				return mapFunc(crlog.IntoContext(ctx, mapLog), object)
			},
			dbsource.KubernetesSourceOptions{
				Namespace:     watch.Namespace,
				LabelSelector: selector,
			},
		))
	}

	return setupController(
		mgr,
		controller.Name,
		controller.Reconciler,
		crcontroller.TypedOptions[Key]{
			MaxConcurrentReconciles: controller.Workers,
			ReconciliationTimeout:   controller.ReconcileTimeout,
		},
		sources...,
	)
}
