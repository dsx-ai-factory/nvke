// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetSubnets(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.Subnet, error) {
	var pageNumber int32 = 1
	allSubnets := []*forgesdk.Subnet{}

	// begin fetching every page
	for {
		apir := apiClient.SubnetAPI.GetAllSubnet(ctx, orgID).
			SiteId(siteID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageSubnets, res, err := apir.Execute()
		if err != nil {
			return nil, err
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, subnet := range pageSubnets {
			allSubnets = append(allSubnets, &subnet)
		}

		if len(pageSubnets) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allSubnets, nil
}
