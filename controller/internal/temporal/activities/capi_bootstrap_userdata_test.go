// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		BootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/custom.conf", Owner: rootRoot, Permissions: "0600", Content: "hello"},
		},
		PreKubeadmCommands: []string{"echo before kubeadm"},
	})
	userData, err := activities.renderCAPIBootstrapUserData(context.Background(), nodeSet, "")
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

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{SpireAttestationMethod: db.SpireAttestationMethodJoinToken})
	userData, err := activities.renderCAPIBootstrapUserData(context.Background(), nodeSet, "join-token-123")
	require.NoError(t, err)

	assert.Contains(t, userData, `join_token = "join-token-123"`)
	assert.Contains(t, userData, `NodeAttestor "join_token"`)
	assert.NotContains(t, userData, `NodeAttestor "tpm"`)
}

func TestRenderCAPIBootstrapUserData_JoinTokenRequiresToken(t *testing.T) {
	activities := &Activities{cfg: config.Config{}}

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{SpireAttestationMethod: db.SpireAttestationMethodJoinToken})
	_, err := activities.renderCAPIBootstrapUserData(context.Background(), nodeSet, "")
	require.EqualError(t, err, "spire join token is required when capiSpireAttestationMethod is joinToken")
}

// The "none" method writes no SPIRE material. It must also not call the trust
// bundle source at all: a bootstrap has no SPIRE server to answer.
func TestRenderCAPIBootstrapUserData_NoneAttestor(t *testing.T) {
	originalGetTrustDomainAndBundle := getTrustDomainAndBundle
	t.Cleanup(func() {
		getTrustDomainAndBundle = originalGetTrustDomainAndBundle
	})
	trustBundleCalls := 0
	getTrustDomainAndBundle = func(context.Context, config.Config) (string, string, error) {
		trustBundleCalls++
		return "example.org", "-----BEGIN CERT-----\nabc123\n-----END CERT-----", nil
	}

	activities := &Activities{
		cfg: config.Config{
			SpireServerAddress: "spire.example.org",
			SpireServerPort:    "9091",
		},
	}

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{
		SpireAttestationMethod: db.SpireAttestationMethodNone,
		BootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/custom.conf", Owner: rootRoot, Permissions: "0600", Content: "hello"},
		},
		PreKubeadmCommands: []string{"echo before kubeadm"},
	})
	userData, err := activities.renderCAPIBootstrapUserData(context.Background(), nodeSet, "")
	require.NoError(t, err)

	assert.Equal(t, 0, trustBundleCalls)
	assert.Contains(t, userData, "#cloud-config\n")
	assert.Contains(t, userData, "/etc/custom.conf")
	assert.Contains(t, userData, "echo before kubeadm")
	assert.NotContains(t, userData, "/etc/spire-agent/agent.conf")
	assert.NotContains(t, userData, "/etc/spire-agent/initial-trust-bundle.pem")
	var parsed map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(userData), &parsed))
}

func TestSpiffeEnabledForAttestationMethod(t *testing.T) {
	assert.True(t, db.SpiffeEnabledForAttestationMethod(db.SpireAttestationMethodTPM))
	assert.True(t, db.SpiffeEnabledForAttestationMethod(db.SpireAttestationMethodJoinToken))
	// A Forge node pool leaves the method empty and is unaffected.
	assert.True(t, db.SpiffeEnabledForAttestationMethod(""))
	assert.False(t, db.SpiffeEnabledForAttestationMethod(db.SpireAttestationMethodNone))
}
