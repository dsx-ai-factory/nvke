// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shared

// TenantAPIAccessMethod names how the controller reaches a tenant cluster's
// Kubernetes API. A provider client chooses one; internal/kube owns the
// mechanics of each, so a provider only says which, never how.
type TenantAPIAccessMethod string

const (
	// TenantAPIAccessSecret reads the kubeconfig TenantAccess publishes in
	// nke-system/<cluster-name>-kubeconfig.
	TenantAPIAccessSecret TenantAPIAccessMethod = "tenant-access-secret"
	// TenantAPIAccessNodeAgent asks a joined control-plane node's node agent
	// for a kubeconfig and uses the first whose API server is reachable.
	TenantAPIAccessNodeAgent TenantAPIAccessMethod = "control-plane-node-agent"
)
