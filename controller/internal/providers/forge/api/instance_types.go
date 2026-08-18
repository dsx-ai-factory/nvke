// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetInstanceTypes(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.InstanceType, error) {
	var pageNumber int32 = 1
	allInstanceTypes := []*forgesdk.InstanceType{}

	// begin fetching every page
	for {
		apir := apiClient.InstanceTypeAPI.GetAllInstanceType(ctx, orgID).
			SiteId(siteID).
			IncludeAllocationStats(true).
			ExcludeUnallocated(true).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageInstanceTypes, res, err := apir.Execute()
		if err != nil {
			return nil, fmt.Errorf("failed to get page %d of instance types from forge: %w", pageNumber, err)
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, instanceType := range pageInstanceTypes {
			allInstanceTypes = append(allInstanceTypes, &instanceType)
		}

		if len(pageInstanceTypes) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allInstanceTypes, nil
}
