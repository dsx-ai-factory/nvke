// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func GetSSHKeyGroups(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.SshKeyGroup, error) {
	var pageNumber int32 = 1
	allSSHKeyGroups := []*forgesdk.SshKeyGroup{}

	// begin fetching every page
	for {
		apir := apiClient.SSHKeyGroupAPI.GetAllSshKeyGroup(ctx, orgID).
			SiteId(siteID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageSSHKeyGroups, res, err := apir.Execute()
		if err != nil {
			return nil, err
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, sshKeyGroup := range pageSSHKeyGroups {
			allSSHKeyGroups = append(allSSHKeyGroups, &sshKeyGroup)
		}

		if len(pageSSHKeyGroups) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allSSHKeyGroups, nil
}
