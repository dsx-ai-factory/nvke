// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestControlPlaneConfigSpecProviderSettersMirrorFlatFields(t *testing.T) {
	instanceTypeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	subnetID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	vpcPrefixID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	sshKeyGroupID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	machineTemplate := json.RawMessage(`{"kind":"ForgeMachineTemplate"}`)

	var nicoCfg db.ControlPlaneConfigSpec
	nicoCfg.SetNico(db.NicoControlPlaneProvider{
		InstanceTypePreference: []uuid.UUID{instanceTypeID},
		SubnetIDs:              []uuid.UUID{subnetID},
		VpcPrefixIDs:           []uuid.UUID{vpcPrefixID},
		OSProfile:              "nke-260318",
		DiskSelectionStrategy:  db.DiskSelectionStrategyLargest,
		ConfigProfile:          "gpu",
		SSHKeyGroupIDs:         []uuid.UUID{sshKeyGroupID},
	})

	require.NotNil(t, nicoCfg.Nico)
	require.Nil(t, nicoCfg.Capi)
	assert.Equal(t, []uuid.UUID{instanceTypeID}, nicoCfg.Nico.InstanceTypePreference)
	assert.Equal(t, []uuid.UUID{subnetID}, nicoCfg.Nico.SubnetIDs)
	assert.Equal(t, []uuid.UUID{vpcPrefixID}, nicoCfg.Nico.VpcPrefixIDs)
	assert.Equal(t, "nke-260318", nicoCfg.Nico.OSProfile)
	assert.Equal(t, db.DiskSelectionStrategyLargest, nicoCfg.Nico.DiskSelectionStrategy)
	assert.Equal(t, "gpu", nicoCfg.Nico.ConfigProfile)
	assert.Equal(t, []uuid.UUID{sshKeyGroupID}, nicoCfg.Nico.SSHKeyGroupIDs)

	var capiCfg db.ControlPlaneConfigSpec
	capiCfg.SetCapi(db.CAPIControlPlaneProvider{
		InfraMachineTemplatePreferences: []json.RawMessage{machineTemplate},
		SpireAttestationMethod:          db.SpireAttestationMethodJoinToken,
		BootstrapFiles:                  []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}},
		PreKubeadmCommands:              []string{"echo pre"},
	})

	require.Nil(t, capiCfg.Nico)
	require.NotNil(t, capiCfg.Capi)
	require.Len(t, capiCfg.Capi.InfraMachineTemplatePreferences, 1)
	assert.JSONEq(t, string(machineTemplate), string(capiCfg.Capi.InfraMachineTemplatePreferences[0]))
	assert.Equal(t, db.SpireAttestationMethodJoinToken, capiCfg.Capi.SpireAttestationMethod)
	assert.Equal(t, []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}}, capiCfg.Capi.BootstrapFiles)
	assert.Equal(t, []string{"echo pre"}, capiCfg.Capi.PreKubeadmCommands)
}

func TestControlPlaneConfigSpecProviderAccessors(t *testing.T) {
	instanceTypeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	subnetID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	machineTemplate := json.RawMessage(`{"kind":"ForgeMachineTemplate"}`)
	cfg := db.ControlPlaneConfigSpec{
		Nico: &db.NicoControlPlaneProvider{
			InstanceTypePreference: []uuid.UUID{instanceTypeID},
			SubnetIDs:              []uuid.UUID{subnetID},
		},
		Capi: &db.CAPIControlPlaneProvider{
			InfraMachineTemplatePreferences: []json.RawMessage{machineTemplate},
			PreKubeadmCommands:              []string{"echo pre"},
		},
	}

	nico, err := cfg.NicoProvider()
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{instanceTypeID}, nico.InstanceTypePreference)
	nico.InstanceTypePreference[0] = uuid.New()
	assert.Equal(t, []uuid.UUID{instanceTypeID}, cfg.Nico.InstanceTypePreference)

	capi, err := cfg.CAPIProvider()
	require.NoError(t, err)
	require.Len(t, capi.InfraMachineTemplatePreferences, 1)
	assert.JSONEq(t, string(machineTemplate), string(capi.InfraMachineTemplatePreferences[0]))
	capi.PreKubeadmCommands[0] = "mutated"
	assert.Equal(t, []string{"echo pre"}, cfg.Capi.PreKubeadmCommands)
}

func TestCreateControlPlaneConfigRowIfChanged_StoresProviderPayload(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.ControlPlaneConfiguration)(nil))
	dbImpl := db.NewDB(bunDB)
	cluster := newControlPlaneConfigTestCluster()
	createdCluster, err := dbImpl.CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	instanceTypeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	subnetID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	snapshotCfg := db.ControlPlaneConfigSpec{}
	snapshotCfg.SetNico(db.NicoControlPlaneProvider{
		InstanceTypePreference: []uuid.UUID{instanceTypeID},
		SubnetIDs:              []uuid.UUID{subnetID},
		OSProfile:              "nke-260318",
		ConfigProfile:          "gpu",
	})

	require.NoError(t, db.CreateControlPlaneConfigRowIfChanged(ctx, dbImpl, &createdCluster, snapshotCfg, db.CreateControlPlaneConfigRowOpts{
		ChangeReason: "test provider sync",
	}))

	row, err := dbImpl.GetControlPlaneConfiguration(ctx, createdCluster.ControlPlaneConfig.ConfigurationID)
	require.NoError(t, err)
	require.NotNil(t, row.Config.Nico)
	require.Nil(t, row.Config.Capi)
	assert.Equal(t, []uuid.UUID{instanceTypeID}, row.Config.Nico.InstanceTypePreference)
	assert.Equal(t, []uuid.UUID{subnetID}, row.Config.Nico.SubnetIDs)
	assert.Equal(t, "nke-260318", row.Config.Nico.OSProfile)
	assert.Equal(t, "gpu", row.Config.Nico.ConfigProfile)
}

// withNicoProfiles returns spec with the nico payload's OS and config profiles
// replaced, which is where the rollout diff reads them from.
func withNicoProfiles(spec db.ControlPlaneConfigSpec, osProfile, configProfile string) db.ControlPlaneConfigSpec {
	spec.SetNico(db.NicoControlPlaneProvider{OSProfile: osProfile, ConfigProfile: configProfile})
	return spec
}

func TestIsControlPlaneRolloutRequired(t *testing.T) {
	base := withNicoProfiles(db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}, "os-v1", "cfg-v1")

	tests := []struct {
		name string
		mut  func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec
		want bool
	}{
		{
			name: "identical",
			mut:  func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec { return s },
			want: false,
		},
		{
			name: "kubernetes version differs",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				s.KubernetesVersion = "1.34.3"
				return s
			},
			want: true,
		},
		{
			name: "os profile differs",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				return withNicoProfiles(s, "os-v2", "cfg-v1")
			},
			want: true,
		},
		{
			name: "config profile differs",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				return withNicoProfiles(s, "os-v1", "cfg-v2")
			},
			want: true,
		},
		{
			name: "non-rollout field differs is ignored",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				s.SystemReservedMemory = "32Gi"
				s.TeleportEnabled = true
				return s
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := db.IsControlPlaneRolloutRequired(base, tt.mut(base))
			assert.Equal(t, tt.want, got)
			// The predicate is symmetric.
			assert.Equal(t, tt.want, db.IsControlPlaneRolloutRequired(tt.mut(base), base))
		})
	}
}

func TestComputeControlPlaneRolloutChanges(t *testing.T) {
	base := withNicoProfiles(db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}, "os-v1", "cfg-v1")

	tests := []struct {
		name string
		mut  func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec
		want []db.ControlPlaneRolloutChange
	}{
		{
			name: "identical returns nil",
			mut:  func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec { return s },
			want: nil,
		},
		{
			name: "kubernetes version differs",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				s.KubernetesVersion = "1.34.3"
				return s
			},
			want: []db.ControlPlaneRolloutChange{{Field: "kubernetesVersion", From: "1.34.2", To: "1.34.3"}},
		},
		{
			name: "os profile differs",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				return withNicoProfiles(s, "os-v2", "cfg-v1")
			},
			want: []db.ControlPlaneRolloutChange{{Field: "osProfile", From: "os-v1", To: "os-v2"}},
		},
		{
			name: "config profile differs",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				return withNicoProfiles(s, "os-v1", "cfg-v2")
			},
			want: []db.ControlPlaneRolloutChange{{Field: "configProfile", From: "cfg-v1", To: "cfg-v2"}},
		},
		{
			name: "all rollout fields differ, in stable order",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				s.KubernetesVersion = "1.34.3"
				return withNicoProfiles(s, "os-v2", "cfg-v2")
			},
			want: []db.ControlPlaneRolloutChange{
				{Field: "kubernetesVersion", From: "1.34.2", To: "1.34.3"},
				{Field: "osProfile", From: "os-v1", To: "os-v2"},
				{Field: "configProfile", From: "cfg-v1", To: "cfg-v2"},
			},
		},
		{
			name: "non-rollout field differs is ignored",
			mut: func(s db.ControlPlaneConfigSpec) db.ControlPlaneConfigSpec {
				s.SystemReservedMemory = "32Gi"
				s.TeleportEnabled = true
				return s
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := db.ComputeControlPlaneRolloutChanges(base, tt.mut(base))
			assert.Equal(t, tt.want, got)
			// IsControlPlaneRolloutRequired is a thin predicate over the diff and must agree.
			assert.Equal(t, len(tt.want) > 0, db.IsControlPlaneRolloutRequired(base, tt.mut(base)))
		})
	}
}

func newControlPlaneConfigTestCluster() db.Cluster {
	now := time.Now()
	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              fmt.Sprintf("cluster-%s", uuid.New().String()),
		KubernetesVersion: "1.29",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{
		OrgID:  "org1",
		SiteID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		VpcID:  uuid.New(),
	})
	return cluster
}
