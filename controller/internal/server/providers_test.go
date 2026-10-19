// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
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

func TestValidateNodeAgentProfile(t *testing.T) {
	t.Parallel()

	spiffeTPM := db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM}
	spiffeDSXIMDS := db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS}
	var vmPool db.NodePool
	vmPool.SetLaunchLayer(db.LaunchLayerNodePoolProvider{InstanceType: "vm200", VMProfile: "ubuntu"})
	tests := map[string]struct {
		profile  db.Node
		provider db.ProviderType
		nodePool db.NodePool
		wantErr  string
	}{
		"launchLayer on a SPIFFE tpm deployment is rejected": {
			profile:  spiffeTPM,
			provider: db.ProviderTypeLaunchLayer,
			wantErr: "this deployment's node-agent settings do not support the launchlayer provider: " +
				"SPIFFE-enabled LaunchLayer nodes require dsxImds SPIRE attestation, got tpm",
		},
		"launchLayer on a SPIFFE dsxImds deployment is accepted": {
			profile:  spiffeDSXIMDS,
			provider: db.ProviderTypeLaunchLayer,
		},
		"launchLayer vmProfile pool on a SPIFFE dsxImds deployment is rejected": {
			profile:  spiffeDSXIMDS,
			provider: db.ProviderTypeLaunchLayer,
			nodePool: vmPool,
			wantErr: "this deployment's node-agent settings do not support the launchlayer provider: " +
				`SPIFFE-enabled LaunchLayer nodes without external mTLS require hostOSProfile: dsxImds attestation is not available to vmProfile "ubuntu" instances`,
		},
		"launchLayer vmProfile pool with external mTLS is accepted": {
			profile:  db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS, NodeAgentExternalMTLSEnabled: true},
			provider: db.ProviderTypeLaunchLayer,
			nodePool: vmPool,
		},
		"launchLayer vmProfile pool without SPIFFE is accepted": {
			provider: db.ProviderTypeLaunchLayer,
			nodePool: vmPool,
		},
		"nico on a SPIFFE tpm deployment is accepted": {
			profile:  spiffeTPM,
			provider: db.ProviderTypeNico,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := validateNodeAgentProfile(tt.profile, db.Cluster{ProviderType: tt.provider}, tt.nodePool)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}
