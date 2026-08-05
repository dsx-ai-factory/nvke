// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
)

type Activities struct {
	cfg                  config.Config
	db                   db.DB
	k8sClient            ctrlclient.Client
	nodeAgentClient      agent.Client
	tenantClientProvider TenantClientProvider
	forgeProvider        *forge.ForgeProvider
	trustDomain          string
}

func NewActivities(
	cfg config.Config,
	database db.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
	forgeProvider *forge.ForgeProvider,
	trustDomain string,
	tenantClientProvider TenantClientProvider,
) *Activities {
	return &Activities{
		cfg:                  cfg,
		db:                   database,
		k8sClient:            k8sClient,
		nodeAgentClient:      nodeAgentClient,
		tenantClientProvider: tenantClientProvider,
		forgeProvider:        forgeProvider,
		trustDomain:          trustDomain,
	}
}
