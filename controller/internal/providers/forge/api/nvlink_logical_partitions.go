// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
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
