// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

const (
	ClusterNamespacePrefix = "cluster-"
	ClusterNameMaxLength   = 63 - len(ClusterNamespacePrefix)
)

func ClusterNamespace(clusterName string) string {
	return ClusterNamespacePrefix + clusterName
}
