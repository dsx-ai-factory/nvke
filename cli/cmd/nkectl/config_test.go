// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/NVIDIA/nke/cli/internal/config"
)

func TestSetSiteRequiresKubeconfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{"config", "set-site", "--url", "http://localhost:8080"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when --kubeconfig is omitted, got nil")
	}
}

func TestSetSitePersistsKubeconfigAndCurrentSite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := newRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{
		"config", "set-site",
		"--url", "http://localhost:8080",
		"--kubeconfig", "/home/op/.kube/dev-mgmt.yaml",
		"--name", "dev",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("set-site: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CurrentSite != "dev" {
		t.Errorf("CurrentSite = %q, want %q", cfg.CurrentSite, "dev")
	}
	site, ok := cfg.Sites["dev"]
	if !ok {
		t.Fatal("site \"dev\" not saved")
	}
	if site.Kubeconfig != "/home/op/.kube/dev-mgmt.yaml" {
		t.Errorf("Kubeconfig = %q, want %q", site.Kubeconfig, "/home/op/.kube/dev-mgmt.yaml")
	}
}

func TestUseSiteSwitchesCurrentSite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &config.Config{
		Sites: map[string]config.SiteConfig{
			"dev":  {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"},
			"prod": {URL: "https://prod-cwe.example.com", Kubeconfig: "/kube/prod.yaml"},
		},
		CurrentSite: "dev",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	root := newRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{"config", "use-site", "prod"})
	if err := root.Execute(); err != nil {
		t.Fatalf("use-site: %v", err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentSite != "prod" {
		t.Errorf("CurrentSite = %q, want %q", loaded.CurrentSite, "prod")
	}
}

func TestUseSiteUnknownNameErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &config.Config{
		Sites:       map[string]config.SiteConfig{"dev": {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"}},
		CurrentSite: "dev",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	root := newRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{"config", "use-site", "staging"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown site name, got nil")
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentSite != "dev" {
		t.Errorf("CurrentSite changed to %q despite error, want unchanged %q", loaded.CurrentSite, "dev")
	}
}

func TestResolveSiteConfigPrefersSiteFlagOverride(t *testing.T) {
	cfg := &config.Config{
		Sites: map[string]config.SiteConfig{
			"dev":  {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"},
			"prod": {URL: "https://prod-cwe.example.com", Kubeconfig: "/kube/prod.yaml"},
		},
		CurrentSite: "dev",
	}

	site = "prod"
	t.Cleanup(func() { site = "" })

	resolved, err := resolveSiteConfig(cfg)
	if err != nil {
		t.Fatalf("resolveSiteConfig: %v", err)
	}
	if resolved.URL != "https://prod-cwe.example.com" {
		t.Errorf("URL = %q, want prod URL", resolved.URL)
	}
}

func TestResolveSiteConfigFallsBackToCurrentSite(t *testing.T) {
	cfg := &config.Config{
		Sites:       map[string]config.SiteConfig{"dev": {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"}},
		CurrentSite: "dev",
	}

	site = ""

	resolved, err := resolveSiteConfig(cfg)
	if err != nil {
		t.Fatalf("resolveSiteConfig: %v", err)
	}
	if resolved.URL != "http://localhost:8080" {
		t.Errorf("URL = %q, want dev URL", resolved.URL)
	}
}

func TestResolveSiteConfigUnknownSiteFlagErrors(t *testing.T) {
	cfg := &config.Config{
		Sites:       map[string]config.SiteConfig{"dev": {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"}},
		CurrentSite: "dev",
	}

	site = "staging"
	t.Cleanup(func() { site = "" })

	if _, err := resolveSiteConfig(cfg); err == nil {
		t.Fatal("expected error for unknown --site override, got nil")
	}
}
