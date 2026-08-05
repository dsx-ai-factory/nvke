// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package api

import (
	"context"
	"fmt"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

// GetNVLinkLogicalPartitions retrieves all NVLink logical partitions for a given site.
func GetNVLinkLogicalPartitions(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.NVLinkLogicalPartition, error) {
	var pageNumber int32 = 1
	allNVLinkLogicalPartitions := []*forgesdk.NVLinkLogicalPartition{}

	// begin fetching every page
	for {
		apir := apiClient.NVLinkLogicalPartitionAPI.GetAllNvlinkLogicalPartition(ctx, orgID).
			SiteId(siteID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageNVLinkLogicalPartitions, res, err := apir.Execute()
		if err != nil {
			return nil, fmt.Errorf(
				"failed to get page %d of NVLink logical partitions from forge: %w",
				pageNumber,
				err,
			)
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, nvlinkLogicalPartition := range pageNVLinkLogicalPartitions {
			allNVLinkLogicalPartitions = append(allNVLinkLogicalPartitions, &nvlinkLogicalPartition)
		}

		if len(pageNVLinkLogicalPartitions) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allNVLinkLogicalPartitions, nil
}
