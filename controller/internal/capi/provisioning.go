// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"github.com/NVIDIA/nke/controller/internal/db"
)

func ShouldUseCAPIForNodeProvisioning(enableForgeCapi bool, cluster db.Cluster) bool {
	return enableForgeCapi || cluster.ProviderType == db.ProviderTypeCAPI
}
