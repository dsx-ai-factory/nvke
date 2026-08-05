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
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
)

type Option func(*EnableOIDCOperation)

// WithDialer injects the node-agent dialer. A nil dialer is recorded as unset so
// Validate fails loudly rather than letting a nil dialer masquerade as configured.
func WithDialer(dialer nodeagent.Dialer) Option {
	return func(op *EnableOIDCOperation) {
		if dialer == nil {
			op.nodeAgentClientDialer = nil
			return
		}
		op.nodeAgentClientDialer = func(ctx context.Context, node rollout.Node) (nodeAgentOIDCClient, func(), error) {
			return dialer(ctx, node)
		}
	}
}

// WithClient injects the management-cluster controller-runtime client used to
// read the OIDC issuer CA bundle when rendering the auth-config.
func WithClient(k8sClient client.Client) Option {
	return func(op *EnableOIDCOperation) { op.k8sClient = k8sClient }
}

// WithOIDCConfig injects the site OIDC settings (issuer URL, CA-bundle secret ref)
// the auth-config is rendered from.
func WithOIDCConfig(oidc *config.NkeOIDCAuth) Option {
	return func(op *EnableOIDCOperation) { op.oidcConfig = oidc }
}

// WithMethodSupportChecker injects the reusable node-agent capability check
// (gRPC reflection) that Validate uses to confirm every control-plane node-agent
// implements the PatchAPIServerManifest RPC before any node is touched — in place
// of a node-agent version floor. A nil checker is recorded as unset so Validate
// fails loudly.
func WithMethodSupportChecker(checker nodeagent.MethodSupportChecker) Option {
	return func(op *EnableOIDCOperation) { op.methodChecker = checker }
}
