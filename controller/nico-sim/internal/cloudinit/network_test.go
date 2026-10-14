// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

const selectedNetwork = "version: 2\nethernets:\n  nic0:\n    match:\n      macaddress: '02:00:00:00:00:42'\n    addresses: ['192.168.64.2/24']\n"

func TestInjectNVKENetworkPreservesCallerWork(t *testing.T) {
	in := "#cloud-config\nhostname: caller-node\nwrite_files:\n  - path: /etc/caller.cfg\n    content: keep\nruncmd:\n  - [echo, caller-work]\n"
	out, err := InjectNVKENetwork(in, selectedNetwork)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hostname string `yaml:"hostname"`
		Files    []struct {
			Path        string `yaml:"path"`
			Content     string `yaml:"content"`
			Permissions string `yaml:"permissions"`
		} `yaml:"write_files"`
		Commands [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "caller-node" || len(cfg.Files) != 2 || cfg.Files[0].Path != "/etc/caller.cfg" || cfg.Files[0].Content != "keep" {
		t.Fatalf("caller data changed: %+v", cfg)
	}
	if cfg.Files[1].Path != "/etc/netplan/00-dhcp-all.yaml" || cfg.Files[1].Permissions != "0600" {
		t.Fatalf("network override changed unrelated files: %+v", cfg.Files)
	}
	var netplan struct {
		Network map[string]interface{} `yaml:"network"`
	}
	if err := yaml.Unmarshal([]byte(cfg.Files[1].Content), &netplan); err != nil {
		t.Fatal(err)
	}
	if netplan.Network["version"] != 2 || !strings.Contains(cfg.Files[1].Content, "192.168.64.2/24") || !strings.Contains(cfg.Files[1].Content, "02:00:00:00:00:42") {
		t.Fatalf("selected MAC/address were not retained: %s", cfg.Files[1].Content)
	}
	if len(cfg.Commands) != 2 || strings.Join(cfg.Commands[0], " ") != "netplan apply" || strings.Join(cfg.Commands[1], " ") != "echo caller-work" {
		t.Fatalf("caller commands or prerequisite order changed: %v", cfg.Commands)
	}
}

func TestInjectNVKENetworkRejectsAmbiguousInputs(t *testing.T) {
	tests := []struct{ name, input, network string }{
		{name: "non cloud-config", input: "#!/bin/sh\necho test"},
		{name: "invalid magic header", input: "#cloud-config-unknown\nhostname: node"},
		{name: "malformed YAML", input: "#cloud-config\nwrite_files: ["},
		{name: "caller netplan", input: "#cloud-config\nwrite_files: [{path: /etc/netplan/caller.yaml}]"},
		{name: "caller network", input: "#cloud-config\nnetwork: {}"},
		{name: "non sequence files", input: "#cloud-config\nwrite_files: {}"},
		{name: "non sequence commands", input: "#cloud-config\nruncmd: wrong"},
		{name: "non string file path", input: "#cloud-config\nwrite_files: [{path: 42}]"},
		{name: "wrong network version", input: "#cloud-config", network: "version: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			network := tt.network
			if network == "" {
				network = selectedNetwork
			}
			if out, err := InjectNVKENetwork(tt.input, network); err == nil || out != "" {
				t.Fatalf("invalid input emitted cloud-config: %q, %v", out, err)
			}
		})
	}
}
