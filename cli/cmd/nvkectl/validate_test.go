// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generatedSiteSpec writes what `generate sitespec` produces, so the tests
// start from the file an operator holds.
func generatedSiteSpec(t *testing.T) string {
	t.Helper()
	stdout, _, err := runGenerate(t, "generate", "sitespec", "my-tier0-site", "--nodes", writeCSV(t, threeHostCSV), "--set", threeNodeVIPSet)
	if err != nil {
		t.Fatalf("generate sitespec: %v", err)
	}
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// filledSiteSpec replaces every controllerStack placeholder with a value and
// creates the secret files the section names.
func filledSiteSpec(t *testing.T, edits map[string]string) string {
	t.Helper()
	path := generatedSiteSpec(t)
	dir := filepath.Dir(path)
	for _, name := range []string{"nico-token", "registry-token", "tenant-password"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("placeholder\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	values := map[string]string{
		"REPLACE_ARCH": "arm64", "REPLACE_NICO_ENDPOINT": "https://nico.example.com", "REPLACE_NICO_ORG_ID": "org-1",
		"REPLACE_NICO_API_NAME": "forge", "REPLACE_NICO_TOKEN_PATH": filepath.Join(dir, "nico-token"),
		"REPLACE_REGISTRY_SERVER": "ghcr.io", "REPLACE_REGISTRY_USERNAME": "robot", "REPLACE_REGISTRY_TOKEN_PATH": filepath.Join(dir, "registry-token"),
		"REPLACE_TENANT_USER": "nke", "REPLACE_TENANT_PASSWORD_PATH": filepath.Join(dir, "tenant-password"),
		"REPLACE_DB_PASSWORD_PATH": filepath.Join(dir, "db-password"),
		// deploy/nico enables profiles.image without a pin.
		"  # images:\n  #   controller: \"\"\n  #   profiles: \"\"": "  images:\n    profiles: ghcr.io/example/controller-profiles:v1",
	}
	for k, v := range edits {
		values[k] = v
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for placeholder, value := range values {
		text = strings.ReplaceAll(text, placeholder, value)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateSiteSpec_GeneratedFileListsPlaceholders(t *testing.T) {
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", generatedSiteSpec(t))
	if err == nil {
		t.Fatal("a generated site spec validated as ready")
	}
	if !strings.Contains(stdout, "bootstrap: valid\n") {
		t.Errorf("bootstrap section not reported valid:\n%s", stdout)
	}
	if !strings.Contains(stdout, "controllerStack: 11 values still to fill in: controllerStack.arch, controllerStack.nico.endpoint") {
		t.Errorf("placeholders not listed:\n%s", stdout)
	}
	if strings.Contains(stdout, "is required") {
		t.Errorf("placeholders reported as errors too:\n%s", stdout)
	}
}

func TestValidateSiteSpec_BootstrapOnly(t *testing.T) {
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", generatedSiteSpec(t), "--bootstrap-only")
	if err != nil || stdout != "bootstrap: valid\n" {
		t.Errorf("--bootstrap-only: %q %v", stdout, err)
	}
}

func TestValidateSiteSpec_Filled(t *testing.T) {
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", filledSiteSpec(t, nil))
	if err != nil || !strings.HasSuffix(stdout, "controllerStack: valid\n") {
		t.Errorf("filled site spec: %q %v", stdout, err)
	}
}

func TestValidateSiteSpec_Problems(t *testing.T) {
	cases := map[string]struct {
		edits map[string]string
		want  string
	}{
		"bad arch":       {map[string]string{"REPLACE_ARCH": "x86"}, `controllerStack.arch "x86" must be one of amd64, arm64`},
		"bad endpoint":   {map[string]string{"REPLACE_NICO_ENDPOINT": "nico.example.com"}, "controllerStack.nico.endpoint"},
		"missing secret": {map[string]string{"REPLACE_TENANT_PASSWORD_PATH": "/nonexistent/password"}, "controllerStack.tenant.passwordFile \"/nonexistent/password\" cannot be read"},
		"empty required": {map[string]string{"REPLACE_TENANT_USER": `""`}, "controllerStack.tenant.user is required"},
		"half filled":    {map[string]string{"REPLACE_ARCH": "REPLACE_ARCH"}, "1 values still to fill in: controllerStack.arch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", filledSiteSpec(t, tc.edits))
			if err == nil {
				t.Fatalf("validated as ready:\n%s", stdout)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, stdout)
			}
		})
	}
}

func TestValidateSiteSpec_NoSection(t *testing.T) {
	path := generatedSiteSpec(t)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	text = text[:strings.Index(text, "\ncontrollerStack:\n")+1]
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", path)
	if err == nil || !strings.Contains(stdout, "controllerStack: missing") {
		t.Errorf("missing section: %q %v", stdout, err)
	}
	if _, _, err := runGenerate(t, "validate", "sitespec", "-f", path, "--bootstrap-only"); err != nil {
		t.Errorf("--bootstrap-only without the section: %v", err)
	}
}

func TestValidateSiteSpec_BootstrapErrorsComeFirst(t *testing.T) {
	path := filledSiteSpec(t, nil)
	body, _ := os.ReadFile(path)
	text := strings.Replace(string(body), `k8sVersion: "`, `k8sVersion: "v`, 1)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", path)
	if err == nil || !strings.Contains(err.Error(), "k8sVersion") || strings.Contains(stdout, "bootstrap: valid") {
		t.Errorf("bootstrap error: %q %v", stdout, err)
	}
	if _, _, err := runGenerate(t, "validate", "sitespec"); err == nil {
		t.Error("missing -f accepted")
	}
}
