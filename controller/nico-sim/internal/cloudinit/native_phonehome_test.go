// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestNativePhoneHomeUsesDeclaredMetadataGateway(t *testing.T) {
	for _, test := range []struct {
		name, gateway, inputURL, expected string
		native, allowed                   bool
	}{
		{"native default subnet", "192.168.64.1", legacyMetadataPhoneHomeURL, "http://192.168.64.1:7777/latest/meta-data/phone_home", true, true},
		{"native explicit subnet", "172.30.0.1", legacyMetadataPhoneHomeURL, "http://172.30.0.1:7777/latest/meta-data/phone_home", true, true},
		{"legacy unchanged", "172.30.0.1", legacyMetadataPhoneHomeURL, legacyMetadataPhoneHomeURL, false, true},
		{"caller callback preserved", "172.30.0.1", "http://caller.example/callback", "http://caller.example/callback", true, true},
		{"missing native gateway", "", legacyMetadataPhoneHomeURL, "", true, false},
		{"nonprivate gateway", "8.8.8.8", legacyMetadataPhoneHomeURL, "", true, false},
		{"unsupported gateway family", "fd00::1", legacyMetadataPhoneHomeURL, "", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := yaml.Marshal(map[string]interface{}{"phone_home": map[string]interface{}{"url": test.inputURL, "tries": 10, "post": "all"}, "hostname": "caller"})
			if err != nil {
				t.Fatal(err)
			}
			input := "#cloud-config\n" + string(data)
			var out string
			if test.native {
				out, err = InjectNativeNVKENetwork(input, selectedNetwork, test.gateway)
			} else {
				out, err = InjectNVKENetwork(input, selectedNetwork)
			}
			if (err == nil) != test.allowed || !test.allowed && out != "" {
				t.Fatalf("callback admission differs: %v", err)
			}
			if !test.allowed {
				return
			}
			var observed struct {
				Hostname  string `yaml:"hostname"`
				PhoneHome struct {
					URL   string `yaml:"url"`
					Tries int    `yaml:"tries"`
					Post  string `yaml:"post"`
				} `yaml:"phone_home"`
			}
			if err := yaml.Unmarshal([]byte(out), &observed); err != nil {
				t.Fatal(err)
			}
			if observed.Hostname != "caller" || observed.PhoneHome.URL != test.expected || observed.PhoneHome.Tries != 10 || observed.PhoneHome.Post != "all" {
				t.Fatalf("callback or caller settings changed: %+v", observed)
			}
		})
	}
}
