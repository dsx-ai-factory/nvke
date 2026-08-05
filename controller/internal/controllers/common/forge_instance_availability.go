// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package common //nolint:revive // package name matches controllers/common directory

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"

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
	if cluster.SiteID == uuid.Nil {
		return nil, nil //nolint:nilnil // local/CAPI clusters have no Forge site
	}
	if forgeProvider == nil {
		return nil, fmt.Errorf("forge provider is nil")
	}

	instanceType, err := forgeProvider.GetInstanceTypeByID(
		ctx,
		cluster.OrgID,
		cluster.SiteID.String(),
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
