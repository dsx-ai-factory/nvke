// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/state"
	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm"
)

// leaseNet serves a fixed lease (none when empty) and ignores reservations.
type leaseNet struct {
	NetworkManager
	ip string
}

func (n leaseNet) LookupLease(context.Context, string) (string, bool, error) {
	return n.ip, n.ip != "", nil
}

func (leaseNet) WriteReservation(string, string, string, string, *slog.Logger) {}

// powerRuntime records the power operations the engine issues.
type powerRuntime struct {
	vm.Runtime
	calls []string
}

func (r *powerRuntime) SupportsTCPProbe() bool { return true }

func (r *powerRuntime) Stop(context.Context, string) error {
	r.calls = append(r.calls, "stop")
	return nil
}

func (r *powerRuntime) Start(context.Context, string) error {
	r.calls = append(r.calls, "start")
	return nil
}

func TestReconcileRunningLeaseTimeout(t *testing.T) {
	tests := []struct {
		name         string
		waited       time.Duration
		priorReason  string
		lease        string
		wantCalls    []string
		wantObserved string
		wantReason   string
		wantIP       string
	}{
		{"within the timeout", leaseTimeout - time.Second, "", "", nil, state.ObservedStateRunning, "", ""},
		{"past the timeout power-cycles once", leaseTimeout + time.Second, "", "", []string{"stop", "start"}, state.ObservedStateRunning, leasePowerCycledReason, ""},
		{"within the timeout after a power-cycle", leaseTimeout - time.Second, leasePowerCycledReason, "", nil, state.ObservedStateRunning, leasePowerCycledReason, ""},
		{"past the timeout after a power-cycle errors", leaseTimeout + time.Second, leasePowerCycledReason, "", nil, state.ObservedStateError, "no DHCP lease 5m0s after a power-cycle; the guest likely failed to boot, see its console log", ""},
		{"lease found past the timeout", leaseTimeout + time.Second, "", "192.0.2.1", nil, state.ObservedStateRunning, "", "192.0.2.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			e, id := newStopStartEngine(t, state.DesiredStatePresent, state.ObservedStateRunning, vm.MachineStateRunning)
			rt := &powerRuntime{Runtime: e.runtime}
			e.runtime = rt
			e.net = leaseNet{ip: tt.lease}

			instance, err := e.store.GetInstance(ctx, id)
			require.NoError(t, err)
			require.NoError(t, e.store.UpdateInterfaceIPs(ctx, instance.Interfaces[0].ID, nil))
			require.NoError(t, e.store.PatchVMInstanceState(ctx, id, state.VMInstanceStatePatch{ErrorReason: &tt.priorReason}))
			patched, err := e.store.GetInstance(ctx, id)
			require.NoError(t, err)
			e.now = func() time.Time { return patched.UpdatedAt.Add(tt.waited) }

			_ = e.reconcileInstance(ctx, id)

			got, err := e.store.GetInstance(ctx, id)
			require.NoError(t, err)
			require.Equal(t, tt.wantCalls, rt.calls)
			require.Equal(t, tt.wantObserved, got.VM.ObservedState)
			require.Equal(t, tt.wantReason, got.VM.ErrorReason)
			ip, err := firstInterfaceIP(got.Interfaces)
			require.NoError(t, err)
			require.Equal(t, tt.wantIP, ip)
		})
	}
}
