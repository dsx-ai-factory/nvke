// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package common

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
)

func GetForgeInstanceType(
	ctx context.Context,
	forgeProvider *forge.ForgeProvider,
	cluster db.Cluster,
	instanceTypeID uuid.UUID,
) (*forgesdk.InstanceType, error) {
	if instanceTypeID == uuid.Nil {
		return nil, nil //nolint:nilnil // no Forge lookup for nil instance type
	}
	if cluster.ProviderType != db.ProviderTypeNico {
		return nil, nil //nolint:nilnil // local/CAPI clusters have no Forge site
	}
	nicoProvider, err := cluster.NicoProvider()
	if err != nil {
		return nil, err
	}
	if forgeProvider == nil {
		return nil, fmt.Errorf("forge provider is nil")
	}

	instanceType, err := forgeProvider.GetInstanceTypeByID(
		ctx,
		nicoProvider.OrgID,
		nicoProvider.SiteID.String(),
		instanceTypeID.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("get forge instance type %s: %w", instanceTypeID, err)
	}

	return instanceType, nil
}

func GetForgeInstanceTypeUnusedUsable(
	ctx context.Context,
	forgeProvider *forge.ForgeProvider,
	cluster db.Cluster,
	instanceTypeID uuid.UUID,
) (int, error) {
	instanceType, err := GetForgeInstanceType(ctx, forgeProvider, cluster, instanceTypeID)
	if err != nil {
		return 0, err
	}
	if instanceType == nil || instanceType.AllocationStats == nil || instanceType.AllocationStats.UnusedUsable == nil {
		return 0, nil
	}
	return int(*instanceType.AllocationStats.UnusedUsable), nil
}
