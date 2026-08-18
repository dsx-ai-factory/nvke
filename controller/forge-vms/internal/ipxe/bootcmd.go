// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ipxe

import (
	"fmt"
	"strings"
)

// GRUBBootcmdYAML generates a cloud-init bootcmd YAML snippet that configures
// GRUB with the appropriate console and kernel parameters. This replaces the
// GRUB configuration that qcow-imager.efi would normally perform on bare metal.
//
// The returned string is a complete bootcmd section (including the "bootcmd:" key)
// ready to be prepended to user-data.
func (spec *BootSpec) GRUBBootcmdYAML() string {
	if !spec.UpdateGrubTmpl && !spec.UpdateGrubCfg {
		return ""
	}

	// Build the GRUB_CMDLINE_LINUX value
	// Adapt console: ttyS1 -> ttyS0 for VM (QEMU serial is ttyS0)
	var grubParams []string

	console := adaptConsoleForVM(spec.ConsoleConfig)
	if console != "" {
		grubParams = append(grubParams, console)
	}

	// Extract loglevel from kernel params if present
	for _, param := range strings.Fields(spec.KernelParams) {
		if strings.HasPrefix(param, "loglevel=") {
			grubParams = append(grubParams, param)
		}
	}

	// Add LUKS key if present
	if spec.LUKSKey != "" {
		grubParams = append(grubParams, spec.LUKSKey)
	}

	grubCmdline := strings.Join(grubParams, " ")

	var cmds []string
	if spec.UpdateGrubTmpl {
		cmds = append(cmds, fmt.Sprintf(
			`    sed -i 's|^GRUB_CMDLINE_LINUX=.*|GRUB_CMDLINE_LINUX="%s"|' /etc/default/grub`,
			grubCmdline,
		))
	}
	if spec.UpdateGrubCfg {
		cmds = append(cmds, `    update-grub`)
	}

	return "bootcmd:\n  - |\n" + strings.Join(cmds, "\n") + "\n"
}

// adaptConsoleForVM converts bare-metal console settings to VM-appropriate ones.
//
// x86_64: production uses ttyS1 (COM2); QEMU exposes serial as ttyS0 → remap.
// aarch64: production uses ttyAMA0 (PL011 UART); QEMU also uses ttyAMA0 → keep as-is.
func adaptConsoleForVM(consoleConfig string) string {
	if consoleConfig == "" {
		return "console=ttyS0"
	}

	var parts []string
	for _, param := range strings.Fields(consoleConfig) {
		if !strings.HasPrefix(param, "console=") {
			continue
		}
		device := strings.TrimPrefix(param, "console=")
		if strings.HasPrefix(device, "ttyS1") {
			// x86_64: remap ttyS1 (bare-metal COM2) → ttyS0 (QEMU serial)
			device = "ttyS0" + strings.TrimPrefix(device, "ttyS1")
		}
		// ttyAMA0 (aarch64 PL011) and tty0 are kept as-is
		parts = append(parts, "console="+device)
	}

	if len(parts) == 0 {
		return "console=ttyS0"
	}
	return strings.Join(parts, " ")
}

// PrependBootcmd merges a bootcmd YAML prefix with existing user-data.
// If the prefix is empty, userData is returned unchanged.
// If userData already has a bootcmd section, the prefix commands are prepended.
func PrependBootcmd(userData, bootcmdYAML string) string {
	if bootcmdYAML == "" {
		return userData
	}
	if userData == "" {
		return "#cloud-config\n" + bootcmdYAML
	}

	// If userData starts with #cloud-config, insert bootcmd after it
	if strings.HasPrefix(userData, "#cloud-config") {
		// Check if userData already has bootcmd
		if strings.Contains(userData, "\nbootcmd:") {
			// Replace the existing key with the generated section. The existing
			// sequence items remain attached to that same bootcmd key.
			return strings.Replace(
				userData,
				"\nbootcmd:",
				"\n"+strings.TrimSuffix(bootcmdYAML, "\n"),
				1,
			)
		}
		// Insert bootcmd after the #cloud-config line
		parts := strings.SplitN(userData, "\n", 2)
		if len(parts) == 2 {
			return parts[0] + "\n" + bootcmdYAML + parts[1]
		}
		return userData + "\n" + bootcmdYAML
	}

	// Not cloud-config format, wrap it
	return "#cloud-config\n" + bootcmdYAML + userData
}
