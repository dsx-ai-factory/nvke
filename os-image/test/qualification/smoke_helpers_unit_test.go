// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func smokeFixture() (smokeTarget, smokeFacts) {
	target := smokeTarget{
		Address: "192.0.2.10", ProvisioningMAC: "02:00:00:00:00:01",
		NICDriver: "mlx5_core", RequiredModules: []string{"mlx5_core", "nvme"},
		Expected: imageExpectation{
			SchemaVersion: "nvke.os-image.identity/v1", Ubuntu: "26.04", Arch: "arm64",
			KernelRelease: "test-kernel", BuildID: "candidate-build-123",
		},
	}
	facts := smokeFacts{
		Ubuntu: "26.04", Distro: "ubuntu", Arch: "arm64", KernelRelease: "test-kernel",
		BuildID: "candidate-build-123", CloudInit: "done", RootSource: "/dev/mapper/ubuntu-root",
		RouteInterface: "enp1s0", Modules: []string{"mlx5_core", "nvme"}, GPUCount: -1,
		Interfaces: map[string]smokeInterface{
			"enp1s0": {MAC: "02:00:00:00:00:01", Addresses: []string{"192.0.2.10"}, Carrier: true, Driver: "mlx5_core"},
		},
	}
	return target, facts
}

func TestSmokeChecks(t *testing.T) {
	cases := []struct {
		name   string
		change func(*smokeTarget, *smokeFacts)
		want   string
	}{
		{name: "CPU-only shape skips GPU checks", change: func(*smokeTarget, *smokeFacts) {}},
		{name: "wrong Ubuntu", change: func(_ *smokeTarget, f *smokeFacts) { f.Ubuntu = "24.04" }, want: "Ubuntu"},
		{name: "wrong architecture", change: func(_ *smokeTarget, f *smokeFacts) { f.Arch = "amd64" }, want: "architecture"},
		{name: "wrong kernel", change: func(_ *smokeTarget, f *smokeFacts) { f.KernelRelease = "old-kernel" }, want: "kernel"},
		{name: "stale image", change: func(_ *smokeTarget, f *smokeFacts) { f.BuildID = "previous-build" }, want: "build ID"},
		{name: "unstamped image", change: func(_ *smokeTarget, f *smokeFacts) { f.BuildID = "" }, want: "build ID"},
		{name: "cloud-init unfinished", change: func(_ *smokeTarget, f *smokeFacts) { f.CloudInit = "running" }, want: "cloud-init"},
		{name: "missing root device", change: func(_ *smokeTarget, f *smokeFacts) { f.RootSource = "overlay" }, want: "root filesystem"},
		{name: "missing route", change: func(_ *smokeTarget, f *smokeFacts) { f.RouteInterface = "missing" }, want: "route"},
		{name: "disconnected NIC", change: func(_ *smokeTarget, f *smokeFacts) {
			iface := f.Interfaces["enp1s0"]
			iface.Carrier = false
			f.Interfaces["enp1s0"] = iface
		}, want: "carrier"},
		{name: "wrong onboard NIC", change: func(_ *smokeTarget, f *smokeFacts) {
			iface := f.Interfaces["enp1s0"]
			iface.MAC = "02:00:00:00:00:02"
			f.Interfaces["enp1s0"] = iface
		}, want: "NIC MAC"},
		{name: "wrong NIC driver", change: func(_ *smokeTarget, f *smokeFacts) {
			iface := f.Interfaces["enp1s0"]
			iface.Driver = "unexpected"
			f.Interfaces["enp1s0"] = iface
		}, want: "NIC driver"},
		{name: "wrong allocated address", change: func(target *smokeTarget, _ *smokeFacts) { target.Address = "192.0.2.20" }, want: "allocated address"},
		{name: "missing storage driver", change: func(_ *smokeTarget, f *smokeFacts) { f.Modules = []string{"mlx5_core"} }, want: "nvme"},
		{name: "GPU shape with failed driver", change: func(target *smokeTarget, _ *smokeFacts) { target.GPUCount = 4 }, want: "healthy GPUs"},
		{name: "GPU shape with correct count", change: func(target *smokeTarget, f *smokeFacts) { target.GPUCount, f.GPUCount = 4, 4 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, facts := smokeFixture()
			tc.change(&target, &facts)
			err := checkSmoke(target, facts)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestSmokeConfig(t *testing.T) {
	for _, mutation := range []string{"valid", "wrong machine type", "missing manifest", "same Ubuntu twice", "missing case", "unsafe user", "wrong schema"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			writeJSON := func(name string, value any) {
				t.Helper()
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "ssh.conf"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			target, _ := smokeFixture()
			expected := target.Expected
			if mutation == "wrong schema" {
				expected.SchemaVersion = "unsupported"
			}
			writeJSON("26.json", expected)
			if mutation != "same Ubuntu twice" {
				expected.Ubuntu = "24.04"
			}
			writeJSON("24.json", expected)
			target.Manifest, target.SSHConfig, target.User = "26.json", "ssh.conf", "ubuntu"
			other := target
			other.Manifest, other.Address = "24.json", "192.0.2.11"
			cfg := smokeConfig{MachineTypeID: "example-type", Targets: []smokeTarget{target, other}}
			switch mutation {
			case "wrong machine type":
				cfg.MachineTypeID = "another-type"
			case "missing manifest":
				cfg.Targets[0].Manifest = "missing.json"
			case "missing case":
				cfg.Targets = cfg.Targets[:1]
			case "unsafe user":
				cfg.Targets[0].User = "-oProxyCommand=example"
			}
			writeJSON("config.json", cfg)
			_, err := loadSmokeConfig(filepath.Join(dir, "config.json"), "example-type")
			if (err == nil) != (mutation == "valid") {
				t.Fatalf("configuration result: %v", err)
			}
		})
	}
}

func TestSmokeJSONRejectsUnknownFieldsAndTrailingObjects(t *testing.T) {
	for _, raw := range []string{`{"build_id":"example","typo":true}`, `{} {}`} {
		var expectation imageExpectation
		if err := decodeStrictJSON([]byte(raw), &expectation); err == nil {
			t.Fatalf("accepted invalid manifest: %s", raw)
		}
	}
}
