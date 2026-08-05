// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package forge

import (
	"context"
	"errors"
	"fmt"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"

	"github.com/NVIDIA/nke/controller/internal/providers/forge/api"
)

// TriggerInstanceReboot will reboot the specified instance.
func (p *ForgeProvider) TriggerInstanceReboot(ctx context.Context, orgID, instanceID string) (*forgesdk.Instance, error) {
	apiClient, ctx, err := p.cacheEngine.ClientAndContext(ctx, orgID)
	if err != nil {
		return nil, err
	}

	triggerReboot := true
	payload := forgesdk.InstanceUpdateRequest{
		TriggerReboot: *forgesdk.NewNullableBool(&triggerReboot),
	}

	return api.UpdateInstance(ctx, apiClient, orgID, instanceID, payload)
}

// ApplyInstanceLabels will apply the specified labels to the instance.
func (p *ForgeProvider) ApplyInstanceLabels(ctx context.Context, orgID, instanceID string, labels map[string]string) (*forgesdk.Instance, error) {
	apiClient, ctx, err := p.cacheEngine.ClientAndContext(ctx, orgID)
	if err != nil {
		return nil, err
	}

	payload := forgesdk.InstanceUpdateRequest{
		Labels: labels,
	}

	return api.UpdateInstance(ctx, apiClient, orgID, instanceID, payload)
}

// GetInstances will retrieve the instances for a given site. If a healthy cache exists, it will
// be used; otherwise, an error is returned.
func (p *ForgeProvider) GetInstances(ctx context.Context, orgID, siteID string) ([]*forgesdk.Instance, error) {
	cache := p.cacheEngine.GetSiteInstancesCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	return cache.Items(), nil
}

// GetInstanceByID will retrieve a specific instance by its ID for a given site. If a healthy cache exists, it will
func (p *ForgeProvider) GetInstanceByID(ctx context.Context, orgID, siteID, instanceID string) (*forgesdk.Instance, error) {
	cache := p.cacheEngine.GetSiteInstancesCache(orgID, siteID)

	if cache.IsExpired() {
		lastError := cache.GetLastError()
		if lastError == nil {
			return nil, fmt.Errorf("cache expired")
		}

		return nil, fmt.Errorf("cache expired: %w", lastError)
	}

	instance, found := cache.Get(instanceID)
	if !found {
		return nil, fmt.Errorf("instance %s not found in cache", instanceID)
	}

	return instance, nil
}

// CreateInstance will create a new instance in Forge.
func (p *ForgeProvider) CreateInstance(ctx context.Context, orgID string, req forgesdk.InstanceCreateRequest) (*forgesdk.Instance, error) {
	apiClient, ctx, err := p.cacheEngine.ClientAndContext(ctx, orgID)
	if err != nil {
		return nil, err
	}

	p.instanceMutationMu.Lock()
	defer p.instanceMutationMu.Unlock()

	return api.CreateInstance(ctx, apiClient, orgID, req)
}

// DeleteInstance will delete the specified instance in Forge.
func (p *ForgeProvider) DeleteInstance(ctx context.Context, orgID, instanceID string, machineHealthIssue *MachineHealthIssue) error {
	apiClient, ctx, err := p.cacheEngine.ClientAndContext(ctx, orgID)
	if err != nil {
		return err
	}

	payload := forgesdk.InstanceDeleteRequest{
		MachineHealthIssue: toForgeSDKMachineHealthIssue(machineHealthIssue),
	}

	p.instanceMutationMu.Lock()
	defer p.instanceMutationMu.Unlock()

	if err := api.DeleteInstance(ctx, apiClient, orgID, instanceID, payload); err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil
		}
		return err
	}

	return nil
}
