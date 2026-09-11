// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/spiffe"
)

type trustDomainAndBundleGetter func(context.Context, config.Config) (string, string, error)

type Activities struct {
	cfg                  config.Config
	db                   db.DB
	k8sClient            ctrlclient.Client
	nodeAgentClient      agent.Client
	tenantClientProvider TenantClientProvider
	trustDomain          string
	getTrustDomainBundle trustDomainAndBundleGetter
}

func NewActivities(
	cfg config.Config,
	database db.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
	trustDomain string,
	tenantClientProvider TenantClientProvider,
) *Activities {
	return &Activities{
		cfg:                  cfg,
		db:                   database,
		k8sClient:            k8sClient,
		nodeAgentClient:      nodeAgentClient,
		tenantClientProvider: tenantClientProvider,
		trustDomain:          trustDomain,
		getTrustDomainBundle: spiffe.GetTrustDomainAndBundle,
	}
}
