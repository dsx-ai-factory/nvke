// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetSites(ctx context.Context, apiClient *forgesdk.APIClient, orgID, tenantID string) ([]*forgesdk.Site, error) {
	var pageNumber int32 = 1
	allSites := []*forgesdk.Site{}

	// begin fetching every page
	for {
		apir := apiClient.SiteAPI.GetAllSite(ctx, orgID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			TenantId(tenantID).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageSites, res, err := apir.Execute()
		if err != nil {
			// Map to a specific sentinel (e.g. ErrTenantNotAuthorized for a 403) so callers
			// can distinguish an expected no-access tenant from a genuine failure.
			return nil, fmt.Errorf("failed to get page %d of sites from forge: %w", pageNumber, asForgeAPIError(res, err))
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, site := range pageSites {
			allSites = append(allSites, &site)
		}

		if len(pageSites) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allSites, nil
}
