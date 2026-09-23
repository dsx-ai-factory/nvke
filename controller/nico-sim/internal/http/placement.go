// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package http

import (
	"context"
	"errors"
	"fmt"

	nicosdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/state"
)

// resolveCreateSite authenticates the tenant and follows the VPC to the site
// that selects the instance backend.
func (s *Server) resolveCreateSite(
	ctx context.Context,
	orgID string,
	req nicosdk.InstanceCreateRequest,
) (state.SiteRow, error) {
	if orgID == "" || req.GetTenantId() == "" || req.GetVpcId() == "" {
		return state.SiteRow{}, fmt.Errorf("%w: org, tenantId, and vpcId are required", state.ErrBadRequest)
	}
	tenant, err := s.store.GetTenantByOrg(ctx, orgID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return state.SiteRow{}, fmt.Errorf("%w: organization %s has no tenant", state.ErrBadRequest, orgID)
		}
		return state.SiteRow{}, err
	}
	if tenant.ID != req.GetTenantId() {
		return state.SiteRow{}, fmt.Errorf("%w: tenant %s does not belong to organization %s", state.ErrBadRequest, req.GetTenantId(), orgID)
	}
	vpc, err := s.store.GetVPCByID(ctx, req.GetVpcId())
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return state.SiteRow{}, fmt.Errorf("%w: vpc %s not found", state.ErrBadRequest, req.GetVpcId())
		}
		return state.SiteRow{}, err
	}
	if vpc.TenantID != tenant.ID {
		return state.SiteRow{}, fmt.Errorf("%w: vpc %s does not belong to tenant %s", state.ErrBadRequest, vpc.ID, tenant.ID)
	}
	site, err := s.store.GetSiteByID(ctx, vpc.SiteID)
	if err != nil {
		return state.SiteRow{}, fmt.Errorf("resolve site %s for vpc %s: %w", vpc.SiteID, vpc.ID, err)
	}
	if site.TenantID != tenant.ID {
		return state.SiteRow{}, fmt.Errorf("%w: site %s does not belong to tenant %s", state.ErrBadRequest, site.ID, tenant.ID)
	}
	if site.Backend != state.SiteBackendVM && site.Backend != state.SiteBackendContainer {
		return state.SiteRow{}, fmt.Errorf("%w: site %s has unsupported backend %q", state.ErrBadRequest, site.ID, site.Backend)
	}
	return site, nil
}
