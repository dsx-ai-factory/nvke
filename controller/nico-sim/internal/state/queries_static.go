// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package state provides SQLite-backed persistence for nico-sim.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/uptrace/bun"
)

// GetTenantByOrg returns the tenant seeded for the given organisation ID.
// It returns ErrNotFound when no tenant exists for the org so callers can map
// the miss to a 404 (used by the Forge tenant/current endpoint).
func (s *Store) GetTenantByOrg(ctx context.Context, orgID string) (TenantRow, error) {
	var row TenantRow
	if err := s.db.NewSelect().
		Model(&row).
		Where("org_id = ?", orgID).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return row, ErrNotFound
		}
		return row, fmt.Errorf("failed to get tenant for org %s: %w", orgID, err)
	}
	return row, nil
}

func (s *Store) GetSiteByID(ctx context.Context, siteID string) (SiteRow, error) {
	var site SiteRow
	if err := s.db.NewSelect().Model(&site).Where("id = ?", siteID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SiteRow{}, ErrNotFound
		}
		return SiteRow{}, fmt.Errorf("failed to get site %s: %w", siteID, err)
	}
	return site, nil
}

func (s *Store) GetSiteForOrg(ctx context.Context, orgID, siteID string) (SiteRow, error) {
	tenant, err := s.GetTenantByOrg(ctx, orgID)
	if err != nil {
		return SiteRow{}, err
	}
	site, err := s.GetSiteByID(ctx, siteID)
	if err != nil {
		return SiteRow{}, err
	}
	if site.TenantID != tenant.ID {
		return SiteRow{}, ErrNotFound
	}
	return site, nil
}

func (s *Store) GetVPCByID(ctx context.Context, vpcID string) (VPCRow, error) {
	var vpc VPCRow
	if err := s.db.NewSelect().Model(&vpc).Where("id = ?", vpcID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VPCRow{}, ErrNotFound
		}
		return VPCRow{}, fmt.Errorf("failed to get vpc %s: %w", vpcID, err)
	}
	return vpc, nil
}

func (s *Store) ListSites(ctx context.Context, orgID string, page Page) ([]SiteRow, error) {
	page = normalizePage(page)

	var tenant TenantRow
	if err := s.db.NewSelect().Model(&tenant).Where("org_id = ?", orgID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get tenant for org %s: %w", orgID, err)
	}

	var rows []SiteRow
	if err := s.db.NewSelect().
		Model(&rows).
		Where("tenant_id = ?", tenant.ID).
		Order("id ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list sites: %w", err)
	}
	return rows, nil
}

// GetVPC returns a single VPC by ID, constrained to the given organisation via
// the VPC's tenant (vpc -> tenant -> org_id). It returns ErrNotFound when the
// org has no tenant, when no VPC with that ID exists, or when the VPC belongs to
// a different org — a foreign-org VPC is deliberately indistinguishable from a
// missing one so the endpoint cannot be used to probe for existence. Callers map
// the miss to a 404 (used by the Forge vpc/{vpcId} endpoint that CAPNICo reads
// for machine topology).
func (s *Store) GetVPC(ctx context.Context, orgID, id string) (VPCRow, error) {
	var row VPCRow

	tenant, err := s.GetTenantByOrg(ctx, orgID)
	if err != nil {
		return row, err
	}

	if err := s.db.NewSelect().
		Model(&row).
		Where("id = ?", id).
		Where("tenant_id = ?", tenant.ID).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return row, ErrNotFound
		}
		return row, fmt.Errorf("failed to get vpc %s: %w", id, err)
	}
	return row, nil
}

func (s *Store) ListVPCs(ctx context.Context, siteID string, page Page) ([]VPCRow, error) {
	page = normalizePage(page)
	var rows []VPCRow
	if err := s.db.NewSelect().
		Model(&rows).
		Where("site_id = ?", siteID).
		Order("id ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list vpcs: %w", err)
	}
	return rows, nil
}

func (s *Store) ListSubnets(ctx context.Context, siteID string, page Page) ([]SubnetRow, error) {
	page = normalizePage(page)
	var rows []SubnetRow
	if err := s.db.NewSelect().
		Model(&rows).
		Where("site_id = ?", siteID).
		Order("id ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list subnets: %w", err)
	}
	return rows, nil
}

func (s *Store) ListVPCPrefixes(ctx context.Context, siteID string, page Page) ([]VPCPrefixRow, error) {
	page = normalizePage(page)
	var rows []VPCPrefixRow
	if err := s.db.NewSelect().
		Model(&rows).
		Where("site_id = ?", siteID).
		Order("id ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list vpc prefixes: %w", err)
	}
	return rows, nil
}

func (s *Store) ListSSHKeyGroups(ctx context.Context, siteID string, page Page) ([]SSHKeyGroupRow, error) {
	page = normalizePage(page)
	var rows []SSHKeyGroupRow
	if err := s.db.NewSelect().
		Model(&rows).
		Order("id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list ssh key groups: %w", err)
	}

	filtered := make([]SSHKeyGroupRow, 0, len(rows))
	for _, row := range rows {
		siteIDs, err := row.SiteIDs()
		if err != nil {
			return nil, err
		}
		for _, id := range siteIDs {
			if id == siteID {
				filtered = append(filtered, row)
				break
			}
		}
	}
	start := (page.Number - 1) * page.Size
	if start >= len(filtered) {
		return []SSHKeyGroupRow{}, nil
	}
	end := min(start+page.Size, len(filtered))
	return filtered[start:end], nil
}

func (s *Store) ListIPBlocks(ctx context.Context, siteID string, page Page) ([]IPBlockRow, error) {
	page = normalizePage(page)
	var rows []IPBlockRow
	if err := s.db.NewSelect().
		Model(&rows).
		Where("site_id = ?", siteID).
		Order("id ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list ip blocks: %w", err)
	}
	return rows, nil
}

func (s *Store) ListInfiniBandPartitions(ctx context.Context, siteID string, page Page) ([]InfiniBandPartitionRow, error) {
	page = normalizePage(page)
	var rows []InfiniBandPartitionRow
	if err := s.db.NewSelect().
		Model(&rows).
		Where("site_id = ?", siteID).
		Order("id ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list infiniband partitions: %w", err)
	}
	return rows, nil
}

// GetInstanceType returns a single instance type by ID, constrained to the given
// organisation via the type's site (instance_type -> site -> tenant -> org_id).
// It returns ErrNotFound when the org has no tenant, when no instance type with
// that ID exists, or when the instance type belongs to a different org — a
// foreign-org instance type is deliberately indistinguishable from a missing one
// (same sentinel, same number of statements) so the endpoint cannot be used to
// probe for existence. Callers map the miss to a 404.
//
// Use GetInstanceTypeByID for trusted internal paths that have no org in scope.
func (s *Store) GetInstanceType(ctx context.Context, orgID, id string) (InstanceTypeRow, error) {
	var row InstanceTypeRow

	tenant, err := s.GetTenantByOrg(ctx, orgID)
	if err != nil {
		return row, err
	}

	// The tenant's sites are resolved as a subquery rather than a second round
	// trip so a cross-org hit costs exactly as many statements as a genuine miss.
	tenantSites := s.db.NewSelect().
		Model((*SiteRow)(nil)).
		Column("id").
		Where("tenant_id = ?", tenant.ID)

	if err := s.db.NewSelect().
		Model(&row).
		Where("id = ?", id).
		Where("site_id IN (?)", tenantSites).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return row, ErrNotFound
		}
		return row, fmt.Errorf("failed to get instance type %s: %w", id, err)
	}
	return row, nil
}

// GetInstanceTypeByID returns a single instance type by ID with no organisation
// constraint. It is for trusted internal callers that have no org in scope — the
// reconciliation engine resolves vCPU/memory/disk for an instance it already
// owns. Do NOT call this from HTTP handlers: use GetInstanceType, which enforces
// org isolation.
func (s *Store) GetInstanceTypeByID(ctx context.Context, id string) (InstanceTypeRow, error) {
	var row InstanceTypeRow
	if err := s.db.NewSelect().
		Model(&row).
		Where("id = ?", id).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return row, ErrNotFound
		}
		return row, fmt.Errorf("failed to get instance type %s: %w", id, err)
	}
	return row, nil
}

func (s *Store) ListSitesByBackend(ctx context.Context, backend SiteBackend) ([]SiteRow, error) {
	var rows []SiteRow
	if err := s.db.NewSelect().Model(&rows).Where("backend = ?", backend).Order("id ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list %s sites: %w", backend, err)
	}
	return rows, nil
}

// ListVMSubnets returns the subnets attached to VM sites.
func (s *Store) ListVMSubnets(ctx context.Context) ([]SubnetRow, error) {
	var rows []SubnetRow
	if err := s.db.NewSelect().
		TableExpr("subnets AS subnet").
		ColumnExpr("subnet.*").
		Join("JOIN sites AS site ON site.id = subnet.site_id").
		Where("site.backend = ?", SiteBackendVM).
		OrderExpr("subnet.id ASC").
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failed to list VM subnets: %w", err)
	}
	return rows, nil
}

// ListVMVPCPrefixes returns the VPC prefixes attached to VM sites.
func (s *Store) ListVMVPCPrefixes(ctx context.Context) ([]VPCPrefixRow, error) {
	var rows []VPCPrefixRow
	if err := s.db.NewSelect().
		TableExpr("vpc_prefixes AS prefix").
		ColumnExpr("prefix.*").
		Join("JOIN sites AS site ON site.id = prefix.site_id").
		Where("site.backend = ?", SiteBackendVM).
		OrderExpr("prefix.id ASC").
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failed to list VM vpc prefixes: %w", err)
	}
	return rows, nil
}

// GetContainerSiteConfig returns the catalog IDs for the single container site.
func (s *Store) GetContainerSiteConfig(ctx context.Context) (ContainerSiteConfig, error) {
	sites, err := s.ListSitesByBackend(ctx, SiteBackendContainer)
	if err != nil {
		return ContainerSiteConfig{}, err
	}
	if len(sites) != 1 {
		return ContainerSiteConfig{}, fmt.Errorf("container backend requires exactly one site, found %d", len(sites))
	}
	site := sites[0]

	var tenant TenantRow
	if err := s.db.NewSelect().Model(&tenant).Where("id = ?", site.TenantID).Scan(ctx); err != nil {
		return ContainerSiteConfig{}, fmt.Errorf("resolve container tenant %s: %w", site.TenantID, err)
	}
	var vpcs []VPCRow
	if err := s.db.NewSelect().Model(&vpcs).Where("site_id = ?", site.ID).Order("id ASC").Scan(ctx); err != nil {
		return ContainerSiteConfig{}, fmt.Errorf("resolve container vpc: %w", err)
	}
	if len(vpcs) != 1 {
		return ContainerSiteConfig{}, fmt.Errorf("container site %s requires exactly one vpc, found %d", site.ID, len(vpcs))
	}
	var subnets []SubnetRow
	if err := s.db.NewSelect().Model(&subnets).Where("site_id = ?", site.ID).Order("id ASC").Scan(ctx); err != nil {
		return ContainerSiteConfig{}, fmt.Errorf("resolve container subnet: %w", err)
	}
	if len(subnets) != 1 || subnets[0].VPCID != vpcs[0].ID {
		return ContainerSiteConfig{}, fmt.Errorf("container site %s requires exactly one subnet in vpc %s", site.ID, vpcs[0].ID)
	}
	var instanceTypes []InstanceTypeRow
	if err := s.db.NewSelect().Model(&instanceTypes).Where("site_id = ?", site.ID).Order("id ASC").Scan(ctx); err != nil {
		return ContainerSiteConfig{}, fmt.Errorf("resolve container instance type: %w", err)
	}
	if len(instanceTypes) != 1 {
		return ContainerSiteConfig{}, fmt.Errorf("container site %s requires exactly one instance type, found %d", site.ID, len(instanceTypes))
	}
	var groups []SSHKeyGroupRow
	if err := s.db.NewSelect().Model(&groups).Order("id ASC").Scan(ctx); err != nil {
		return ContainerSiteConfig{}, fmt.Errorf("resolve container ssh key group: %w", err)
	}
	containerGroups := make([]SSHKeyGroupRow, 0, len(groups))
	for _, group := range groups {
		siteIDs, decodeErr := group.SiteIDs()
		if decodeErr != nil {
			return ContainerSiteConfig{}, fmt.Errorf("decode ssh key group %s site IDs: %w", group.ID, decodeErr)
		}
		if slices.Contains(siteIDs, site.ID) {
			containerGroups = append(containerGroups, group)
		}
	}
	groups = containerGroups
	if len(groups) != 1 {
		return ContainerSiteConfig{}, fmt.Errorf("container site %s requires exactly one ssh key group, found %d", site.ID, len(groups))
	}

	return ContainerSiteConfig{
		OrgID:          tenant.OrgID,
		TenantID:       tenant.ID,
		SiteID:         site.ID,
		VPCID:          vpcs[0].ID,
		SubnetID:       subnets[0].ID,
		InstanceTypeID: instanceTypes[0].ID,
		SSHKeyGroupID:  groups[0].ID,
	}, nil
}

func (s *Store) ListInstanceTypes(ctx context.Context, siteID string, page Page) ([]InstanceTypeRow, error) {
	page = normalizePage(page)
	var rows []InstanceTypeRow
	if err := s.db.NewSelect().
		Model(&rows).
		Where("site_id = ?", siteID).
		Order("id ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list instance types: %w", err)
	}
	return rows, nil
}

func (s *Store) CountActiveInstancesByType(ctx context.Context, siteID string) (map[string]int32, error) {
	type row struct {
		InstanceTypeID string `bun:"instance_type_id"`
		Cnt            int32  `bun:"cnt"`
	}

	var rows []row
	if err := s.db.NewSelect().
		TableExpr("instances AS instance").
		ColumnExpr("instance.instance_type_id").
		ColumnExpr("COUNT(*) AS cnt").
		Join("LEFT JOIN vms AS vm ON vm.instance_id = instance.id").
		Where("instance.site_id = ?", siteID).
		WhereGroup(" AND ", func(query *bun.SelectQuery) *bun.SelectQuery {
			return query.
				WhereOr("instance.backend = ?", SiteBackendContainer).
				WhereOr("(instance.backend = ? AND vm.desired_state = ? AND vm.observed_state != ?)",
					SiteBackendVM, DesiredStatePresent, ObservedStateDeleted)
		}).
		Group("instance.instance_type_id").
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failed counting active instances: %w", err)
	}

	out := make(map[string]int32, len(rows))
	for _, r := range rows {
		out[r.InstanceTypeID] = r.Cnt
	}
	return out, nil
}
