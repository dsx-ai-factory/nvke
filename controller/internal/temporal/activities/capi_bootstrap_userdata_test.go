// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestRenderCAPIBootstrapUserData_TPMAttestor(t *testing.T) {
	originalGetTrustDomainAndBundle := getTrustDomainAndBundle
	t.Cleanup(func() {
		getTrustDomainAndBundle = originalGetTrustDomainAndBundle
	})
	getTrustDomainAndBundle = func(context.Context, config.Config) (string, string, error) {
		return "example.org", "-----BEGIN CERT-----\nabc123\n-----END CERT-----", nil
	}

	activities := &Activities{
		cfg: config.Config{
			SpireServerAddress: "spire.example.org",
			SpireServerPort:    "9091",
		},
	}

	userData, err := activities.renderCAPIBootstrapUserData(context.Background(), db.NodeSet{
		CapiSpireAttestationMethod: db.SpireAttestationMethodTPM,
		CapiBootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/custom.conf", Owner: rootRoot, Permissions: "0600", Content: "hello"},
		},
		CapiPreKubeadmCommands: []string{"echo before kubeadm"},
	}, "")
	require.NoError(t, err)

	assert.Contains(t, userData, "#cloud-config\n")
	assert.Contains(t, userData, "/etc/custom.conf")
	assert.Contains(t, userData, "/etc/spire-agent/agent.conf")
	assert.Contains(t, userData, "/etc/spire-agent/initial-trust-bundle.pem")
	assert.Contains(t, userData, `NodeAttestor "tpm"`)
	assert.NotContains(t, userData, `NodeAttestor "join_token"`)
	assert.Contains(t, userData, "echo before kubeadm")
	var parsed map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(userData), &parsed))
}

func TestRenderCAPIBootstrapUserData_JoinTokenAttestor(t *testing.T) {
	originalGetTrustDomainAndBundle := getTrustDomainAndBundle
	t.Cleanup(func() {
		getTrustDomainAndBundle = originalGetTrustDomainAndBundle
	})
	getTrustDomainAndBundle = func(context.Context, config.Config) (string, string, error) {
		return "example.org", "bundle", nil
	}

	activities := &Activities{
		cfg: config.Config{
			SpireServerAddress: "spire.example.org",
			SpireServerPort:    "9091",
		},
	}

	userData, err := activities.renderCAPIBootstrapUserData(context.Background(), db.NodeSet{
		CapiSpireAttestationMethod: db.SpireAttestationMethodJoinToken,
	}, "join-token-123")
	require.NoError(t, err)

	assert.Contains(t, userData, `join_token = "join-token-123"`)
	assert.Contains(t, userData, `NodeAttestor "join_token"`)
	assert.NotContains(t, userData, `NodeAttestor "tpm"`)
}

func TestRenderCAPIBootstrapUserData_JoinTokenRequiresToken(t *testing.T) {
	activities := &Activities{cfg: config.Config{}}

	_, err := activities.renderCAPIBootstrapUserData(context.Background(), db.NodeSet{
		CapiSpireAttestationMethod: db.SpireAttestationMethodJoinToken,
	}, "")
	require.EqualError(t, err, "spire join token is required when capiSpireAttestationMethod is joinToken")
}
