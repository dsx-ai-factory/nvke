// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"
)

func localNetworkBootcmdFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/local-network-bootcmd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Bootcmd []string `yaml:"bootcmd"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil || len(cfg.Bootcmd) != 1 {
		t.Fatalf("source local boot command differs: %v", err)
	}
	return cfg.Bootcmd[0]
}

func TestNativeLocalBootcmdRequiresExactSource(t *testing.T) {
	source := localNetworkBootcmdFixture(t)
	for _, test := range []struct {
		name     string
		commands interface{}
		allowed  bool
	}{
		{"known command", []interface{}{source}, true},
		{"unrelated command", []interface{}{"echo caller"}, true},
		{"changed command", []interface{}{source + "echo changed\n"}, false},
		{"changed guard", []interface{}{strings.Replace(source, legacyLocalInterfaceGuard, "exit 1", 1)}, false},
		{"duplicate command", []interface{}{source, source}, false},
		{"invalid sequence", "echo caller", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := map[string]interface{}{"bootcmd": test.commands, "hostname": "caller"}
			raw, err := yaml.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			out, err := InjectNativeNVKENetwork("#cloud-config\n"+string(raw), selectedNetwork, "192.168.64.1")
			if (err == nil) != test.allowed || !test.allowed && out != "" {
				t.Fatalf("source admission differs: %v", err)
			}
			if !test.allowed {
				return
			}
			var observed struct {
				Hostname string   `yaml:"hostname"`
				Bootcmd  []string `yaml:"bootcmd"`
			}
			if err := yaml.Unmarshal([]byte(out), &observed); err != nil {
				t.Fatal(err)
			}
			if observed.Hostname != "caller" || len(observed.Bootcmd) != 1 {
				t.Fatal("caller fields changed")
			}
			expected := "echo caller"
			if test.name == "known command" {
				expected = strings.Replace(source, legacyLocalInterfaceGuard, `case "$candidate" in eth0|enp1s0|enp0s1) ;; *) exit 1 ;; esac`, 1)
			}
			if observed.Bootcmd[0] != expected {
				t.Fatal("native projection changed more than its exact interface guard")
			}
		})
	}
}

// Execute the admitted shell against private filesystem fixtures. No host
// interface, system configuration, GRUB binary or real guest is touched.
func TestNativeLocalBootcmdPreservesFailureSafety(t *testing.T) {
	source := localNetworkBootcmdFixture(t)
	for _, scenario := range []string{"native-repeat", "linux-rejects-native", "unsupported", "ambiguous", "foreign-grub", "symlink-grub"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := map[string]interface{}{"bootcmd": []interface{}{source}}
			if scenario != "linux-rejects-native" {
				if err := projectNativeLocalBootcmd(cfg); err != nil {
					t.Fatal(err)
				}
			}
			script := cfg["bootcmd"].([]interface{})[0].(string)
			root := t.TempDir()
			for _, dir := range []string{"sys/class/net", "grub.d", "network"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			nic := "enp0s1"
			if scenario == "unsupported" {
				nic = "enp0s2"
			}
			if err := os.MkdirAll(filepath.Join(root, "sys/class/net", nic, "device"), 0o700); err != nil {
				t.Fatal(err)
			}
			if scenario == "ambiguous" {
				if err := os.MkdirAll(filepath.Join(root, "sys/class/net/enp1s0/device"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			grub := filepath.Join(root, "grub.d/zz-nkd-local-network.cfg")
			if scenario == "foreign-grub" {
				if err := os.WriteFile(grub, []byte("foreign\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink-grub" {
				if err := os.Symlink(filepath.Join(root, "foreign"), grub); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "update-grub"), []byte("#!/bin/sh\nprintf executed > \"$TASK_NATIVE_SHELL_ROOT/invoked\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			script = strings.ReplaceAll(script, "/etc/default/grub.d", filepath.Join(root, "grub.d"))
			script = strings.ReplaceAll(script, "/etc/systemd/network", filepath.Join(root, "network"))
			script = strings.ReplaceAll(script, "/sys/class/net", filepath.Join(root, "sys/class/net"))
			for range 2 {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				command := exec.CommandContext(ctx, "sh", "-ec", script)
				command.Env = append(os.Environ(), "TASK_NATIVE_SHELL_ROOT="+root, "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"))
				command.WaitDelay = time.Second
				raw, err := command.CombinedOutput()
				cancel()
				if scenario != "native-repeat" {
					if err == nil {
						t.Fatal("unsafe or unsupported interface setup admitted")
					}
					if _, err := os.Stat(filepath.Join(root, "invoked")); !os.IsNotExist(err) {
						t.Fatal("rejected boot command reached GRUB")
					}
					return
				}
				if err != nil {
					t.Fatalf("native boot command: %v\n%s", err, raw)
				}
				link, err := os.ReadFile(filepath.Join(root, "network/10-netplan-enp0s1.link.d/10-nkd-local-name.conf"))
				if err != nil || string(link) != "[Link]\nNamePolicy=\nName=enp0s1\n" {
					t.Fatalf("native interface policy differs: %s %v", link, err)
				}
			}
		})
	}
}
