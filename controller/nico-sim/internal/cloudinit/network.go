// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v4"
)

// InjectNVKENetwork explicitly configures images whose source-owned recipe
// disables NoCloud network-config and installs /etc/netplan/00-dhcp-all.yaml.
// Existing caller-owned network files are rejected rather than overwritten.
func InjectNVKENetwork(userData, networkConfig string) (string, error) {
	return injectNVKENetwork(userData, networkConfig, "", false)
}

// InjectNativeNVKENetwork selects the explicit Darwin handoff. In addition to
// the declared Netplan data, it adapts the exact supported local profile's
// interface-name guard for QEMU's observed enp0s1 device. Artifact bytes are not
// changed; this is a diagnostic runtime user-data projection.
func InjectNativeNVKENetwork(userData, networkConfig, gateway string) (string, error) {
	return injectNVKENetwork(userData, networkConfig, gateway, true)
}

func injectNVKENetwork(userData, networkConfig, gateway string, native bool) (string, error) {
	// Trailing whitespace can be part of a YAML literal scalar (for example a
	// boot command). Preserve it while locating the cloud-config header.
	trimmed := strings.TrimLeft(userData, " \t\r\n")
	header, body, _ := strings.Cut(trimmed, "\n")
	if strings.TrimSpace(header) != "#cloud-config" {
		return "", fmt.Errorf("NVKE network injection requires cloud-config user-data")
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		return "", fmt.Errorf("decode cloud-config: %w", err)
	}
	if cfg == nil {
		cfg = make(map[string]interface{})
	}
	if native {
		if err := projectNativeLocalBootcmd(cfg); err != nil {
			return "", err
		}
		if err := projectNativePhoneHome(cfg, gateway); err != nil {
			return "", err
		}
	}
	if _, exists := cfg["network"]; exists {
		return "", fmt.Errorf("caller already owns cloud-config network configuration")
	}
	var files []interface{}
	if current, exists := cfg["write_files"]; exists {
		var ok bool
		files, ok = current.([]interface{})
		if !ok {
			return "", fmt.Errorf("cloud-config write_files must be a sequence")
		}
	}
	for _, entry := range files {
		file, ok := entry.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("cloud-config write_files entry must be a mapping")
		}
		path, ok := file["path"].(string)
		if !ok {
			return "", fmt.Errorf("cloud-config write_files path must be a string")
		}
		if strings.HasPrefix(path, "/etc/netplan/") {
			return "", fmt.Errorf("caller already owns netplan file %q; select seed mode with its existing network configuration", path)
		}
	}
	var commands []interface{}
	if current, exists := cfg["runcmd"]; exists {
		var ok bool
		commands, ok = current.([]interface{})
		if !ok {
			return "", fmt.Errorf("cloud-config runcmd must be a sequence")
		}
	}
	// The network seed is a version-2 Netplan body; the file needs the owning
	// network key. The existing YAML codec parses and renders the full document.
	var network map[string]interface{}
	if err := yaml.Unmarshal([]byte(networkConfig), &network); err != nil {
		return "", fmt.Errorf("decode selected network configuration: %w", err)
	}
	if network["version"] != 2 {
		return "", fmt.Errorf("selected network configuration must declare version 2")
	}
	content, err := yaml.Marshal(map[string]interface{}{"network": network})
	if err != nil {
		return "", fmt.Errorf("render selected network configuration: %w", err)
	}
	files = append(files, map[string]interface{}{
		"path": "/etc/netplan/00-dhcp-all.yaml", "permissions": "0600", "content": string(content),
	})
	// This is a guest operation. The source-owned DHCP file is replaced above;
	// apply the exact declared MAC/address before caller work.
	cfg["write_files"] = files
	cfg["runcmd"] = append([]interface{}{
		[]interface{}{"netplan", "apply"},
	}, commands...)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("render cloud-config: %w", err)
	}
	return "#cloud-config\n" + string(out), nil
}
