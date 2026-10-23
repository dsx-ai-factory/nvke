// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/seed"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm"
)

func TestReconcileStopStart(t *testing.T) {
	tests := []struct {
		name         string
		desired      string
		observed     string
		machineState vm.MachineState
		wantObserved string // "" means the instance must be gone
		wantMachine  vm.MachineState
		wantCandid   bool // whether the reconcile query picks the instance up
	}{
		{"stop a ready VM", state.DesiredStateStopped, state.ObservedStateReady, vm.MachineStateRunning, state.ObservedStateStopped, vm.MachineStateShutOff, true},
		{"stop a running VM", state.DesiredStateStopped, state.ObservedStateRunning, vm.MachineStateRunning, state.ObservedStateStopped, vm.MachineStateShutOff, true},
		{"stop does not re-provision a VM whose process is gone", state.DesiredStateStopped, state.ObservedStateRunning, vm.MachineStateShutOff, state.ObservedStateStopped, vm.MachineStateShutOff, true},
		{"already stopped is left alone", state.DesiredStateStopped, state.ObservedStateStopped, vm.MachineStateShutOff, state.ObservedStateStopped, vm.MachineStateShutOff, false},
		{"start a stopped VM", state.DesiredStatePresent, state.ObservedStateStopped, vm.MachineStateShutOff, state.ObservedStateRunning, vm.MachineStateRunning, true},
		{"delete from stopped", state.DesiredStateAbsent, state.ObservedStateStopped, vm.MachineStateShutOff, "", vm.MachineStateNotFound, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			e, id := newStopStartEngine(t, tt.desired, tt.observed, tt.machineState)

			candidates, err := e.store.ListVMReconcileCandidates(ctx)
			require.NoError(t, err)
			require.Equal(t, tt.wantCandid, len(candidates) == 1)

			require.NoError(t, e.reconcileInstance(ctx, id))

			got, err := e.store.GetInstance(ctx, id)
			if tt.wantObserved == "" {
				require.ErrorIs(t, err, state.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.wantObserved, got.VM.ObservedState)
			}
			machineState, err := e.runtime.GetState(ctx, "machine-1")
			require.NoError(t, err)
			require.Equal(t, tt.wantMachine, machineState)
		})
	}
}

func newStopStartEngine(t *testing.T, desired, observed string, machineState vm.MachineState) (*Engine, string) {
	t.Helper()
	ctx := context.Background()

	store, err := state.Open(filepath.Join(t.TempDir(), "nico-sim.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	doc := seed.Document{
		Tenants:       []seed.Tenant{{ID: "tenant-1", OrgID: "org-1", DisplayName: "tenant"}},
		Sites:         []seed.Site{{ID: "site-1", Name: "site", TenantID: "tenant-1", Backend: seed.SiteBackendVM}},
		VPCs:          []seed.VPC{{ID: "vpc-1", Name: "vpc", TenantID: "tenant-1", SiteID: "site-1"}},
		Subnets:       []seed.Subnet{{ID: "subnet-1", Name: "subnet", VPCID: "vpc-1", SiteID: "site-1", CIDR: "10.1.0.0/24", PrefixLength: 24}},
		InstanceTypes: []seed.InstanceType{{ID: "itype-1", Name: "itype", SiteID: "site-1", CPUCount: 1, MemoryGiB: 1}},
	}
	require.NoError(t, store.ApplySeed(ctx, doc))

	subnet := "subnet-1"
	instance, _, err := store.CreateInstance(ctx, state.CreateInstanceInput{
		Backend:        state.SiteBackendVM,
		SiteID:         "site-1",
		Name:           "node-1",
		TenantID:       "tenant-1",
		InstanceTypeID: "itype-1",
		VPCID:          "vpc-1",
		MachineID:      "machine-1",
		Interfaces:     []state.InterfaceRequest{{SubnetID: &subnet}},
		VM:             &state.VMState{DesiredState: desired, ObservedState: observed},
	})
	require.NoError(t, err)

	runtime := vm.NewMockRuntime()
	runtime.SetState("machine-1", machineState)
	return &Engine{
		cfg:     config.Config{DBPath: filepath.Join(t.TempDir(), "nico-sim.db")},
		store:   store,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		runtime: runtime,
	}, instance.ID
}
