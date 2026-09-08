// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package state provides SQLite-backed persistence for nico-vms.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func (s *Store) ListAllSubnets(ctx context.Context) ([]SubnetRow, error) {
	var rows []SubnetRow
	if err := s.db.NewSelect().
		Model(&rows).
		Order("id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list all subnets: %w", err)
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

func (s *Store) ListAllVPCPrefixes(ctx context.Context) ([]VPCPrefixRow, error) {
	var rows []VPCPrefixRow
	if err := s.db.NewSelect().
		Model(&rows).
		Order("id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list all vpc prefixes: %w", err)
	}
	return rows, nil
}

func (s *Store) ListSSHKeyGroups(ctx context.Context, siteID string, page Page) ([]SSHKeyGroupRow, error) {
	page = normalizePage(page)
	var rows []SSHKeyGroupRow
	if err := s.db.NewSelect().
		Model(&rows).
		Order("id ASC").
		Limit(page.Size).
		Offset((page.Number - 1) * page.Size).
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
	return filtered, nil
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
		Table("instances").
		Column("instance_type_id").
		ColumnExpr("COUNT(*) AS cnt").
		Where("site_id = ?", siteID).
		Where("desired_state = ?", DesiredStatePresent).
		Where("observed_state != ?", ObservedStateDeleted).
		Group("instance_type_id").
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failed counting active instances: %w", err)
	}

	out := make(map[string]int32, len(rows))
	for _, r := range rows {
		out[r.InstanceTypeID] = r.Cnt
	}
	return out, nil
}
