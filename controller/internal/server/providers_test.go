// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
)

func TestProviderSupport_Require(t *testing.T) {
	t.Parallel()

	type parameters struct {
		configured []string
		provider   string
		wantErr    string
	}

	tests := map[string]parameters{
		"default allows nico": {
			provider: config.InfraProviderNico,
		},
		"default allows infra": {
			provider: config.InfraProviderInfra,
		},
		"configured allows listed provider": {
			configured: []string{config.InfraProviderNico},
			provider:   config.InfraProviderNico,
		},
		"configured rejects unlisted provider": {
			configured: []string{config.InfraProviderNico},
			provider:   config.InfraProviderInfra,
			wantErr:    "infra provider is not supported by this deployment",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := newProviderSupport(params.configured).require(params.provider)
			if params.wantErr != "" {
				require.EqualError(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}
