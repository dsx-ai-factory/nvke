// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetVPCs(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.VPC, error) {
	var pageNumber int32 = 1
	allVPCs := []*forgesdk.VPC{}

	// begin fetching every page
	for {
		apir := apiClient.VPCAPI.GetAllVpc(ctx, orgID).
			SiteId(siteID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageVPCs, res, err := apir.Execute()
		if err != nil {
			return nil, err
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, vpc := range pageVPCs {
			allVPCs = append(allVPCs, &vpc)
		}

		if len(pageVPCs) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allVPCs, nil
}
