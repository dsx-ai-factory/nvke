// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
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
		ID:                     uuid.New(),
		Name:                   "worker-1",
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
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

	// Re-fetch to verify the timestamp was persisted, not just changed in memory.
	persisted, err := database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.True(t, persisted.UpdatedAt.After(insertedAt),
		"UpdatedAt should advance on update: before=%s after=%s", insertedAt.Format(time.RFC3339Nano), persisted.UpdatedAt.Format(time.RFC3339Nano))
}

func TestUnit_UpdateNodePreservesSecurityProfile(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Node)(nil))
	database := db.NewDB(bunDB)
	node := db.Node{
		ID:                     uuid.New(),
		Name:                   "worker-1",
		SpiffeEnabled:          true,
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
	}
	created, err := database.CreateNode(ctx, &node)
	require.NoError(t, err)

	created.SpireAttestationMethod = db.SpireAttestationMethodDSXIMDS
	created.NodeAgentExternalMTLSEnabled = true
	created.NodeAgentTLSJWTEnabled = true
	updated, err := database.UpdateNode(ctx, &created)
	require.NoError(t, err)
	require.Equal(t, db.SpireAttestationMethodTPM, updated.SpireAttestationMethod)
	require.False(t, updated.NodeAgentExternalMTLSEnabled)
	require.False(t, updated.NodeAgentTLSJWTEnabled)

	stored, err := database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	require.Equal(t, updated.SpireAttestationMethod, stored.SpireAttestationMethod)
}

func TestUnit_NodeValidateSecurityProfile(t *testing.T) {
	t.Parallel()
	type parameters struct {
		node    db.Node
		wantErr string
	}
	tests := map[string]parameters{
		"TPM SPIFFE":         {node: db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM}},
		"TPM without SPIFFE": {node: db.Node{SpireAttestationMethod: db.SpireAttestationMethodTPM}},
		"DSX IMDS with external mTLS and TLS JWT": {node: db.Node{
			SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS,
			NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true,
		}},
		"missing attestation method": {node: db.Node{}, wantErr: "invalid SPIRE attestation method"},
		"DSX IMDS without SPIFFE":    {node: db.Node{SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS}, wantErr: "requires SPIFFE"},
		"TLS JWT without TLS": {node: db.Node{
			SpireAttestationMethod: db.SpireAttestationMethodTPM, NodeAgentTLSJWTEnabled: true,
		}, wantErr: "requires a TLS transport"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := params.node.ValidateSecurityProfile()
			if params.wantErr != "" {
				require.ErrorContains(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestUnit_NodeGetSpiffeID(t *testing.T) {
	t.Parallel()
	const nodeID = "45f59791-87df-4e85-9430-e565166ad020"

	tests := map[string]struct {
		node db.Node
		want string
	}{
		"legacy SPIFFE identity": {
			node: db.Node{ID: uuid.MustParse(nodeID)},
			want: "spiffe://example.org/host/" + nodeID,
		},
		"canonical DSX exchange identity": {
			node: db.Node{ID: uuid.MustParse(nodeID), NodeAgentTLSJWTEnabled: true},
			want: "spiffe://example.org/tenant/tenant-a/service/dsx/bm/" + nodeID,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.want, test.node.GetSpiffeID("example.org", "tenant-a", "dsx"))
		})
	}
}

func TestNode_NicoProvider(t *testing.T) {
	instanceTypeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	type parameters struct {
		node    db.Node
		want    db.NicoNodeProvider
		wantErr error
	}

	tests := map[string]parameters{
		"returns payload": {
			node: db.Node{
				ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
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

func TestNodeProviderSetters(t *testing.T) {
	instanceTypeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	node := db.Node{}

	node.SetNico(db.NicoNodeProvider{
		ForgeInstanceID: "forge-instance",
		ForgeMachineID:  "forge-machine",
		InstanceTypeID:  instanceTypeID,
	})

	require.Equal(t, "forge-instance", node.Nico.ForgeInstanceID)
	require.Equal(t, "forge-machine", node.Nico.ForgeMachineID)
	require.Equal(t, instanceTypeID, node.Nico.InstanceTypeID)
	require.Equal(t, instanceTypeID, node.NicoInstanceTypeID())
	require.Equal(t, "forge-instance", node.NicoForgeInstanceID())
	require.Equal(t, "forge-machine", node.NicoForgeMachineID())

	nextInstanceTypeID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	node.SetNico(db.NicoNodeProvider{
		ForgeInstanceID: "next-instance",
		ForgeMachineID:  "next-machine",
		InstanceTypeID:  nextInstanceTypeID,
	})
	require.Equal(t, "next-instance", node.Nico.ForgeInstanceID)
	require.Equal(t, "next-machine", node.Nico.ForgeMachineID)
	require.Equal(t, nextInstanceTypeID, node.Nico.InstanceTypeID)
}

// TestUnit_NodeConditionsEqualIgnoresTimeRepresentation guards against a
// regression where the tenant node-health reconciler compared condition sets
// with ==, which also compares each LastTransitionTime's monotonic reading and
// *Location pointer. A condition read back from JSONB (UTC) and the same
// instant taken from a metav1.Time (local, see its UnmarshalJSON) were never
// == equal, so the reconciler wrote an identical row on every pass — a
// self-retriggering UPDATE loop that produced tens of gigabytes of WAL a day
// against a table of a few dozen rows.
func TestUnit_NodeConditionsEqualIgnoresTimeRepresentation(t *testing.T) {
	instant := time.Date(2026, 8, 17, 21, 21, 14, 0, time.UTC)
	fromDB := db.NodeConditions{
		KubernetesReady: db.Condition{
			Status:             db.ConditionStatusTrue,
			Message:            "kubelet is posting ready status",
			LastTransitionTime: instant,
		},
	}
	fromKube := fromDB
	fromKube.KubernetesReady.LastTransitionTime = instant.In(time.FixedZone("MDT", -6*60*60))

	// The test is only meaningful while the two representations really are
	// structurally different — that difference is what == tripped over.
	require.NotEqual(t, fromDB.KubernetesReady.LastTransitionTime, fromKube.KubernetesReady.LastTransitionTime)
	require.True(t, fromDB.Equal(fromKube))

	changed := fromDB
	changed.KubernetesReady.Status = db.ConditionStatusFalse
	require.False(t, fromDB.Equal(changed))
}

// TestUnit_NodeConditionsAllCoversEveryField fails when a condition is added to
// NodeConditions without being added to All. Everything that walks the set —
// Equal, and the reconciler's report of which condition changed — reads All, so
// a missing entry silently drops real changes to that condition on the floor.
func TestUnit_NodeConditionsAllCoversEveryField(t *testing.T) {
	conditionsType := reflect.TypeFor[db.NodeConditions]()
	require.Len(t, db.NodeConditions{}.All(), conditionsType.NumField(),
		"NodeConditions.All does not list every condition")

	for i := range conditionsType.NumField() {
		field := conditionsType.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			require.Equal(t, reflect.TypeFor[db.Condition](), field.Type,
				"NodeConditions.All assumes every field is a db.Condition")

			var base, modified db.NodeConditions
			reflect.ValueOf(&modified).Elem().Field(i).Set(reflect.ValueOf(db.Condition{
				Status:             db.ConditionStatusTrue,
				Message:            "changed",
				LastTransitionTime: time.Date(2026, 8, 17, 21, 21, 14, 0, time.UTC),
			}))

			require.False(t, base.Equal(modified), "%s is not compared by NodeConditions.Equal", field.Name)

			// The name All reports is the one that reaches logs, so a
			// placeholder or a duplicated line is worth catching here.
			require.NotEmpty(t, modified.All()[i].Name)
			require.Equal(t, db.ConditionStatusTrue, modified.All()[i].Condition.Status,
				"All entry %d does not correspond to struct field %s", i, field.Name)
		})
	}
}
