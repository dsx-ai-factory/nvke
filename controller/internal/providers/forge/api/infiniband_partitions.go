// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetInfiniBandPartitions(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.InfiniBandPartition, error) {
	var pageNumber int32 = 1
	allInfiniBandPartitions := []*forgesdk.InfiniBandPartition{}

	// begin fetching every page
	for {
		apir := apiClient.InfiniBandPartitionAPI.GetAllInfinibandPartition(ctx, orgID).
			SiteId(siteID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageInfiniBandPartitions, res, err := apir.Execute()
		if err != nil {
			return nil, err
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, infinibandPartition := range pageInfiniBandPartitions {
			allInfiniBandPartitions = append(allInfiniBandPartitions, &infinibandPartition)
		}

		if len(pageInfiniBandPartitions) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allInfiniBandPartitions, nil
}
