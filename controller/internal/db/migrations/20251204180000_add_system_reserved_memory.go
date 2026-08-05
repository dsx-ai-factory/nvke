// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() {
	migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		// Add system_reserved_memory column to node_pools table
		if _, err := db.NewAddColumn().
			Table("node_pools").
			ColumnExpr("system_reserved_memory text NOT NULL DEFAULT '16Gi'").
			Exec(ctx); err != nil {
			return err
		}

		// Add system_reserved_memory column to node_sets table
		if _, err := db.NewAddColumn().
			Table("node_sets").
			ColumnExpr("system_reserved_memory text NOT NULL DEFAULT '16Gi'").
			Exec(ctx); err != nil {
			return err
		}

		// Backfill node_pools based on config_profile
		configProfileMemoryMap := map[string]string{
			"lepton-cp-v1":                      "16Gi",
			"lepton-infra-v1":                   "16Gi",
			"lepton-monitoring-v1":              "16Gi",
			"lepton-w-L40-graphics-v1":          "124Gi",
			"nvcf-cp-ovn-v1":                    "128Gi",
			"nvcf-cp-v1":                        "128Gi",
			"nvcf-cp-v2":                        "128Gi",
			"nvcf-cp-v3":                        "32Gi",
			"nvcf-monitoring-ovn-v1":            "128Gi",
			"nvcf-monitoring-v1":                "128Gi",
			"nvcf-monitoring-v2":                "128Gi",
			"nvcf-monitoring-v3":                "32Gi",
			"nvcf-w-GB200-v1":                   "128Gi",
			"nvcf-w-kata-L40-compute-v1":        "124Gi",
			"nvcf-w-kata-L40-graphics-v1":       "124Gi",
			"nvcf-w-kata-L40S-compute-v1":       "124Gi",
			"nvcf-w-kata-L40S-graphics-v1":      "124Gi",
			"nvcf-w-L40-compute-v1":             "128Gi",
			"nvcf-w-L40-ovn-v1":                 "128Gi",
			"nvcf-w-L40-v2":                     "32Gi",
			"nvcf-w-L40S-compute-v1":            "128Gi",
			"nvcf-w-L40S-v2":                    "128Gi",
			"nvmesh-cp-v1":                      "256Gi",
			"nvmesh-w-v1":                       "256Gi",
			"osmo-cp-v2":                        "16Gi",
			"osmo-infra-v2":                     "16Gi",
			"osmo-monitoring-v2":                "16Gi",
			"osmo-worker-L40S-v2":               "16Gi",
			"osmo-worker-v2":                    "16Gi",
			"vmaas-cp-monitoring-ovn-v1":        "128Gi",
			"vmaas-w-kata-L40S-ovn-graphics-v1": "124Gi",
		}

		// Update node_pools with correct values
		for configProfile, memValue := range configProfileMemoryMap {
			if _, err := db.NewUpdate().
				Table("node_pools").
				Set("system_reserved_memory = ?", memValue).
				Where("config_profile = ?", configProfile).
				Exec(ctx); err != nil {
				return err
			}
		}

		// Backfill node_sets based on config_profile
		for configProfile, memValue := range configProfileMemoryMap {
			if _, err := db.NewUpdate().
				Table("node_sets").
				Set("system_reserved_memory = ?", memValue).
				Where("config_profile = ?", configProfile).
				Exec(ctx); err != nil {
				return err
			}
		}

		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		return fmt.Errorf("down migration not implemented")
	})
}
