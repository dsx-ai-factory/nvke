// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/dsx"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/forge"
)

// A full run's tenant half speaks Forge and only Forge, so any other provider
// fails at the tenant however healthy its hardware is. The check is worth
// having because of what the failure costs: a machine held through provision,
// bootstrap and deploy first, and on redstone-dev1-dh2 that is the only CPU
// machine the datahall has.
func TestFullStageSupported(t *testing.T) {
	if err := fullStageSupported(forge.ProviderName); err != nil {
		t.Errorf("full stage on Forge = %v, want supported -- Forge is the provider the tenant spec is written for", err)
	}

	for _, provider := range []string{dsx.ProviderName, "some-provider-added-later"} {
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
			for _, stage := range []string{siteStageProvision, siteStageBootstrap} {
				if !strings.Contains(err.Error(), stage) {
					t.Errorf("error does not offer stage %q: %v", stage, err)
				}
			}
		})
	}
}
