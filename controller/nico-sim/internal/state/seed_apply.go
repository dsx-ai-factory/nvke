// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/NVIDIA/nke/controller/nico-sim/internal/seed"
)

func (s *Store) ApplySeed(ctx context.Context, doc seed.Document) error {
	if err := doc.NormalizeAndValidate(); err != nil {
		return err
	}
	now := s.nowFn()
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, tenant := range doc.Tenants {
			row := TenantRow{
				ID:          tenant.ID,
				OrgID:       tenant.OrgID,
				DisplayName: tenant.DisplayName,
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		for _, site := range doc.Sites {
			row := SiteRow{
				ID:                        site.ID,
				Name:                      site.Name,
				TenantID:                  site.TenantID,
				Backend:                   SiteBackend(site.Backend),
				CapabilitiesNativeNetwork: site.Capabilities.NativeNetworking,
				CreatedAt:                 now,
				UpdatedAt:                 now,
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		for _, vpc := range doc.VPCs {
			row := VPCRow{
				ID:       vpc.ID,
				Name:     vpc.Name,
				TenantID: vpc.TenantID,
				SiteID:   vpc.SiteID,
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		for _, subnet := range doc.Subnets {
			row := SubnetRow{
				ID:           subnet.ID,
				Name:         subnet.Name,
				VPCID:        subnet.VPCID,
				SiteID:       subnet.SiteID,
				CIDR:         subnet.CIDR,
				PrefixLength: subnet.PrefixLength,
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		for _, pfx := range doc.VPCPrefixes {
			row := VPCPrefixRow{
				ID:           pfx.ID,
				Name:         pfx.Name,
				VPCID:        pfx.VPCID,
				SiteID:       pfx.SiteID,
				PrefixLength: pfx.PrefixLength,
				Prefix:       pfx.Prefix,
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		for _, it := range doc.InstanceTypes {
			row := InstanceTypeRow{
				ID:        it.ID,
				Name:      it.Name,
				SiteID:    it.SiteID,
				GPUCount:  it.GPUCount,
				CPUCount:  it.CPUCount,
				MemoryGiB: it.MemoryGiB,
				DiskGiB:   it.DiskGiB,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		for _, group := range doc.SSHKeyGroups {
			siteIDsJSON, err := json.Marshal(group.SiteIDs)
			if err != nil {
				return fmt.Errorf("failed to marshal ssh_key_group site ids: %w", err)
			}
			row := SSHKeyGroupRow{
				ID:          group.ID,
				Name:        group.Name,
				SiteIDsJSON: string(siteIDsJSON),
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		for _, block := range doc.IPBlocks {
			row := IPBlockRow{
				ID:     block.ID,
				Name:   block.Name,
				SiteID: block.SiteID,
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		for _, part := range doc.InfiniBandPartitions {
			row := InfiniBandPartitionRow{
				ID:     part.ID,
				Name:   part.Name,
				SiteID: part.SiteID,
			}
			if err := upsertTx(ctx, tx, &row, "id"); err != nil {
				return err
			}
		}

		return nil
	})
}

type bunTx interface {
	NewInsert() *bun.InsertQuery
}

func upsertTx(ctx context.Context, tx bunTx, model interface{}, keyColumn string) error {
	_, err := tx.NewInsert().
		Model(model).
		On("CONFLICT (" + keyColumn + ") DO UPDATE SET " + keyColumn + " = EXCLUDED." + keyColumn).
		Returning("NULL").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to upsert row: %w", err)
	}
	return nil
}
