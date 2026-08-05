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

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
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
