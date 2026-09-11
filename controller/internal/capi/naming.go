// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

const (
	// ClusterNameMaxLength caps cluster names so the Kubernetes object names
	// derived from them stay inside the 63-character limit. The eight characters
	// of headroom date from the retired per-cluster "cluster-<name>" namespace.
	ClusterNameMaxLength = 63 - 8
)
