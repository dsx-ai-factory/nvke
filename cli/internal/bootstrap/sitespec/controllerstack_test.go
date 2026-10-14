// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func filledStack(t *testing.T) *ControllerStack {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{}
	for _, name := range []string{"nico-token", "tenant-password"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("placeholder\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = path
	}
	return &ControllerStack{
		Arch:     "arm64",
		Nico:     StackNico{Endpoint: "https://nico.example.com", OrgID: "org-1", APIName: "forge", TokenFile: files["nico-token"]},
		Tenant:   StackTenant{User: "nke", PasswordFile: files["tenant-password"]},
		Database: StackDatabase{PasswordFile: filepath.Join(dir, "db-password")},
	}
}

// baseSpec is the generated three-node site spec: what an operator holds
// after `nvkectl generate sitespec`, controllerStack placeholders included.
func baseSpec(t *testing.T) *SiteSpec {
	t.Helper()
	spec, err := Build("my-tier0-site", threeNodes(), []string{"cluster.controlPlaneVIP=10.0.0.5"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return spec
}

// The generated site spec carries the controllerStack section with placeholders, and
// the bootstrap must still accept it: the section is not the bootstrap's.
func TestTemplate_ControllerStackPlaceholdersPassBootstrapValidation(t *testing.T) {
	spec, err := Build("my-tier0-site", threeNodes(), []string{"cluster.controlPlaneVIP=10.0.0.5"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if spec.ControllerStack == nil {
		t.Fatal("the template carries no controllerStack: section")
	}
	left := spec.ControllerStackPlaceholders()
	want := []string{
		"controllerStack.arch", "controllerStack.nico.endpoint", "controllerStack.nico.orgID", "controllerStack.nico.apiName", "controllerStack.nico.tokenFile",
		"controllerStack.tenant.user", "controllerStack.tenant.passwordFile", "controllerStack.database.passwordFile",
	}
	if strings.Join(left, " ") != strings.Join(want, " ") {
		t.Errorf("placeholders:\n got %v\nwant %v", left, want)
	}
	err = spec.ValidateControllerStack()
	if err == nil {
		t.Fatal("ValidateControllerStack accepted placeholders")
	}
	for _, path := range want {
		if !strings.Contains(err.Error(), path+" is still a REPLACE_* placeholder") {
			t.Errorf("ValidateControllerStack does not name %s: %v", path, err)
		}
	}
}

func TestLoad_ControllerStackSectionIsOptional(t *testing.T) {
	spec := baseSpec(t)
	spec.ControllerStack = nil
	if err := spec.Validate(); err != nil {
		t.Fatalf("a spec without controllerStack: must validate for the bootstrap: %v", err)
	}
	if got := spec.ControllerStackPlaceholders(); got != nil {
		t.Errorf("placeholders without a section: %v", got)
	}
	if err := spec.ValidateControllerStack(); err == nil || !strings.Contains(err.Error(), "no controllerStack: section") {
		t.Errorf("ValidateControllerStack without a section: %v", err)
	}
}

func TestLoad_ControllerStackRejectsUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	body := strings.Replace(string(templateYAML), "    endpoint: REPLACE_NICO_ENDPOINT\n", "    endpont: x\n", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "endpont") {
		t.Errorf("unknown install key accepted: %v", err)
	}
}

func TestValidateControllerStack_Filled(t *testing.T) {
	spec := baseSpec(t)
	spec.ControllerStack = filledStack(t)
	if err := spec.ValidateControllerStack(); err != nil {
		t.Errorf("filled section rejected: %v", err)
	}
	if err := spec.ValidateControllerStackReferences(); err != nil {
		t.Errorf("existing files rejected, or the absent database password file was: %v", err)
	}
}

func TestValidateControllerStack_Checks(t *testing.T) {
	cases := map[string]struct {
		mutate func(*ControllerStack)
		want   string
	}{
		"bad arch":           {func(in *ControllerStack) { in.Arch = "x86" }, `controllerStack.arch "x86" must be one of amd64, arm64`},
		"bad endpoint":       {func(in *ControllerStack) { in.Nico.Endpoint = "nico.example.com" }, "controllerStack.nico.endpoint"},
		"missing token":      {func(in *ControllerStack) { in.Nico.TokenFile = "" }, "controllerStack.nico.tokenFile is required"},
		"missing user":       {func(in *ControllerStack) { in.Tenant.User = "" }, "controllerStack.tenant.user is required"},
		"missing db file":    {func(in *ControllerStack) { in.Database.PasswordFile = "" }, "controllerStack.database.passwordFile is required"},
		"mirror with scheme": {func(in *ControllerStack) { in.Mirror = "https://mirror.example.com" }, "controllerStack.mirror"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spec := baseSpec(t)
			spec.ControllerStack = filledStack(t)
			tc.mutate(spec.ControllerStack)
			err := spec.ValidateControllerStack()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateControllerStackReferences_MissingFile(t *testing.T) {
	spec := baseSpec(t)
	spec.ControllerStack = filledStack(t)
	spec.ControllerStack.Tenant.PasswordFile = filepath.Join(t.TempDir(), "absent")
	err := spec.ValidateControllerStackReferences()
	if err == nil || !strings.Contains(err.Error(), "controllerStack.tenant.passwordFile") || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("missing file: %v", err)
	}
}

func TestEffectiveTrustDomainAndEndpoint(t *testing.T) {
	spec := baseSpec(t)
	spec.Spire.TrustDomain = ""
	if got := spec.EffectiveTrustDomain("nvkectl-bootstrap"); got != "nvkectl-bootstrap.nke.local" {
		t.Errorf("default trust domain: %s", got)
	}
	spec.Spire.TrustDomain = "site.example.com"
	if got := spec.EffectiveTrustDomain("ignored"); got != "site.example.com" {
		t.Errorf("explicit trust domain: %s", got)
	}
	spec.Cluster.ControlPlaneVIP = "10.0.0.5"
	if ep, err := spec.ControllerEndpoint(); err != nil || ep != "10.0.0.5" {
		t.Errorf("VIP endpoint: %q %v", ep, err)
	}
	spec.Cluster.ControlPlaneVIP = ""
	if _, err := spec.ControllerEndpoint(); err == nil {
		t.Error("several nodes without a VIP accepted")
	}
	spec.Cluster.Nodes = spec.Cluster.Nodes[:1]
	if ep, err := spec.ControllerEndpoint(); err != nil || ep != spec.Cluster.Nodes[0].IP {
		t.Errorf("single node endpoint: %q %v", ep, err)
	}
}

// Relative file paths in the section resolve from the site spec's directory,
// not from wherever the command runs.
func TestValidateControllerStackReferences_RelativeToTheSpec(t *testing.T) {
	specDir := t.TempDir()
	for _, name := range []string{"nico-token", "tenant-password"} {
		if err := os.WriteFile(filepath.Join(specDir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := strings.NewReplacer(
		"REPLACE_NICO_TOKEN_PATH", "nico-token",
		"REPLACE_TENANT_PASSWORD_PATH", "tenant-password",
		"REPLACE_DB_PASSWORD_PATH", "db-password",
		"REPLACE_ARCH", "arm64",
		"REPLACE_NICO_ENDPOINT", "https://nico.example.com",
		"REPLACE_NICO_ORG_ID", "org",
		"REPLACE_NICO_API_NAME", "forge",
		"REPLACE_TENANT_USER", "nke",
		"  # controlPlaneVIP: \"10.0.0.100\"", "  controlPlaneVIP: \"10.0.0.100\"",
	).Replace(string(templateYAML))
	path := filepath.Join(specDir, "sitespec.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	spec, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.ValidateControllerStackReferences(); err != nil {
		t.Errorf("relative secret paths not resolved from the spec: %v", err)
	}
	if got := spec.ResolvePath("nico-token"); got != filepath.Join(specDir, "nico-token") {
		t.Errorf("ResolvePath: %s", got)
	}
	if got := spec.ResolvePath("/abs/x"); got != "/abs/x" {
		t.Errorf("ResolvePath absolute: %s", got)
	}
	// A loaded, filled-in spec renders back to itself.
	if _, err := spec.Render(); err != nil {
		t.Errorf("Render of a loaded spec with a filled controllerStack: %v", err)
	}
}

// A loaded spec without the section renders without it: the template's
// placeholder block is removed, so the file says what the spec says.
func TestRender_LoadedSpecWithoutTheSection(t *testing.T) {
	body := string(templateYAML)
	body = body[:strings.Index(body, "\ncontrollerStack:\n")+1]
	body = strings.Replace(body, "  # controlPlaneVIP: \"10.0.0.100\"", "  controlPlaneVIP: \"10.0.0.100\"", 1)
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ControllerStack != nil {
		t.Fatal("the section was decoded from a file that has none")
	}
	rendered, err := spec.Render()
	if err != nil {
		t.Fatalf("Render without the section: %v", err)
	}
	if strings.Contains(string(rendered), "controllerStack:") {
		t.Errorf("rendered file carries a section the spec does not have:\n%s", rendered)
	}
}
