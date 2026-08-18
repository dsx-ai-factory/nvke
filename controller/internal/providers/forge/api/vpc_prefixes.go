// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetVpcPrefixes(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.VpcPrefix, error) {
	var pageNumber int32 = 1
	allVpcPrefixes := []*forgesdk.VpcPrefix{}

	// begin fetching every page
	for {
		apir := apiClient.VPCPrefixAPI.GetAllVpcPrefix(ctx, orgID).
			SiteId(siteID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageVpcPrefixes, res, err := apir.Execute()
		if err != nil {
			return nil, fmt.Errorf("failed to get page %d of VPC prefixes from forge: %w", pageNumber, err)
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, vpcPrefix := range pageVpcPrefixes {
			allVpcPrefixes = append(allVpcPrefixes, &vpcPrefix)
		}

		if len(pageVpcPrefixes) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allVpcPrefixes, nil
}
