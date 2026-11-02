// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
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

// The workflow seds the jump hosts and the SSA issuer out of the text files it
// uploads; the suite has to carry the same values into the archives it
// writes, raw and without the trailing slash the workflow also strips.
func TestWithheldFromEnvironment(t *testing.T) {
	t.Setenv("FORGE_JUMP_HOST", "jump.example.test/")
	t.Setenv("DSX_DATAHALL_1_JUMP_HOST", "10.1.2.3")
	t.Setenv("SSA_ISSUER", "https://issuer.example.test/")
	t.Setenv("JUMP_HOST_PASSWORD", "hunter2hunter2")
	t.Setenv("FORGE_API_KEY", "not-withheld-by-this-list")

	got := strings.Join(withheldFromEnvironment(), "\n")
	for _, want := range []string{"jump.example.test/", "jump.example.test", "10.1.2.3", "https://issuer.example.test/", "https://issuer.example.test", "hunter2hunter2"} {
		if !strings.Contains(got, want) {
			t.Errorf("withheldFromEnvironment lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not-withheld-by-this-list") {
		t.Errorf("withheldFromEnvironment carries the API key, which newStack names instead:\n%s", got)
	}
}
