// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package ipxe

import (
	"strings"
	"testing"
)

func TestGRUBBootcmdYAML(t *testing.T) {
	spec := &BootSpec{
		ConsoleConfig:  "console=tty0 console=ttyS1,115200",
		KernelParams:   "pci=realloc=off reboot=pci loglevel=1 iommu=pt intel_iommu=on",
		UpdateGrubTmpl: true,
		UpdateGrubCfg:  true,
	}

	yaml := spec.GRUBBootcmdYAML()

	if !strings.Contains(yaml, "bootcmd:") {
		t.Error("expected bootcmd: key")
	}
	if !strings.Contains(yaml, "console=ttyS0,115200") {
		t.Errorf("expected console=ttyS0,115200 in output, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "loglevel=1") {
		t.Errorf("expected loglevel=1 in output, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "update-grub") {
		t.Error("expected update-grub command")
	}
	if !strings.Contains(yaml, "sed -i") {
		t.Error("expected sed command for GRUB template")
	}
}

func TestGRUBBootcmdYAMLNoFlags(t *testing.T) {
	spec := &BootSpec{
		UpdateGrubTmpl: false,
		UpdateGrubCfg:  false,
	}
	if yaml := spec.GRUBBootcmdYAML(); yaml != "" {
		t.Errorf("expected empty string when both flags are false, got: %q", yaml)
	}
}

func TestGRUBBootcmdYAMLWithLUKS(t *testing.T) {
	spec := &BootSpec{
		ConsoleConfig:  "console=ttyS1,115200",
		LUKSKey:        "luks.key=changeit",
		UpdateGrubTmpl: true,
		UpdateGrubCfg:  true,
	}

	yaml := spec.GRUBBootcmdYAML()
	if !strings.Contains(yaml, "luks.key=changeit") {
		t.Errorf("expected LUKS key in output, got:\n%s", yaml)
	}
}

func TestAdaptConsoleForVM(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"console=tty0 console=ttyS1,115200", "console=tty0 console=ttyS0,115200"},
		{"console=ttyS1,115200", "console=ttyS0,115200"},
		{"console=ttyAMA0,115200", "console=ttyAMA0,115200"}, // aarch64: kept as-is
		{"", "console=ttyS0"},
	}
	for _, tt := range tests {
		got := adaptConsoleForVM(tt.input)
		if got != tt.want {
			t.Errorf("adaptConsoleForVM(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPrependBootcmd(t *testing.T) {
	bootcmd := "bootcmd:\n  - |\n    sed -i 's|foo|bar|' /etc/default/grub\n    update-grub\n"

	t.Run("empty userData", func(t *testing.T) {
		result := PrependBootcmd("", bootcmd)
		if !strings.HasPrefix(result, "#cloud-config\n") {
			t.Error("expected #cloud-config prefix")
		}
		if !strings.Contains(result, "bootcmd:") {
			t.Error("expected bootcmd in result")
		}
	})

	t.Run("cloud-config without bootcmd", func(t *testing.T) {
		userData := "#cloud-config\nruncmd:\n  - echo hello\n"
		result := PrependBootcmd(userData, bootcmd)
		if !strings.Contains(result, "bootcmd:") {
			t.Error("expected bootcmd in result")
		}
		if !strings.Contains(result, "runcmd:") {
			t.Error("expected original runcmd preserved")
		}
	})

	t.Run("empty bootcmd", func(t *testing.T) {
		userData := "#cloud-config\nruncmd:\n  - echo hello\n"
		result := PrependBootcmd(userData, "")
		if result != userData {
			t.Error("expected userData unchanged when bootcmd is empty")
		}
	})
}
