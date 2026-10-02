// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package registry composes the management engine's production controllers.
package registry

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Register installs production controllers before mgr.Start.
// The registry is currently empty.
func Register(mgr ctrl.Manager) error {
	if mgr == nil {
		return fmt.Errorf("controller registration requires a manager")
	}
	return nil
}
