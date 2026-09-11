// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// TestUpdateNode_BumpsUpdatedAt guards against a regression where Node's
// BeforeUpdate hook silently stopped firing because its signature didn't
// match bun.BeforeUpdateHook (BeforeUpdate(context.Context, *bun.UpdateQuery)
// error), so Bun never invoked it and UpdatedAt stayed stuck at its
// insert-time value on every subsequent write.
func TestUpdateNode_BumpsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Node)(nil))
	database := db.NewDB(bunDB)

	node := db.Node{
		ID:   uuid.New(),
		Name: "worker-1",
	}
	created, err := database.CreateNode(ctx, &node)
	require.NoError(t, err)
	insertedAt := created.UpdatedAt

	// SQLite (used by the test harness) stores timestamps with second
	// precision, so sleep past a full second boundary to make the change
	// unambiguous rather than relying on sub-second resolution.
	time.Sleep(1100 * time.Millisecond)

	created.AgentVersion = "v1.2.3"
	_, err = database.UpdateNode(ctx, &created)
	require.NoError(t, err)

	// UpdateNode's return value doesn't round-trip UpdatedAt from the DB (no
	// RETURNING clause), so re-fetch to see what was actually persisted.
	persisted, err := database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.True(t, persisted.UpdatedAt.After(insertedAt),
		"UpdatedAt should advance on update: before=%s after=%s", insertedAt.Format(time.RFC3339Nano), persisted.UpdatedAt.Format(time.RFC3339Nano))
}

func TestNode_NicoProvider(t *testing.T) {
	instanceTypeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	type parameters struct {
		node    db.Node
		want    db.NicoNodeProvider
		wantErr error
	}

	tests := map[string]parameters{
		"returns payload without legacy fallback": {
			node: db.Node{
				ID:              uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
				ForgeInstanceID: "legacy-instance",
				ForgeMachineID:  "legacy-machine",
				InstanceTypeID:  uuid.MustParse("22222222-2222-2222-2222-222222222222"),
				Nico: &db.NicoNodeProvider{
					ForgeInstanceID: "payload-instance",
					ForgeMachineID:  "payload-machine",
					InstanceTypeID:  instanceTypeID,
				},
			},
			want: db.NicoNodeProvider{
				ForgeInstanceID: "payload-instance",
				ForgeMachineID:  "payload-machine",
				InstanceTypeID:  instanceTypeID,
			},
		},
		"missing payload": {
			node:    db.Node{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
			wantErr: db.ErrMissingProviderPayload,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			provider, err := params.node.NicoProvider()

			if params.wantErr != nil {
				require.ErrorIs(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, params.want, provider)
		})
	}
}

func TestNodeProviderSettersAndSync(t *testing.T) {
	instanceTypeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	node := db.Node{
		ForgeInstanceID: "forge-instance",
		ForgeMachineID:  "forge-machine",
		InstanceTypeID:  instanceTypeID,
	}

	node.SyncProviderFields()

	require.Equal(t, "forge-instance", node.Nico.ForgeInstanceID)
	require.Equal(t, "forge-machine", node.Nico.ForgeMachineID)
	require.Equal(t, instanceTypeID, node.Nico.InstanceTypeID)

	nextInstanceTypeID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	node.SetNico(db.NicoNodeProvider{
		ForgeInstanceID: "next-instance",
		ForgeMachineID:  "next-machine",
		InstanceTypeID:  nextInstanceTypeID,
	})
	require.Equal(t, "next-instance", node.ForgeInstanceID)
	require.Equal(t, "next-machine", node.ForgeMachineID)
	require.Equal(t, nextInstanceTypeID, node.InstanceTypeID)
}
