// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package rollout

// Registrar wires one rollout extension package into a Registry. Build
// Registrars at the dependency-injection layer, where each package-specific
// dependency is already available, then pass the collection to runtime setup.
type Registrar interface {
	RegisterRollout(reg *Registry)
}

// RegistrarFunc adapts a function into a Registrar.
type RegistrarFunc func(*Registry)

func (f RegistrarFunc) RegisterRollout(reg *Registry) {
	f(reg)
}
