// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"
)

func SetupController(
	mgr manager.Manager,
	name string,
	reconciler reconcile.TypedReconciler[Key],
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

	ctrl, err := controller.NewTyped[Key](name, mgr, controller.TypedOptions[Key]{
		Reconciler: reconciler,
	})
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
