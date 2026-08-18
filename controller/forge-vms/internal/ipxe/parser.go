// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package ipxe parses iPXE boot scripts to extract VM provisioning parameters.
package ipxe

import (
	"fmt"
	"strings"
)

// BootSpec holds the parsed parameters extracted from an iPXE script.
type BootSpec struct {
	ImageURL       string // qcow2 cloud image URL
	ConsoleConfig  string // e.g. "console=tty0 console=ttyS1,115200"
	KernelParams   string // bare-metal kernel params like "pci=realloc=off ..."
	LUKSKey        string // e.g. "luks.key=changeit"
	TargetDisk     string // e.g. "/dev/nvme0n1"
	CloudInitDS    string // e.g. "ds=nocloud-net;s=..."
	UpdateGrubTmpl bool   // update_grub_template=yes
	UpdateGrubCfg  bool   // update_grub_cfg=yes
}

// Parse extracts a BootSpec from an iPXE script string.
// It parses `set` variable definitions and `chain` command key=value parameters.
func Parse(script string) (*BootSpec, error) {
	vars := map[string]string{}
	var chainLine string

	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "set ") {
			parts := strings.SplitN(trimmed, " ", 3)
			if len(parts) >= 3 {
				vars[parts[1]] = parts[2]
			}
			continue
		}

		if strings.HasPrefix(trimmed, "chain ") {
			chainLine = trimmed
		}
	}

	if chainLine == "" {
		return nil, fmt.Errorf("ipxe: no chain command found in script")
	}

	// Expand variables in the chain line
	expanded := expandVars(chainLine, vars)

	spec := &BootSpec{}
	// Parse chain line tokens for key=value pairs
	tokens := strings.Fields(expanded)
	for _, token := range tokens[1:] { // skip "chain" itself
		switch {
		case strings.HasPrefix(token, "image_url="):
			spec.ImageURL = strings.TrimPrefix(token, "image_url=")
		case strings.HasPrefix(token, "image_disk="):
			spec.TargetDisk = strings.TrimPrefix(token, "image_disk=")
		case strings.HasPrefix(token, "update_grub_template="):
			spec.UpdateGrubTmpl = strings.TrimPrefix(token, "update_grub_template=") == "yes"
		case strings.HasPrefix(token, "update_grub_cfg="):
			spec.UpdateGrubCfg = strings.TrimPrefix(token, "update_grub_cfg=") == "yes"
		case strings.HasPrefix(token, "ds="):
			spec.CloudInitDS = token
		case strings.HasPrefix(token, "luks.key="):
			spec.LUKSKey = token
		case strings.HasPrefix(token, "console="):
			if spec.ConsoleConfig != "" {
				spec.ConsoleConfig += " "
			}
			spec.ConsoleConfig += token
		}
	}

	// Also pull values from set variables as fallbacks
	if spec.ImageURL == "" {
		spec.ImageURL = vars["image-url"]
	}
	if spec.ConsoleConfig == "" {
		spec.ConsoleConfig = vars["console-config"]
	}
	if spec.KernelParams == "" {
		spec.KernelParams = vars["kernel-params"]
	}
	if spec.LUKSKey == "" {
		if v, ok := vars["luks-key"]; ok {
			spec.LUKSKey = v
		}
	}
	if spec.TargetDisk == "" {
		spec.TargetDisk = vars["target-disk"]
	}
	if spec.CloudInitDS == "" {
		if v, ok := vars["cloud-init-ds"]; ok {
			spec.CloudInitDS = v
		}
	}

	if spec.ImageURL == "" {
		return nil, fmt.Errorf("ipxe: no image_url found in script")
	}

	return spec, nil
}

// expandVars replaces ${var-name} references with their values from the vars map.
func expandVars(s string, vars map[string]string) string {
	result := s
	for name, value := range vars {
		result = strings.ReplaceAll(result, "${"+name+"}", value)
	}
	return result
}
