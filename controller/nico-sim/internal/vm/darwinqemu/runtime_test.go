// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package darwinqemu

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/nico-sim/internal/vm"
)

func TestVMNetLauncherArgs(t *testing.T) {
	tests := []struct {
		name string
		path string
		args []string
	}{
		{name: "current launcher", path: "/opt/homebrew/opt/vmnet-helper/libexec/vmnet-run", args: []string{"--operation-mode", "shared"}},
		{name: "legacy launcher", path: "/opt/vmnet-helper/bin/vmnet-client", args: []string{"--unprivileged", "--operation-mode", "shared"}},
		{name: "unknown launcher", path: "/tmp/custom-launcher"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, err := vmnetLauncherArgs(tt.path)
			if tt.args == nil {
				if err == nil || args != nil {
					t.Fatalf("unknown launcher produced args: %v, %v", args, err)
				}
				return
			}
			if err != nil || !slices.Equal(args, tt.args) {
				t.Fatalf("launcher args = %v, %v; want %v", args, err, tt.args)
			}
		})
	}
}

func TestPlatformArgsAccelerator(t *testing.T) {
	tests := []struct {
		name        string
		qemu        string
		accelerator string
		machine     string
		cpu         string
		wantError   bool
	}{
		{name: "ARM default HVF", qemu: "qemu-system-aarch64", machine: "virt,accel=hvf", cpu: "host"},
		{name: "ARM explicit HVF", qemu: "qemu-system-aarch64", accelerator: "hvf", machine: "virt,accel=hvf", cpu: "host"},
		{name: "ARM TCG", qemu: "/opt/homebrew/bin/qemu-system-aarch64", accelerator: "tcg", machine: "virt,accel=tcg", cpu: "max"},
		{name: "x86 default HVF", qemu: "qemu-system-x86_64", machine: "q35,accel=hvf", cpu: "host"},
		{name: "x86 explicit HVF", qemu: "qemu-system-x86_64", accelerator: "hvf", machine: "q35,accel=hvf", cpu: "host"},
		{name: "x86 TCG", qemu: "qemu-system-x86_64", accelerator: "tcg", machine: "q35,accel=tcg", cpu: "max"},
		{name: "unsupported accelerator", qemu: "qemu-system-aarch64", accelerator: "kvm", wantError: true},
		{name: "no argument injection", qemu: "qemu-system-aarch64", accelerator: "tcg,virtualization=on", wantError: true},
		{name: "no implicit fallback", qemu: "qemu-system-aarch64", accelerator: "hvf:tcg", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine, cpu, firmware, err := platformArgs(tt.qemu, tt.accelerator)
			if tt.wantError {
				if err == nil || machine != "" || cpu != "" || firmware != nil {
					t.Fatalf("rejected selection produced args: %q %q %v, error %v", machine, cpu, firmware, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("platformArgs: %v", err)
			}
			if machine != tt.machine || cpu != tt.cpu {
				t.Fatalf("machine/CPU = %q/%q, want %q/%q", machine, cpu, tt.machine, tt.cpu)
			}
		})
	}
}

func TestInvalidAcceleratorPreservesVMState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin admission fixture requires macOS")
	}
	instances := t.TempDir()
	r := New("qemu-system-aarch64", instances, "kvm")
	instance := "accelerator-rejection"
	dir := filepath.Join(instances, instance)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, "qemu.pid"), filepath.Join(dir, "vmnet-client.pid")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("preserve-existing-state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.EnsureBridge(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "QEMU accelerator") {
		t.Fatalf("EnsureBridge error = %v, want accelerator rejection", err)
	}
	id, err := r.CreateAndStart(context.Background(), vm.InstanceSpec{InstanceID: instance})
	if err == nil || id != "" || !strings.Contains(err.Error(), "QEMU accelerator") {
		t.Fatalf("CreateAndStart = %q, %v; want accelerator rejection", id, err)
	}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != "preserve-existing-state" {
			t.Fatalf("rejected selection changed %s: %q, %v", path, data, readErr)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(paths) {
		t.Fatalf("rejected selection changed instance files: %v, %v", entries, err)
	}
}
