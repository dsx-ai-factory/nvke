// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
)

type forgeProvider interface {
	ApplyInstanceLabels(ctx context.Context, orgID, instanceID string, labels map[string]string) (*forgesdk.Instance, error)
	CreateInstance(ctx context.Context, orgID string, req forgesdk.InstanceCreateRequest) (*forgesdk.Instance, error)
	DeleteInstance(ctx context.Context, orgID, instanceID string, machineHealthIssue *forge.MachineHealthIssue) error
	GetInstanceByID(ctx context.Context, orgID, siteID, instanceID string) (*forgesdk.Instance, error)
	GetInstanceTypeByID(ctx context.Context, orgID, siteID, instanceTypeID string) (*forgesdk.InstanceType, error)
	GetInstances(ctx context.Context, orgID, siteID string) ([]*forgesdk.Instance, error)
	GetSiteByID(ctx context.Context, orgID, siteID string) (*forgesdk.Site, error)
	GetVPCs(ctx context.Context, orgID, siteID string) ([]*forgesdk.VPC, error)
	TriggerInstanceReboot(ctx context.Context, orgID, instanceID string) (*forgesdk.Instance, error)
}

type Activities struct {
	cfg                  config.Config
	db                   db.DB
	k8sClient            ctrlclient.Client
	nodeAgentClient      agent.Client
	tenantClientProvider TenantClientProvider
	forgeProvider        forgeProvider
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
