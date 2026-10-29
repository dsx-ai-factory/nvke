// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundTripsSiteConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &Config{
		Sites: map[string]SiteConfig{
			"dev": {
				URL:                 "http://localhost:8080",
				Kubeconfig:          "/home/op/.kube/dev-mgmt.yaml",
				ProxyClientCertFile: "/home/op/.config/nvkectl/client.crt",
				ProxyClientKeyFile:  "/home/op/.config/nvkectl/client.key",
			},
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
	if site.ProxyClientCertFile != "/home/op/.config/nvkectl/client.crt" {
		t.Errorf("ProxyClientCertFile = %q, want proxy client certificate path", site.ProxyClientCertFile)
	}
	if site.ProxyClientKeyFile != "/home/op/.config/nvkectl/client.key" {
		t.Errorf("ProxyClientKeyFile = %q, want proxy client key path", site.ProxyClientKeyFile)
	}
}

// TestLoadReadsTheLegacyLocationOnce: a config written by nkectl, the binary's
// previous name, is read when nvkectl has none of its own, copied to the
// current location, and not read again once that copy exists.
func TestLoadReadsTheLegacyLocationOnce(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	legacy := filepath.Join(xdg, "nkectl", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("sites:\n  old:\n    url: http://old.example\ncurrent-site: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentSite != "old" || loaded.Sites["old"].URL != "http://old.example" {
		t.Fatalf("Load did not read the legacy file: %+v", loaded)
	}
	current := filepath.Join(xdg, "nvkectl", "config.yaml")
	if _, err := os.Stat(current); err != nil {
		t.Fatalf("Load did not copy the legacy file to %s: %v", current, err)
	}

	// The legacy file changes after the handover; the copy is what is read now.
	if err := os.WriteFile(legacy, []byte("sites:\n  other:\n    url: http://other.example\ncurrent-site: other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatalf("Load again: %v", err)
	}
	if again.CurrentSite != "old" {
		t.Errorf("second Load read the legacy file again: current-site = %q, want %q", again.CurrentSite, "old")
	}
}

func TestLoadReadsTheOTLPFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "nvkectl"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := "sites:\n  dev:\n    url: http://localhost:8080\n    kubeconfig: /k\n    otlp-endpoint: collector:4317\n    otlp-insecure: true\ncurrent-site: dev\n"
	if err := os.WriteFile(filepath.Join(dir, "nvkectl", "config.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	site := cfg.Sites["dev"]
	if site.OTLPEndpoint != "collector:4317" || !site.OTLPInsecure {
		t.Errorf("site = %+v, want otlp-endpoint collector:4317 and otlp-insecure true", site)
	}
}

func TestSaveOmitsUnsetOTLPFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg := &Config{Sites: map[string]SiteConfig{"dev": {URL: "http://localhost:8080", Kubeconfig: "/k"}}, CurrentSite: "dev"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "nvkectl", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "otlp") {
		t.Errorf("config without an OTLP endpoint serialises otlp keys:\n%s", data)
	}
}

func TestSaveLoadRoundTripsTheOTLPFields(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &Config{Sites: map[string]SiteConfig{"dev": {
		URL: "http://localhost:8080", Kubeconfig: "/k", OTLPEndpoint: "collector:4317", OTLPInsecure: true,
	}}, CurrentSite: "dev"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Sites["dev"]; got != cfg.Sites["dev"] {
		t.Errorf("round trip = %+v, want %+v", got, cfg.Sites["dev"])
	}
}
