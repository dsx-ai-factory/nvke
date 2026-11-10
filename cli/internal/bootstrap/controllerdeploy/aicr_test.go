// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

func TestSeedValues_CarryTheSiteAndNothingTheCatalogDefaults(t *testing.T) {
	cfg := Config{
		CWEImage:          "registry.example/nke-controller:dev",
		SPIFFEClusterName: "site-a",
		SPIFFETrustDomain: "site-a.example",
		TPMEKFingerprints: []string{"aa", "bb"},
		NodeAgentPort:     "15005",
		CWEPort:           31000,
	}
	values, err := cfg.SeedValues("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	spire := values["spire"]["spire-server"].(map[string]any)
	if ids := spire["adminIDs"].([]any); ids[0] != "spiffe://site-a.example/nvke-service/nke-controller" {
		t.Errorf("adminIDs = %v", ids)
	}
	tpm := spire["nodeAttestor"].(map[string]any)["tpmDirect"].(map[string]any)
	if len(tpm["hashes"].([]any)) != 2 {
		t.Errorf("tpm hashes = %v", tpm["hashes"])
	}
	ctl := values["nke-controller"]
	if got := ctl["service"].(map[string]any)["port"]; got != 31000 {
		t.Errorf("service port = %v", got)
	}
	if got := values["nvke-seed-config"]["controllerPort"]; got != 31000 {
		t.Errorf("seed NodePort = %v", got)
	}
	img := ctl["image"].(map[string]any)
	if img["repository"] != "registry.example/nke-controller" || img["tag"] != "dev" {
		t.Errorf("image = %v", img)
	}
	if env := ctl["env"].([]any)[0].(map[string]any); env["name"] != "NODE_AGENT_PORT" || env["value"] != "15005" {
		t.Errorf("env = %v", env)
	}
	if values["postgres"]["auth"].(map[string]any)["password"] != "s3cret" {
		t.Error("postgres password missing")
	}
}

func TestSeedValues_NoThumbprintsMeansNoTPMAttestor(t *testing.T) {
	values, err := Config{CWEImage: "x:y"}.SeedValues("p")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["spire"]["spire-server"].(map[string]any)["nodeAttestor"]; ok {
		t.Error("a TPM attestor without thumbprints")
	}
	if td := values["spire"]["global"].(map[string]any)["spire"].(map[string]any)["trustDomain"]; !strings.HasSuffix(td.(string), ".nke.local") {
		t.Errorf("trust domain = %v", td)
	}
}

func TestSeedValues_SelectTheProvisionedProvider(t *testing.T) {
	values, err := testConfig(t).SeedValues("p")
	if err != nil {
		t.Fatal(err)
	}
	for _, comp := range []string{"provisioned-provider", "capnico"} {
		if got := values[comp]["provider"]; got != "provisioned" {
			t.Errorf("%s provider = %v, want provisioned", comp, got)
		}
	}
	if got := values["provisioned-provider"]["image"]; got != "cluster-api-provider-provisioned:latest" {
		t.Errorf("provisioned provider image = %v", got)
	}
	if _, ok := values["capnico"]["nicoCredentials"]; ok {
		t.Error("NICo credentials for a provisioned site")
	}
	if _, ok := values["nvke-seed-config"]["dgxcTenant"]; ok {
		t.Error("tenant credentials for a provisioned site")
	}
	if _, ok := values["nke-controller"]["supportedInfraProviders"]; ok {
		t.Error("the catalog's supportedInfraProviders default was overridden for a provisioned site")
	}
}

func TestSeedValues_SelectNICoAndGiveItsCredentialsAndAddress(t *testing.T) {
	values, err := nicoTestConfig(t).SeedValues("p")
	if err != nil {
		t.Fatal(err)
	}
	for _, comp := range []string{"provisioned-provider", "capnico"} {
		if got := values[comp]["provider"]; got != "nico" {
			t.Errorf("%s provider = %v, want nico", comp, got)
		}
	}
	creds := values["capnico"]["nicoCredentials"].(map[string]any)
	if creds["endpoint"] != "https://nico.example" || creds["token"] != "example-nico-token" || creds["orgID"] != "example-org" || creds["apiName"] != "forge" {
		t.Errorf("nicoCredentials = %v", creds)
	}
	tenant := values["nvke-seed-config"]["dgxcTenant"].(map[string]any)
	if tenant["user"] != "example-tenant" || tenant["password"] != "example-tenant-password" {
		t.Errorf("dgxcTenant = %v", tenant)
	}
	ctl := values["nke-controller"]
	if ctl["supportedInfraProviders"] != "nico" {
		t.Errorf("supportedInfraProviders = %v", ctl["supportedInfraProviders"])
	}
	if got := ctl["secrets"].(map[string]any)["dgxcTenantCredSecretName"]; got != "dgxc-tenant-credentials" {
		t.Errorf("tenant secret name = %v", got)
	}
	spire := ctl["spiffe"].(map[string]any)["spire"].(map[string]any)
	if spire["serverAddress"] != "192.0.2.10" || spire["serverPort"] != "30081" {
		t.Errorf("controller SPIRE address = %v", spire)
	}
	// No thumbprints: the attestor is on with an empty allowlist the controller fills, read from the ConfigMap.
	server := values["spire"]["spire-server"].(map[string]any)
	tpm := server["nodeAttestor"].(map[string]any)["tpmDirect"].(map[string]any)
	if tpm["enabled"] != true || tpm["hashes"] != nil {
		t.Errorf("tpmDirect = %v", tpm)
	}
	if _, ok := server["extraVolumes"]; !ok {
		t.Error("the allowlist ConfigMap is not mounted")
	}
}

func TestSeedValues_NICoNeedsItsAccessAndAnExternalAddress(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"token":    func(c *Config) { c.CAPNICo.Token = "" },
		"tenant":   func(c *Config) { c.DGXCTenant.Password = "" },
		"external": func(c *Config) { c.SPIREExternalAddress = "" },
	} {
		cfg := nicoTestConfig(t)
		edit(&cfg)
		if _, err := cfg.SeedValues("p"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSeedValues_ThePodVIPOverrideOIDCAndProfilesReachTheController(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowControlPlaneWithoutVIP = true
	cfg.ProfilesImage = "ghcr.io/example/profiles:v1"
	cfg.OIDCProviders = []sitespec.OIDCProvider{{Name: "corp", IssuerURL: "https://issuer.example", Audiences: []string{"nke"}, CABundle: "PEM"}}
	values, err := cfg.SeedValues("p")
	if err != nil {
		t.Fatal(err)
	}
	ctl := values["nke-controller"]
	if ctl["allowControlPlaneWithoutVIP"] != true {
		t.Error("the VIP override did not reach the controller")
	}
	if img := ctl["profiles"].(map[string]any)["image"].(map[string]any); img["enabled"] != true || img["repository"] != "ghcr.io/example/profiles" {
		t.Errorf("profiles image = %v", img)
	}
	providers := ctl["oidc"].(map[string]any)["providers"].([]any)
	if len(providers) != 1 || providers[0].(map[string]any)["issuerURL"] != "https://issuer.example" {
		t.Errorf("oidc providers = %v", providers)
	}
}

func TestSetFileArgs_PutValuesInFilesNotArguments(t *testing.T) {
	dir := t.TempDir()
	args, err := setFileArgs(dir, map[string]map[string]any{"postgres": {"auth": map[string]any{"password": "hunter2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "--set-file" || !strings.HasPrefix(args[1], "postgres:auth=") {
		t.Fatalf("args = %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "hunter2") {
		t.Error("the password is an argument")
	}
	body, err := os.ReadFile(strings.TrimPrefix(args[1], "postgres:auth="))
	if err != nil || !strings.Contains(string(body), "hunter2") {
		t.Fatalf("value file: %q %v", body, err)
	}
}

func TestDeployWithAICR_RunsClusterctlThenAicrThenTheBundle(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{{err: errors.New("NotFound")}, {}}}
	cfg := testConfig(t)
	if err := New(runner, nil).DeployWithAICR(context.Background(), "/tmp/kubeconfig", cfg, AICRConfig{Catalog: "/cat", Bin: "aicr"}); err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, c := range runner.calls {
		key := c.name
		if c.name == "aicr" {
			key += " " + c.args[0]
		}
		seq = append(seq, key)
		if c.name == "helm" {
			t.Errorf("nvkectl installed a release itself: %v", c.args)
		}
	}
	want := "kubectl clusterctl aicr recipe aicr bundle env"
	if strings.Join(seq, " ") != want {
		t.Errorf("calls = %q, want %q", strings.Join(seq, " "), want)
	}
}

func TestDeployWithAICR_Refusals(t *testing.T) {
	a := AICRConfig{Catalog: "/cat", Bin: "aicr"}
	for name, tc := range map[string]struct {
		edit func(*Config, *AICRConfig)
		want string
	}{
		"no catalog":  {func(_ *Config, a *AICRConfig) { a.Catalog = "" }, "catalog"},
		"no image":    {func(c *Config, _ *AICRConfig) { c.CWEImage = "" }, "CWEImage"},
		"no provider": {func(c *Config, _ *AICRConfig) { c.ProvisionedProviderImage = "" }, "ProvisionedProviderImage"},
	} {
		cfg, ac := testConfig(t), a
		tc.edit(&cfg, &ac)
		err := New(&fakeRunner{}, nil).DeployWithAICR(context.Background(), "/tmp/kubeconfig", cfg, ac)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}
