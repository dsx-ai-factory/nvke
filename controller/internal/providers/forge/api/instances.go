// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"net/http"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

func UpdateInstance(ctx context.Context, apiClient *forgesdk.APIClient, orgID, instanceID string, payload forgesdk.InstanceUpdateRequest) (*forgesdk.Instance, error) {
	apir := apiClient.InstanceAPI.UpdateInstance(ctx, orgID, instanceID).
		InstanceUpdateRequest(payload)

	instance, res, err := apir.Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to update instance: %w", asForgeAPIError(res, err))
	}

	return instance, nil
}

func GetInstances(ctx context.Context, apiClient *forgesdk.APIClient, orgID, siteID string) ([]*forgesdk.Instance, error) {
	var pageNumber int32 = 1
	allInstances := []*forgesdk.Instance{}

	// begin fetching every page
	for {
		apir := apiClient.InstanceAPI.GetAllInstance(ctx, orgID).
			SiteId(siteID).
			PageSize(MaxPageSize).
			PageNumber(pageNumber).
			OrderBy("CREATED_ASC") // this is so we can enforce consistent results from the API

		pageInstances, res, err := apir.Execute()
		if err != nil {
			return nil, fmt.Errorf("failed to get page %d of instances from forge: %w", pageNumber, err)
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}

		for _, instance := range pageInstances {
			allInstances = append(allInstances, &instance)
		}

		if len(pageInstances) < MaxPageSize {
			break
		}

		pageNumber++
	}

	return allInstances, nil
}

func CreateInstance(ctx context.Context, apiClient *forgesdk.APIClient, orgID string, payload forgesdk.InstanceCreateRequest) (*forgesdk.Instance, error) {
	apir := apiClient.InstanceAPI.CreateInstance(ctx, orgID).
		InstanceCreateRequest(payload)

	instance, res, err := apir.Execute()
	if err != nil {
		return nil, fmt.Errorf("failed to create forge instance: %w", asForgeAPIError(res, err))
	}

	return instance, nil
}

func DeleteInstance(ctx context.Context, apiClient *forgesdk.APIClient, orgID, instanceID string, payload forgesdk.InstanceDeleteRequest) error {
	apir := apiClient.InstanceAPI.DeleteInstance(ctx, orgID, instanceID).
		InstanceDeleteRequest(payload)

	res, err := apir.Execute()

	var statusCode int
	var body []byte
	if res != nil {
		statusCode = res.StatusCode
		if res.Body != nil {
			body, _ = io.ReadAll(res.Body)
			_ = res.Body.Close()
		}
	}

	if err != nil {
		if statusCode == http.StatusNotFound {
			return fmt.Errorf("%w: status %d: %s", ErrInstanceNotFound, statusCode, string(body))
		}

		if statusCode == http.StatusBadRequest {
			return fmt.Errorf("%w: status %d: %s", ErrBadRequest, statusCode, string(body))
		}

		return fmt.Errorf("failed to delete forge instance: status %d: %s: %w", statusCode, string(body), err)
	}

	return nil
}
