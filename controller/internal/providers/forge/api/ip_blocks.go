// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetIPBlocks(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.IpBlock, error) {
	var pageNumber int32 = 1
	allIPBlocks := []*forgesdk.IpBlock{}

	// begin fetching every page
	for {
		apir := apiClient.IPBlockAPI.GetAllIpblock(ctx, orgID).
			SiteId(siteID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageIPBlocks, res, err := apir.Execute()
		if err != nil {
			return nil, err
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, ipBlock := range pageIPBlocks {
			allIPBlocks = append(allIPBlocks, &ipBlock)
		}

		if len(pageIPBlocks) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allIPBlocks, nil
}
