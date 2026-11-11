// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package migrations

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestNodeFailureDomainFallbackColumnsRoundTrip(t *testing.T) {
	ctx := context.Background()
	bunDB := newMigratedDatabase(ctx, t)

	cluster := db.Cluster{ID: uuid.New(), Name: "fd-fallback", KubernetesVersion: "1.34.2", ProviderType: db.ProviderTypeNico}
	_, err := bunDB.NewInsert().Model(&cluster).Exec(ctx)
	require.NoError(t, err)

	since := time.Now().UTC().Truncate(time.Microsecond)
	node := db.Node{
		ID: uuid.New(), ClusterID: cluster.ID, ControlPlane: true, Name: "cp-0", FailureDomain: "fd-b",
		SpireAttestationMethod:        db.SpireAttestationMethodTPM,
		FailureDomainUnavailableSince: &since,
		FailureDomainsTried:           []string{"fd-a", "fd-c"},
	}
	_, err = bunDB.NewInsert().Model(&node).Exec(ctx)
	require.NoError(t, err)

	var got db.Node
	require.NoError(t, bunDB.NewSelect().Model(&got).Where("id = ?", node.ID).Scan(ctx))
	assert.Equal(t, []string{"fd-a", "fd-c"}, got.FailureDomainsTried)
	require.NotNil(t, got.FailureDomainUnavailableSince)
	assert.True(t, since.Equal(*got.FailureDomainUnavailableSince))

	legacy := db.Node{ID: uuid.New(), ClusterID: cluster.ID, ControlPlane: true, Name: "cp-1", SpireAttestationMethod: db.SpireAttestationMethodTPM}
	_, err = bunDB.NewInsert().Model(&legacy).Exec(ctx)
	require.NoError(t, err)
	var gotLegacy db.Node
	require.NoError(t, bunDB.NewSelect().Model(&gotLegacy).Where("id = ?", legacy.ID).Scan(ctx))
	assert.Empty(t, gotLegacy.FailureDomainsTried)
	assert.Nil(t, gotLegacy.FailureDomainUnavailableSince)
}
