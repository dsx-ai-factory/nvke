// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

func TestSaveLoadRoundTripsKubeconfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		Sites: map[string]SiteConfig{
			"dev": {URL: "http://localhost:8080", Kubeconfig: "/home/op/.kube/dev-mgmt.yaml"},
		},
		CurrentSite: "dev",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	site, err := loaded.CurrentSiteConfig()
	if err != nil {
		t.Fatalf("CurrentSiteConfig: %v", err)
	}
	if site.URL != "http://localhost:8080" {
		t.Errorf("URL = %q, want %q", site.URL, "http://localhost:8080")
	}
	if site.Kubeconfig != "/home/op/.kube/dev-mgmt.yaml" {
		t.Errorf("Kubeconfig = %q, want %q", site.Kubeconfig, "/home/op/.kube/dev-mgmt.yaml")
	}
}
