// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/nodeagent"
)

// RegisterDefaults wires the enable-OIDC operation into reg with its runtime
// dependencies: the agent dialer, the reflection-based capability checker
// (verifies PatchAPIServerManifest support before any node is touched), the
// management-cluster client (reads the issuer CA bundle and the RBAC manifest),
// the site OIDC config, the management SPIFFE trust domain, and the RBAC
// ConfigMap that manifest lives in.
func RegisterDefaults(
	reg *rollout.Registry,
	dialer nodeagent.Dialer,
	checker nodeagent.MethodSupportChecker,
	k8sClient client.Client,
	oidc config.NvkeOIDCAuth,
	trustDomain string,
	rbacConfigMap nvkerbac.Source,
) {
	oidcConfig := oidc
	reg.RegisterOperation(rollout.Compile(NewEnableOIDCOperation(
		WithDialer(dialer),
		WithMethodSupportChecker(checker),
		WithClient(k8sClient),
		WithOIDCConfig(&oidcConfig),
		WithTrustDomain(trustDomain),
		WithRBACConfigMap(rbacConfigMap),
	)))
}
