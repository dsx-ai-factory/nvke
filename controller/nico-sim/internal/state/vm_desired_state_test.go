// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/seed"
)

// newVMInstanceInState opens a store holding one VM instance at the given
// desired and observed state.
func newVMInstanceInState(t *testing.T, desired, observed string) (*Store, string) {
	t.Helper()
	ctx := context.Background()

	store, err := Open(filepath.Join(t.TempDir(), "nico-sim.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.NoError(t, store.ApplySeed(ctx, seed.Document{
		Tenants:       []seed.Tenant{{ID: "tenant-1", OrgID: "org-1", DisplayName: "tenant"}},
		Sites:         []seed.Site{{ID: "site-1", Name: "site", TenantID: "tenant-1", Backend: seed.SiteBackendVM}},
		VPCs:          []seed.VPC{{ID: "vpc-1", Name: "vpc", TenantID: "tenant-1", SiteID: "site-1"}},
		Subnets:       []seed.Subnet{{ID: "subnet-1", Name: "subnet", VPCID: "vpc-1", SiteID: "site-1", CIDR: "10.1.0.0/24", PrefixLength: 24}},
		InstanceTypes: []seed.InstanceType{{ID: "itype-1", Name: "itype", SiteID: "site-1", CPUCount: 1, MemoryGiB: 1}},
	}))

	subnet := "subnet-1"
	instance, _, err := store.CreateInstance(ctx, CreateInstanceInput{
		Backend:        SiteBackendVM,
		SiteID:         "site-1",
		Name:           "node-1",
		TenantID:       "tenant-1",
		InstanceTypeID: "itype-1",
		VPCID:          "vpc-1",
		MachineID:      "machine-1",
		Interfaces:     []InterfaceRequest{{SubnetID: &subnet}},
		VM:             &VMState{DesiredState: desired, ObservedState: observed},
	})
	require.NoError(t, err)
	return store, instance.ID
}

func isReconcileCandidate(t *testing.T, store *Store, id string) bool {
	t.Helper()
	candidates, err := store.ListVMReconcileCandidates(context.Background())
	require.NoError(t, err)
	for _, candidate := range candidates {
		if candidate == id {
			return true
		}
	}
	return false
}

func TestSetVMDesiredStateStop(t *testing.T) {
	tests := []struct {
		name          string
		observed      string
		wantConflict  bool
		wantCandidate bool
	}{
		{"running is accepted and picked up", ObservedStateRunning, false, true},
		{"rebooting is accepted and picked up", ObservedStateRebooting, false, true},
		{"ready is accepted and picked up", ObservedStateReady, false, true},
		{"already stopped is accepted and left alone", ObservedStateStopped, false, false},
		{"new is rejected", ObservedStateNew, true, false},
		{"provisioning is rejected", ObservedStateProvisioning, true, false},
		{"error is rejected", ObservedStateError, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, id := newVMInstanceInState(t, DesiredStatePresent, tt.observed)

			found, err := store.SetVMDesiredState(ctx, id, DesiredStateStopped)

			got, getErr := store.GetInstance(ctx, id)
			require.NoError(t, getErr)
			if tt.wantConflict {
				require.ErrorIs(t, err, ErrConflict)
				require.False(t, found)
				require.Equal(t, DesiredStatePresent, got.VM.DesiredState)
				return
			}
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, DesiredStateStopped, got.VM.DesiredState)
			require.Equal(t, tt.wantCandidate, isReconcileCandidate(t, store, id))
		})
	}
}

func TestSetVMDesiredStateStart(t *testing.T) {
	tests := []struct {
		name         string
		observed     string
		wantConflict bool
	}{
		{"stopped is accepted and picked up", ObservedStateStopped, false},
		{"running is accepted", ObservedStateRunning, false},
		{"new is accepted", ObservedStateNew, false},
		{"provisioning is accepted", ObservedStateProvisioning, false},
		{"error is rejected", ObservedStateError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, id := newVMInstanceInState(t, DesiredStateStopped, tt.observed)

			found, err := store.SetVMDesiredState(ctx, id, DesiredStatePresent)

			got, getErr := store.GetInstance(ctx, id)
			require.NoError(t, getErr)
			if tt.wantConflict {
				require.ErrorIs(t, err, ErrConflict)
				require.Equal(t, DesiredStateStopped, got.VM.DesiredState)
				return
			}
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, DesiredStatePresent, got.VM.DesiredState)
			require.True(t, isReconcileCandidate(t, store, id))
		})
	}
}

func TestSetVMDesiredStateUnknownInstance(t *testing.T) {
	store, _ := newVMInstanceInState(t, DesiredStatePresent, ObservedStateReady)

	found, err := store.SetVMDesiredState(context.Background(), "missing", DesiredStateStopped)

	require.NoError(t, err)
	require.False(t, found)
}
