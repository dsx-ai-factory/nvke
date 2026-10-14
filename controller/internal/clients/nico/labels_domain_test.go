// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// Instance labels are write-only here (CAPNICo fills in the rest), so a
// configured domain replaces the legacy one outright rather than adding to it.
func TestBuildInstanceLabelsConfiguredDomain(t *testing.T) {
	nodeID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	clusterID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	nodePoolID := uuid.MustParse("99999999-8888-7777-6666-555555555555")

	labels := BuildInstanceLabels(keys.New("nvke.example.com", ""), db.Node{ID: nodeID}, db.Cluster{ID: clusterID}, nodePoolID)

	assert.Equal(t, map[string]string{
		"nvke.example.com/node-id":      nodeID.String(),
		"nvke.example.com/cluster-id":   clusterID.String(),
		"nvke.example.com/node-pool-id": nodePoolID.String(),
	}, labels)
}
