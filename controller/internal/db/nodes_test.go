// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package db_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
)

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
