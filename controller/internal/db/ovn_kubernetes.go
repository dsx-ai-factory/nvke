// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

// OVNKubernetesConfig stores optional cluster-wide OVN Kubernetes settings.
// Nil fields retain the packaged chart defaults.
type OVNKubernetesConfig struct {
	GatewayInterface *string `json:"gatewayInterface,omitempty"`
	AllowNoUplink    *bool   `json:"allowNoUplink,omitempty"`
	GatewayMode      *string `json:"gatewayMode,omitempty"`
	MTU              *int    `json:"mtu,omitempty"`
}
