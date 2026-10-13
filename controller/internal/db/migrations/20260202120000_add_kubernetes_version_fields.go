// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/utils"
)

//nolint:gochecknoinits // this is the typical way to register bun migrations
func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck // Rollback is safe to ignore: it's a no-op after successful Commit and we handle tx errors at Commit

		_, err = tx.NewAddColumn().
			Table("node_pools").
			ColumnExpr("kubernetes_version text NOT NULL DEFAULT ''").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add node_pools.kubernetes_version column: %w", err)
		}

		_, err = tx.NewAddColumn().
			Table("node_sets").
			ColumnExpr("kubernetes_version text NOT NULL DEFAULT ''").
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to add node_sets.kubernetes_version column: %w", err)
		}

		// Set the KubernetesVersion for all existing node pools and node sets.
		// By getting it from the OSProfile metadata or falling back to the clusters KubernetesVersion.
		nodePools := []NodePool20260202120000{}
		err = tx.NewSelect().
			Model(&nodePools).
			Scan(ctx, &nodePools)
		if err != nil {
			return fmt.Errorf("failed to get node pools: %w", err)
		}

		// The metadata index is read only to backfill the rows selected above,
		// so an empty database needs no OS profiles tree at all. Loading it
		// unconditionally made a profiles bundle a hard prerequisite for
		// starting the controller against a fresh database, which is what `nvkectl bootstrap
		// site` does: it crash-looped here on a missing osProfiles/metadata.yaml
		// before it had created a single cluster.
		//
		// A database that does have rows to backfill still fails loudly, because
		// there the file is the only source of their Kubernetes versions and
		// falling back silently would write wrong ones.
		var metadataIdx *utils.MetadataIndex
		if len(nodePools) > 0 {
			cfg, _, cfgErr := controllerconfig.LoadStartupConfig()
			if cfgErr != nil {
				return fmt.Errorf("failed to parse config: %w", cfgErr)
			}
			metadataIdx, err = utils.LoadOSProfileMetadata(cfg.ProfilesDir)
			if err != nil {
				return fmt.Errorf("failed to load OSProfile metadata: %w", err)
			}
		}

		for _, nodePool := range nodePools {
			cluster := Cluster20260202120000{}
			err = tx.NewSelect().
				Model(&cluster).
				Where("id = ?", nodePool.ClusterID).
				Scan(ctx)
			if err != nil {
				return fmt.Errorf("failed to get cluster: %w", err)
			}

			nodePool.KubernetesVersion = getKubernetesVersionFromOSProfileOrDefault(nodePool.OSProfile, metadataIdx, cluster.KubernetesVersion)
			_, err = tx.NewUpdate().
				Model((*NodePool20260202120000)(nil)).
				Set("kubernetes_version = ?", nodePool.KubernetesVersion).
				Where("id = ?", nodePool.ID).
				Exec(ctx)
			if err != nil {
				return fmt.Errorf("failed to update node pool: %w", err)
			}

			nodeSets := []NodeSet20260202120000{}
			err = tx.NewSelect().
				Model(&nodeSets).
				Where("node_pool_id = ?", nodePool.ID).
				Scan(ctx, &nodeSets)
			if err != nil {
				return fmt.Errorf("failed to get node sets: %w", err)
			}

			for _, nodeSet := range nodeSets {
				nodeSet.KubernetesVersion = getKubernetesVersionFromOSProfileOrDefault(nodeSet.OSProfile, metadataIdx, cluster.KubernetesVersion)
				_, err = tx.NewUpdate().
					Model((*NodeSet20260202120000)(nil)).
					Set("kubernetes_version = ?", nodeSet.KubernetesVersion).
					Where("id = ?", nodeSet.ID).
					Exec(ctx)
				if err != nil {
					return fmt.Errorf("failed to update node set: %w", err)
				}
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		_, err := db.NewDropColumn().
			Table("node_sets").
			Column("kubernetes_version").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop node_sets.kubernetes_version column: %w", err)
		}

		_, err = db.NewDropColumn().
			Table("node_pools").
			Column("kubernetes_version").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop node_pools.kubernetes_version column: %w", err)
		}

		return nil
	})
}

func getKubernetesVersionFromOSProfileOrDefault(osProfile string, metadataIdx *utils.MetadataIndex, clusterKubernetesVersion string) string {
	// nil when there was nothing to backfill, so the index was never loaded.
	if metadataIdx == nil {
		return clusterKubernetesVersion
	}

	currentMeta, ok := metadataIdx.GetOSProfileMetadata(osProfile, true)
	if !ok {
		return clusterKubernetesVersion
	}

	if currentMeta.KubernetesVersion == "" {
		return clusterKubernetesVersion
	}

	return currentMeta.KubernetesVersion
}

type Cluster20260202120000 struct {
	bun.BaseModel `bun:"table:clusters"`

	ID                uuid.UUID `bun:"type:uuid,pk,default:gen_random_uuid()"`
	KubernetesVersion string    `bun:"type:text,notnull"`
}

type NodePool20260202120000 struct {
	bun.BaseModel `bun:"table:node_pools"`

	ID                uuid.UUID `bun:"type:uuid,pk"`
	ClusterID         uuid.UUID `bun:"type:uuid,notnull"`
	OSProfile         string    `bun:"type:text,notnull"`
	KubernetesVersion string    `bun:"type:text,notnull,default:''"`
}

type NodeSet20260202120000 struct {
	bun.BaseModel `bun:"table:node_sets"`

	ID                uuid.UUID `bun:"type:uuid,pk"`
	NodePoolID        uuid.UUID `bun:"type:uuid,notnull"`
	OSProfile         string    `bun:"type:text,notnull"`
	KubernetesVersion string    `bun:"type:text,notnull,default:''"`
}
