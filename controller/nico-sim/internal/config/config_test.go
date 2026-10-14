// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseQEMUAccelerator(t *testing.T) {
	tests := []struct {
		name        string
		backend     string
		accelerator string
		wantError   string
	}{
		{name: "mock default", backend: VMBackendMock},
		{name: "darwin default", backend: VMBackendDarwinQEMU},
		{name: "explicit HVF", backend: VMBackendDarwinQEMU, accelerator: "hvf"},
		{name: "explicit TCG", backend: VMBackendDarwinQEMU, accelerator: "tcg"},
		{name: "unsupported KVM", backend: VMBackendDarwinQEMU, accelerator: "kvm", wantError: "NICO_VMS_QEMU_ACCELERATOR"},
		{name: "unknown mode", backend: VMBackendDarwinQEMU, accelerator: "auto", wantError: "NICO_VMS_QEMU_ACCELERATOR"},
		{name: "no fallback list", backend: VMBackendDarwinQEMU, accelerator: "hvf:tcg", wantError: "NICO_VMS_QEMU_ACCELERATOR"},
		{name: "case is explicit", backend: VMBackendDarwinQEMU, accelerator: "TCG", wantError: "NICO_VMS_QEMU_ACCELERATOR"},
		{name: "mock cannot ignore selection", backend: VMBackendMock, accelerator: "tcg", wantError: "requires the"},
		{name: "libvirt cannot ignore selection", backend: VMBackendLibvirt, accelerator: "hvf", wantError: "requires the"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.backend == VMBackendDarwinQEMU && tt.wantError == "" && runtime.GOOS != "darwin" {
				t.Skip("positive Darwin config requires macOS")
			}
			t.Setenv("NICO_VMS_VM_BACKEND", tt.backend)
			t.Setenv("NICO_VMS_LIBVIRT_URI", "qemu:///session")
			t.Setenv("NICO_VMS_QEMU_ACCELERATOR", tt.accelerator)
			t.Setenv("NICO_SIM_DB_PATH", filepath.Join(t.TempDir(), "nico-sim.db"))
			t.Setenv("NICO_VMS_LIBVIRT_NETWORK", "")
			t.Setenv("NICO_CONTAINERS_ENABLED", "false")
			if tt.wantError == "" {
				t.Setenv("NICO_VMS_BRIDGE_CIDR", "192.168.64.0/24")
			} else {
				// Rejected accelerator choices must fail before normalization
				// can launch a vmnet probe for the missing bridge CIDR.
				t.Setenv("NICO_VMS_BRIDGE_CIDR", "")
			}
			cfg, err := Parse(slog.New(slog.NewTextHandler(io.Discard, nil)))
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Parse error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if cfg.QEMUAccelerator != tt.accelerator {
				t.Fatalf("accelerator = %q, want %q", cfg.QEMUAccelerator, tt.accelerator)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate parsed config: %v", err)
			}
		})
	}
}

func TestValidateQEMUAcceleratorBeforeOtherFields(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		message string
	}{
		{name: "invalid mode", config: Config{VMBackend: VMBackendDarwinQEMU, QEMUAccelerator: "kvm"}, message: "NICO_VMS_QEMU_ACCELERATOR"},
		{name: "wrong backend", config: Config{VMBackend: VMBackendMock, QEMUAccelerator: "tcg"}, message: "requires the"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.config.Validate(); err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("Validate error = %v, want %q", err, tt.message)
			}
		})
	}
}

func TestGuestNetworkConfigAdmission(t *testing.T) {
	for _, tt := range []struct {
		mode  string
		valid bool
	}{
		{mode: "", valid: true},
		{mode: "seed", valid: true},
		{mode: "nvke-userdata", valid: true},
		{mode: "auto"},
		{mode: "seed,nvke-userdata"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			cfg := Config{GuestNetworkConfig: tt.mode}
			err := cfg.validateGuestNetworkConfig()
			if (err == nil) != tt.valid {
				t.Fatalf("mode %q: admission error = %v", tt.mode, err)
			}
			if !tt.valid {
				t.Setenv("NICO_VMS_VM_BACKEND", VMBackendDarwinQEMU)
				t.Setenv("NICO_VMS_GUEST_NETWORK_CONFIG", tt.mode)
				t.Setenv("NICO_VMS_BRIDGE_CIDR", "")
				_, err := Parse(slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err == nil || !strings.Contains(err.Error(), "NICO_VMS_GUEST_NETWORK_CONFIG") {
					t.Fatalf("invalid handoff did not fail before native subnet probing: %v", err)
				}
			}
		})
	}
}
