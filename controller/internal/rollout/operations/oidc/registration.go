// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package oidc

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
)

// RegisterDefaults wires the enable-OIDC operation into reg with its runtime
// dependencies: the node-agent dialer, the reflection-based capability checker
// (verifies PatchAPIServerManifest support before any node is touched), the
// management-cluster client (reads the issuer CA bundle), and the site OIDC
// config.
func RegisterDefaults(reg *rollout.Registry, dialer nodeagent.Dialer, checker nodeagent.MethodSupportChecker, k8sClient client.Client, oidc config.NkeOIDCAuth) {
	oidcConfig := oidc
	reg.RegisterOperation(rollout.Compile(NewEnableOIDCOperation(
		WithDialer(dialer),
		WithMethodSupportChecker(checker),
		WithClient(k8sClient),
		WithOIDCConfig(&oidcConfig),
	)))
}
