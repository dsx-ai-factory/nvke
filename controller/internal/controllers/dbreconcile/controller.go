// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package dbreconcile

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"
)

func SetupController(mgr manager.Manager, name string, reconciler reconcile.TypedReconciler[Key], source crsource.TypedSource[Key]) error {
	if source == nil {
		return fmt.Errorf("must specify Source for DB reconciler")
	}

	ctrl, err := controller.NewTyped[Key](name, mgr, controller.TypedOptions[Key]{
		Reconciler: reconciler,
	})
	if err != nil {
		return err
	}

	return ctrl.Watch(source)
}
