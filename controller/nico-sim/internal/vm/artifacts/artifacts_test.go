// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestNativeBootcmdAdmissionBeforeArtifactOutput(t *testing.T) {
	raw, err := os.ReadFile("../../cloudinit/testdata/local-network-bootcmd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Bootcmd []string `yaml:"bootcmd"`
	}
	if err := yaml.Unmarshal(raw, &source); err != nil || len(source.Bootcmd) != 1 {
		t.Fatalf("source boot command differs: %v", err)
	}
	for _, test := range []struct {
		name                     string
		native, changed, allowed bool
	}{
		{"native supported", true, false, true},
		{"native changed", true, true, false},
		{"legacy supported", false, false, true},
		{"legacy caller command", false, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := source.Bootcmd[0]
			if test.changed {
				body += "echo changed\n"
			}
			data, err := yaml.Marshal(map[string]interface{}{
				"bootcmd":    []string{"echo prefix", body},
				"phone_home": map[string]interface{}{"url": "http://169.254.169.254:7777/latest/meta-data/phone_home", "post": "all", "tries": 10},
			})
			if err != nil {
				t.Fatal(err)
			}
			work := filepath.Join(t.TempDir(), "artifacts")
			_, err = (Builder{MockMode: true}).Build(t.Context(), BuildInput{
				WorkingDir: work, InstanceID: "fixture", UserData: "#cloud-config\n" + string(data),
				NICMAC: "02:00:00:00:00:42", IPAddress: "192.168.64.2", GatewayIP: "192.168.64.1", PrefixLength: 24,
				NetworkConfigViaUserData: true, PreserveInterfaceName: test.native,
			})
			if (err == nil) != test.allowed {
				t.Fatalf("artifact boundary differs: %v", err)
			}
			if !test.allowed {
				if _, err := os.Stat(work); !os.IsNotExist(err) {
					t.Fatal("rejected native profile created artifact output")
				}
				return
			}
			observed, err := os.ReadFile(filepath.Join(work, "user-data"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Bootcmd   []string `yaml:"bootcmd"`
				PhoneHome struct {
					URL   string `yaml:"url"`
					Post  string `yaml:"post"`
					Tries int    `yaml:"tries"`
				} `yaml:"phone_home"`
			}
			if err := yaml.Unmarshal(observed, &cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Bootcmd) != 2 || cfg.Bootcmd[0] != "echo prefix" {
				t.Fatal("unrelated caller boot commands changed")
			}
			if test.native {
				if !strings.Contains(cfg.Bootcmd[1], "eth0|enp1s0|enp0s1") {
					t.Fatal("native artifact omitted its interface projection")
				}
				if cfg.PhoneHome.URL != "http://192.168.64.1:7777/latest/meta-data/phone_home" {
					t.Fatal("native artifact omitted its metadata gateway callback")
				}
			} else if cfg.Bootcmd[1] != body {
				t.Fatal("legacy artifact changed caller command")
			} else if cfg.PhoneHome.URL != "http://169.254.169.254:7777/latest/meta-data/phone_home" {
				t.Fatal("legacy artifact changed caller callback")
			}
			if cfg.PhoneHome.Post != "all" || cfg.PhoneHome.Tries != 10 {
				t.Fatal("artifact changed callback policy")
			}
		})
	}
}

func TestRejectedNetworkHandoffCreatesNoOutput(t *testing.T) {
	work := filepath.Join(t.TempDir(), "not-created")
	_, err := (Builder{MockMode: true}).Build(t.Context(), BuildInput{
		WorkingDir: work, InstanceID: "rejected", UserData: "#!/bin/sh\necho caller",
		NICMAC: "02:00:00:00:00:42", IPAddress: "192.168.64.2", PrefixLength: 24,
		GatewayIP: "192.168.64.1", NetworkConfigViaUserData: true,
	})
	if err == nil {
		t.Fatal("unsupported user-data was accepted")
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("rejected handoff created output: %v", err)
	}
}

func TestSeedNetworkMatchesMACWithoutRenaming(t *testing.T) {
	tests := []struct {
		name     string
		preserve bool
		address  string
	}{
		{name: "legacy static", address: "192.168.64.2"},
		{name: "legacy DHCP"},
		{name: "native static", preserve: true, address: "192.168.64.2"},
		{name: "native DHCP", preserve: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work := t.TempDir()
			_, err := (Builder{MockMode: true}).Build(t.Context(), BuildInput{
				WorkingDir: work, InstanceID: "network-fixture", NICMAC: "02:00:00:00:00:42",
				IPAddress: tt.address, GatewayIP: "192.168.64.1", PrefixLength: 24, PreserveInterfaceName: tt.preserve,
			})
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(work, "network-config"))
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Ethernets map[string]struct {
					Match struct {
						MAC string `yaml:"macaddress"`
					} `yaml:"match"`
					SetName   string   `yaml:"set-name"`
					Addresses []string `yaml:"addresses"`
					DHCP      bool     `yaml:"dhcp4"`
				} `yaml:"ethernets"`
			}
			if err := yaml.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			nic, ok := config.Ethernets["eth0"]
			if !ok || len(config.Ethernets) != 1 || nic.Match.MAC != "02:00:00:00:00:42" {
				t.Fatalf("seed does not select the exact NIC: %+v", config)
			}
			wantName := "eth0"
			if tt.preserve {
				wantName = ""
			}
			if nic.SetName != wantName || nic.DHCP != (tt.address == "") {
				t.Fatalf("unexpected interface naming/DHCP: %+v", nic)
			}
			if tt.address != "" && (len(nic.Addresses) != 1 || nic.Addresses[0] != tt.address+"/24") {
				t.Fatalf("seed changed the declared address: %+v", nic)
			}
		})
	}
}

func TestNativeUserDataOwnsNetworkRendering(t *testing.T) {
	for _, test := range []struct {
		name             string
		native, userData bool
	}{
		{"native user-data", true, true},
		{"native seed", true, false},
		{"legacy user-data", false, true},
		{"legacy seed", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			work := t.TempDir()
			_, err := (Builder{MockMode: true}).Build(t.Context(), BuildInput{
				WorkingDir: work, InstanceID: "single-network-owner", UserData: "#cloud-config\nhostname: caller\nruncmd: [[echo, caller-work]]\n",
				NICMAC: "02:00:00:00:00:42", IPAddress: "192.168.64.2", GatewayIP: "192.168.64.1", PrefixLength: 24,
				PreserveInterfaceName: test.native, NetworkConfigViaUserData: test.userData,
			})
			if err != nil {
				t.Fatal(err)
			}
			seed, err := os.ReadFile(filepath.Join(work, "network-config"))
			if err != nil {
				t.Fatal(err)
			}
			var network map[string]interface{}
			if err := yaml.Unmarshal(seed, &network); err != nil {
				t.Fatal(err)
			}
			if test.native && test.userData {
				if len(network) != 1 || network["config"] != "disabled" {
					t.Fatalf("native handoff retains a second network renderer: %v", network)
				}
			} else if network["config"] != nil || network["version"] != 2 {
				t.Fatalf("ordinary seed network changed: %v", network)
			}
			raw, err := os.ReadFile(filepath.Join(work, "user-data"))
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Hostname string                           `yaml:"hostname"`
				Files    []struct{ Path, Content string } `yaml:"write_files"`
				Commands [][]string                       `yaml:"runcmd"`
			}
			if err := yaml.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Hostname != "caller" || strings.Join(cfg.Commands[len(cfg.Commands)-1], " ") != "echo caller-work" {
				t.Fatal("network ownership changed caller work")
			}
			if test.userData {
				if len(cfg.Files) != 1 || cfg.Files[0].Path != "/etc/netplan/00-dhcp-all.yaml" || strings.Join(cfg.Commands[0], " ") != "netplan apply" {
					t.Fatal("user-data omitted its single file or activation")
				}
				if !strings.Contains(cfg.Files[0].Content, "192.168.64.2/24") || !strings.Contains(cfg.Files[0].Content, "02:00:00:00:00:42") {
					t.Fatal("user-data lost the declared static network")
				}
			} else if len(cfg.Files) != 0 || len(cfg.Commands) != 1 {
				t.Fatal("seed-only handoff injected network work")
			}
		})
	}
}
