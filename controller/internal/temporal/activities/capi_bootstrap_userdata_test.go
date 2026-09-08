// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
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
			SpiffeEnabled:      true,
			SpireServerAddress: "spire.example.org",
			SpireServerPort:    "9091",
		},
	}

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		BootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/custom.conf", Owner: "root:root", Permissions: "0600", Content: "hello"},
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
			SpiffeEnabled:      true,
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
	require.EqualError(t, err, "spire join token is required when infra.spireAttestationMethod is joinToken")
}

// A site stood up with --disable-mtls has no SPIRE, so there is no workload
// socket to read a bundle from. The node gets no bundle and no agent config,
// and rendering must not try to fetch one: the dial would fail, the activity
// would retry forever, and the cluster would never leave Provisioning.
func TestRenderCAPIBootstrapUserData_SpiffeDisabled(t *testing.T) {
	originalGetTrustDomainAndBundle := getTrustDomainAndBundle
	t.Cleanup(func() {
		getTrustDomainAndBundle = originalGetTrustDomainAndBundle
	})
	called := false
	getTrustDomainAndBundle = func(context.Context, config.Config) (string, string, error) {
		called = true
		return "", "", errors.New("workload API is not reachable without SPIRE")
	}

	activities := &Activities{cfg: config.Config{SpiffeEnabled: false}}

	var nodeSet db.NodeSet
	nodeSet.SetCapi(db.CAPINodePoolProvider{
		SpireAttestationMethod: db.SpireAttestationMethodTPM,
		BootstrapFiles: []db.CloudInitFile{
			{Path: "/etc/custom.conf", Owner: "root:root", Permissions: "0600", Content: "hello"},
		},
		PreKubeadmCommands: []string{"echo before kubeadm"},
	})

	userData, err := activities.renderCAPIBootstrapUserData(context.Background(), nodeSet, "")
	require.NoError(t, err)

	assert.False(t, called, "the workload API must not be dialed when SPIFFE is off")
	assert.NotContains(t, userData, "/etc/spire-agent/agent.conf")
	assert.NotContains(t, userData, "/etc/spire-agent/initial-trust-bundle.pem")
	assert.Contains(t, userData, "/etc/custom.conf")
	assert.Contains(t, userData, "echo before kubeadm")
}
