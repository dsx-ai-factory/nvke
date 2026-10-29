// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/nodeagent"
)

type Option func(*EnableOIDCOperation)

// WithDialer injects the agent dialer. A nil dialer is recorded as unset so
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
func WithOIDCConfig(oidc *config.NvkeOIDCAuth) Option {
	return func(op *EnableOIDCOperation) { op.oidcConfig = oidc }
}

// WithTrustDomain injects the management SPIFFE trust domain.
func WithTrustDomain(trustDomain string) Option {
	return func(op *EnableOIDCOperation) { op.trustDomain = trustDomain }
}

// WithRBACConfigMap injects the operator-managed RBAC ConfigMap whose User
// subjects are reserved (config.Config.RBACConfigMapSource).
func WithRBACConfigMap(src nvkerbac.Source) Option {
	return func(op *EnableOIDCOperation) { op.rbacConfigMap = src }
}

// WithMethodSupportChecker injects the reusable agent capability check
// (gRPC reflection) that Validate uses to confirm every control-plane agent
// implements the PatchAPIServerManifest RPC before any node is touched — in place
// of an agent version floor. A nil checker is recorded as unset so Validate
// fails loudly.
func WithMethodSupportChecker(checker nodeagent.MethodSupportChecker) Option {
	return func(op *EnableOIDCOperation) { op.methodChecker = checker }
}
