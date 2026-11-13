// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/dsx"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/forge"
)

// A full run's tenant half speaks the dialects it has specs for, so a provider
// without one fails at the tenant however healthy its hardware is. The check is
// worth having because of what the failure costs: machines held through
// provision, bootstrap and deploy first.
func TestFullStageSupported(t *testing.T) {
	for _, provider := range []string{forge.ProviderName, dsx.ProviderName} {
		if err := fullStageSupported(provider); err != nil {
			t.Errorf("full stage on %q = %v, want supported -- it has a tenant spec", provider, err)
		}
	}

	for _, provider := range []string{"some-provider-added-later"} {
		t.Run(provider, func(t *testing.T) {
			err := fullStageSupported(provider)
			if err == nil {
				t.Fatalf("full stage on %q = nil, want refused before a machine is taken", provider)
			}
			// The message is the whole value of failing here rather than at
			// the tenant: it has to say which provider, and what to run instead.
			if !strings.Contains(err.Error(), provider) {
				t.Errorf("error does not name the provider: %v", err)
			}
			for _, stage := range []string{siteStageProvision, siteStageBootstrap, siteStageDeployment} {
				if !strings.Contains(err.Error(), stage) {
					t.Errorf("error does not offer stage %q: %v", stage, err)
				}
			}
		})
	}
}

// The workflow seds the jump hosts, the SSA issuer and the DSX API hosts out of
// the text files it uploads; the suite has to carry the same values into the
// archives it writes, raw, without the trailing slash the workflow also strips,
// and as the host alone.
func TestWithheldFromEnvironment(t *testing.T) {
	t.Setenv("FORGE_JUMP_HOST", "jump.example.test/")
	t.Setenv("DSX_DATAHALL_1_JUMP_HOST", "10.1.2.3")
	t.Setenv("SSA_ISSUER", "https://issuer.example.test/")
	t.Setenv("DSX_DATAHALL_3_CA_CERT", "-----BEGIN CERTIFICATE-----\nhall3\n-----END CERTIFICATE-----\n")
	t.Setenv("DSX_DATAHALL_3_API", "https://api.hall3.example.test:443/")
	t.Setenv("DSX_DATAHALL_3_SITE_ID", "hall3-site-not-withheld")
	t.Setenv("JUMP_HOST_PASSWORD", "hunter2hunter2")
	t.Setenv("FORGE_API_KEY", "not-withheld-by-this-list")

	values := withheldFromEnvironment()
	got := strings.Join(values, "\n")
	for _, want := range []string{"jump.example.test/", "10.1.2.3", "https://issuer.example.test/", "hall3\n-----END CERTIFICATE-----", "hunter2hunter2", "https://api.hall3.example.test:443/"} {
		if !strings.Contains(got, want) {
			t.Errorf("withheldFromEnvironment lacks %q:\n%s", want, got)
		}
	}
	// Each derived form on its own, not as a substring of the raw value.
	for _, form := range []string{"jump.example.test", "https://issuer.example.test", "https://api.hall3.example.test:443", "api.hall3.example.test", "issuer.example.test"} {
		if !slices.Contains(values, form) {
			t.Errorf("withheldFromEnvironment lacks %q on its own:\n%s", form, got)
		}
	}
	if strings.Contains(got, "not-withheld-by-this-list") {
		t.Errorf("withheldFromEnvironment carries the API key, which newStack names instead:\n%s", got)
	}
	if strings.Contains(got, "hall3-site-not-withheld") {
		t.Errorf("withheldFromEnvironment carries a DSX id that is not an API host:\n%s", got)
	}
}

// A FLAT datahall's tenant needs NicoMachine autoNetwork, which the tenant
// spec does not send yet, so a full run there is refused up front.
func TestDSXTenantSupportedRefusesAFlatDatahall(t *testing.T) {
	// The lane runs this with the datahall's CA in its environment.
	t.Setenv(dsx.CACertEnvName("datahall-3"), "")
	flat := dsx.Site{
		API: "https://hall.example.test", TenantID: "t", VpcID: "v", SSHKeyGroupID: "k", AutoNetwork: true,
		InstanceTypes: []dsx.InstanceType{{
			Name: "tray", ID: "tray-type",
			Provides: map[string]string{"machineType": "gpu", "arch": "arm64", "gpu": "gb300"},
		}},
	}
	env := &dsx.Environment{Org: dsx.Org{ID: "ncx"}, Sites: map[string]dsx.Site{"datahall-3": flat}}
	err := dsxTenantSupported(env, map[string]string{"arch": "arm64", "gpu": "gb300"})
	if err == nil {
		t.Fatal("a full run on a FLAT datahall should be refused before a machine is taken")
	}
	for _, want := range []string{"datahall-3", siteStageDeployment} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}

	flat.AutoNetwork = false
	flat.VpcPrefixID = "p"
	env.Sites["datahall-3"] = flat
	if err := dsxTenantSupported(env, map[string]string{"arch": "arm64", "gpu": "gb300"}); err != nil {
		t.Errorf("a datahall with a prefix should be allowed, got %v", err)
	}
}

// A datahall with a prefix but a private CA is refused too: CAPNICo is not
// handed the CA, so its tenant would fail TLS against the datahall's API.
func TestDSXTenantSupportedRefusesADatahallWithAPrivateCA(t *testing.T) {
	hall := dsx.Site{
		API: "https://hall.example.test", TenantID: "t", VpcID: "v", SSHKeyGroupID: "k", VpcPrefixID: "p",
		InstanceTypes: []dsx.InstanceType{{
			Name: "tray", ID: "tray-type",
			Provides: map[string]string{"machineType": "gpu", "arch": "arm64", "gpu": "gb300"},
		}},
	}
	env := &dsx.Environment{Org: dsx.Org{ID: "ncx"}, Sites: map[string]dsx.Site{"datahall-3": hall}}
	capabilities := map[string]string{"arch": "arm64", "gpu": "gb300"}
	caEnv := dsx.CACertEnvName("datahall-3")

	t.Setenv(caEnv, " ")
	if err := dsxTenantSupported(env, capabilities); err != nil {
		t.Fatalf("a datahall on the system roots should be allowed, got %v", err)
	}

	t.Setenv(caEnv, "-----BEGIN CERTIFICATE-----")
	err := dsxTenantSupported(env, capabilities)
	if err == nil {
		t.Fatal("a full run on a datahall with a private CA should be refused before a machine is taken")
	}
	for _, want := range []string{"datahall-3", caEnv, siteStageDeployment} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}
