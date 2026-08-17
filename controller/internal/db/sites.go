// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrSiteIDAlreadyExists  = errors.New("site ID already exists")
	ErrVIPAlreadyAllocated  = errors.New("VIP already allocated")
	ErrClusterAlreadyHasVIP = errors.New("cluster already has a VIP allocation")
)

type Site struct {
	ID                 uuid.UUID  `bun:"type:uuid,pk"`
	VIPRanges          []string   `bun:"type:text[],notnull"`
	NextReconcileDue   *time.Time `bun:"type:timestamptz"`
	NextCertRenewalDue *time.Time `bun:"type:timestamptz"`
	CreatedAt          time.Time  `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type VIPAllocation struct {
	ID             uuid.UUID  `bun:"type:uuid,pk,default:gen_random_uuid()"`
	SiteID         uuid.UUID  `bun:"type:uuid,notnull"`
	VIP            string     `bun:"type:text,notnull"`
	AllocationType string     `bun:"type:text,notnull"`
	ClusterID      *uuid.UUID `bun:"type:uuid"`
	CreatedAt      time.Time  `bun:"type:timestamptz,notnull,default:current_timestamp"`
}

type SiteQueries interface {
	ListSites(ctx context.Context) ([]Site, error)
	GetSite(ctx context.Context, id uuid.UUID, lockForUpdate bool) (Site, error)
	RegisterSite(ctx context.Context, site *Site) (Site, error)

	ListSitesDueForReconcile(ctx context.Context, batchSize int) ([]Site, error)
	UpdateSiteReconcileDue(ctx context.Context, siteID uuid.UUID, nextDue time.Time) error
	UpdateSiteNextCertRenewalDue(ctx context.Context, siteID uuid.UUID, nextDue time.Time) error

	ListVIPAllocations(ctx context.Context, siteID *uuid.UUID) ([]VIPAllocation, error)
	GetVIPAllocation(ctx context.Context, id uuid.UUID) (VIPAllocation, error)
	CreateVIPAllocation(ctx context.Context, vipAllocation *VIPAllocation) (VIPAllocation, error)
	DeleteVIPAllocationForCluster(ctx context.Context, clusterID uuid.UUID) error
}

func (q *queriesImpl) ListSites(ctx context.Context) ([]Site, error) {
	var sites []Site

	err := q.db.NewSelect().
		Model(&sites).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list sites: %w", err)
	}

	return sites, nil
}

func (q *queriesImpl) GetSite(ctx context.Context, id uuid.UUID, lockForUpdate bool) (Site, error) {
	var site Site

	query := q.db.NewSelect().
		Model(&site).
		Where("id = ?", id)

	// If lockForUpdate is true, lock the site row for the duration of the transaction.
	if lockForUpdate {
		query = query.For("UPDATE")
	}

	if err := query.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Site{}, ErrNotFound
		}
		return Site{}, fmt.Errorf("failed to get site: %w", err)
	}

	return site, nil
}

func (q *queriesImpl) RegisterSite(ctx context.Context, site *Site) (Site, error) {
	_, err := q.db.NewInsert().Model(site).Exec(ctx)
	if err != nil {
		if isViolationOfConstraint(err, "sites_pkey") {
			return Site{}, ErrSiteIDAlreadyExists
		}
		return Site{}, err
	}

	return *site, nil
}

func (q *queriesImpl) ListSitesDueForReconcile(ctx context.Context, batchSize int) ([]Site, error) {
	var sites []Site

	err := q.db.NewSelect().
		Model(&sites).
		Where("next_reconcile_due IS NULL OR next_reconcile_due <= NOW()").
		For("UPDATE SKIP LOCKED").
		Order("next_reconcile_due ASC NULLS FIRST").
		Limit(batchSize).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list sites due for reconcile: %w", err)
	}

	return sites, nil
}

func (q *queriesImpl) UpdateSiteReconcileDue(ctx context.Context, siteID uuid.UUID, nextDue time.Time) error {
	_, err := q.db.NewUpdate().
		Model(&Site{}).
		Set("next_reconcile_due = ?", nextDue).
		Where("id = ?", siteID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update site reconcile due: %w", err)
	}

	return nil
}

func (q *queriesImpl) UpdateSiteNextCertRenewalDue(ctx context.Context, siteID uuid.UUID, nextDue time.Time) error {
	_, err := q.db.NewUpdate().
		Model(&Site{}).
		Set("next_cert_renewal_due = ?", nextDue).
		Where("id = ?", siteID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to update site next cert renewal due: %w", err)
	}

	return nil
}

func (q *queriesImpl) ListVIPAllocations(ctx context.Context, siteID *uuid.UUID) ([]VIPAllocation, error) {
	var vipAllocations []VIPAllocation
	query := q.db.NewSelect().
		Model(&vipAllocations).
		Order("created_at ASC")

	if siteID != nil {
		query = query.Where("site_id = ?", *siteID)
	}

	err := query.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list VIP allocations: %w", err)
	}
	return vipAllocations, nil
}

func (q *queriesImpl) GetVIPAllocation(ctx context.Context, id uuid.UUID) (VIPAllocation, error) {
	var vipAllocation VIPAllocation
	err := q.db.NewSelect().
		Model(&vipAllocation).
		Where("id = ?", id).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VIPAllocation{}, ErrNotFound
		}
		return VIPAllocation{}, fmt.Errorf("failed to get VIP allocation: %w", err)
	}
	return vipAllocation, nil
}

func (q *queriesImpl) CreateVIPAllocation(ctx context.Context, vipAllocation *VIPAllocation) (VIPAllocation, error) {
	_, err := q.db.NewInsert().Model(vipAllocation).Exec(ctx)
	if err != nil {
		if isViolationOfConstraint(err, "vip_allocations_site_vip_unique") {
			return VIPAllocation{}, ErrVIPAlreadyAllocated
		}
		if isViolationOfConstraint(err, "vip_allocations_cluster_unique") {
			return VIPAllocation{}, ErrClusterAlreadyHasVIP
		}
		return VIPAllocation{}, err
	}
	return *vipAllocation, nil
}

func (q *queriesImpl) DeleteVIPAllocationForCluster(ctx context.Context, clusterID uuid.UUID) error {
	_, err := q.db.NewDelete().Model(&VIPAllocation{}).Where("cluster_id = ?", clusterID).Exec(ctx)
	return err
}
