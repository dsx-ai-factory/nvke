// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package vm

import (
	"context"
	"sync"
)

// NewMockRuntime returns a Runtime backed by an in-memory map (no real VMs).
func NewMockRuntime() *MockRuntime {
	return &MockRuntime{running: map[string]bool{}, states: map[string]MachineState{}}
}

// MockRuntime is exported so tests can call SetState.
type MockRuntime struct {
	mu      sync.Mutex
	running map[string]bool
	states  map[string]MachineState
}

// SetState sets the hypervisor state for a machine. Test helper.
func (r *MockRuntime) SetState(machineID string, s MachineState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[machineID] = s
	r.running[machineID] = (s == MachineStateRunning)
}

func (r *MockRuntime) EnsureBridge(context.Context, string, string) error { return nil }

func (r *MockRuntime) CreateAndStart(_ context.Context, spec InstanceSpec) (string, error) {
	machineID := "mock-" + spec.InstanceID
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running[machineID] = true
	r.states[machineID] = MachineStateRunning
	return machineID, nil
}

func (r *MockRuntime) Reboot(_ context.Context, machineID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running[machineID] = true
	r.states[machineID] = MachineStateRunning
	return nil
}

func (r *MockRuntime) Delete(_ context.Context, machineID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, machineID)
	delete(r.states, machineID)
	return nil
}

func (r *MockRuntime) GetState(_ context.Context, machineID string) (MachineState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.states[machineID]; ok {
		return s, nil
	}
	return MachineStateNotFound, nil
}

func (r *MockRuntime) IsRunning(ctx context.Context, machineID string) (bool, error) {
	s, err := r.GetState(ctx, machineID)
	return s == MachineStateRunning, err
}

func (r *MockRuntime) SupportsTCPProbe() bool {
	return false
}

func (r *MockRuntime) ListMachines(_ context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	machines := make([]string, 0, len(r.running))
	for id := range r.running {
		machines = append(machines, id)
	}
	return machines, nil
}

func (r *MockRuntime) TPMEKCert(context.Context, string) (string, error) { return "", nil }
