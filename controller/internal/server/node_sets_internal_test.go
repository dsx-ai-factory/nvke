// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestAPINicoNodeSetProviderFromDB_NVLinkLogicalPartition(t *testing.T) {
	t.Parallel()
	partitionID := uuid.New()

	got := apiNicoNodeSetProviderFromDB(&db.NicoNodePoolProvider{Release: "os-v1", NvLinkLogicalPartitionID: &partitionID})
	require.NotNil(t, got.NvLinkLogicalPartitionID)
	assert.Equal(t, partitionID, *got.NvLinkLogicalPartitionID)

	got = apiNicoNodeSetProviderFromDB(&db.NicoNodePoolProvider{Release: "os-v1"})
	assert.Nil(t, got.NvLinkLogicalPartitionID)
}
