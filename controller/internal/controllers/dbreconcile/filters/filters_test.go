// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package filters

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestUnit_NodeMatchesClusterID(t *testing.T) {
	clusterID := uuid.New()
	filter := Node{ClusterID: &clusterID}

	require.True(t, filter.Matches(db.Node{ClusterID: clusterID}))
	require.False(t, filter.Matches(db.Node{ClusterID: uuid.New()}))
}
