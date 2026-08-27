// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

const (
	clusterNamespacePrefix = "cluster-"
	ClusterNameMaxLength   = 63 - len(clusterNamespacePrefix)
)

func LegacyClusterNamespace(clusterName string) string {
	return clusterNamespacePrefix + clusterName
}
