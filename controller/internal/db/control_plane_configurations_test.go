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

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
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
	assert.Equal(t, []uuid.UUID{instanceTypeID}, nicoCfg.InstanceTypePreference)
	assert.Equal(t, []uuid.UUID{subnetID}, nicoCfg.SubnetIDs)
	assert.Equal(t, []uuid.UUID{vpcPrefixID}, nicoCfg.VpcPrefixIDs)
	assert.Equal(t, "nke-260318", nicoCfg.OSProfile)
	assert.Equal(t, db.DiskSelectionStrategyLargest, nicoCfg.DiskSelectionStrategy)
	assert.Equal(t, "gpu", nicoCfg.ConfigProfile)
	assert.Equal(t, []uuid.UUID{sshKeyGroupID}, nicoCfg.SSHKeyGroupIDs)

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
	require.Len(t, capiCfg.InfraMachineTemplatePreferences, 1)
	assert.JSONEq(t, string(machineTemplate), string(capiCfg.InfraMachineTemplatePreferences[0]))
	assert.Equal(t, db.SpireAttestationMethodJoinToken, capiCfg.CapiSpireAttestationMethod)
	assert.Equal(t, []db.CloudInitFile{{Path: "/etc/test", Content: "ok"}}, capiCfg.CapiBootstrapFiles)
	assert.Equal(t, []string{"echo pre"}, capiCfg.CapiPreKubeadmCommands)
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

	nico := cfg.NicoControlPlaneProvider()
	require.NotNil(t, nico)
	assert.Equal(t, []uuid.UUID{instanceTypeID}, nico.InstanceTypePreference)
	nico.InstanceTypePreference[0] = uuid.New()
	assert.Equal(t, []uuid.UUID{instanceTypeID}, cfg.Nico.InstanceTypePreference)

	capi := cfg.CAPIControlPlaneProvider()
	require.NotNil(t, capi)
	require.Len(t, capi.InfraMachineTemplatePreferences, 1)
	assert.JSONEq(t, string(machineTemplate), string(capi.InfraMachineTemplatePreferences[0]))
	capi.PreKubeadmCommands[0] = "mutated"
	assert.Equal(t, []string{"echo pre"}, cfg.Capi.PreKubeadmCommands)
}

func TestControlPlaneConfigSpecProviderAccessors_ReturnNilForLegacyFlatOnly(t *testing.T) {
	cfg := db.ControlPlaneConfigSpec{
		InstanceTypePreference:          []uuid.UUID{uuid.New()},
		SubnetIDs:                       []uuid.UUID{uuid.New()},
		InfraMachineTemplatePreferences: []json.RawMessage{[]byte(`{"kind":"ForgeMachineTemplate"}`)},
	}

	assert.Nil(t, cfg.NicoControlPlaneProvider())
	assert.Nil(t, cfg.CAPIControlPlaneProvider())
}

func TestCreateControlPlaneConfigRowIfChanged_SyncsProviderPayload(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t, (*db.Cluster)(nil), (*db.ControlPlaneConfiguration)(nil))
	dbImpl := db.NewDB(bunDB)
	cluster := newControlPlaneConfigTestCluster()
	createdCluster, err := dbImpl.CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	instanceTypeID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	subnetID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	snapshotCfg := db.ControlPlaneConfigSpec{
		InstanceTypePreference: []uuid.UUID{instanceTypeID},
		SubnetIDs:              []uuid.UUID{subnetID},
		OSProfile:              "nke-260318",
		ConfigProfile:          "gpu",
	}

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
	assert.Equal(t, []uuid.UUID{instanceTypeID}, row.Config.InstanceTypePreference)
	assert.Equal(t, []uuid.UUID{subnetID}, row.Config.SubnetIDs)
}

func newControlPlaneConfigTestCluster() db.Cluster {
	now := time.Now()
	return db.Cluster{
		ID:                uuid.New(),
		Name:              fmt.Sprintf("cluster-%s", uuid.New().String()),
		OrgID:             "org1",
		SiteID:            uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		ProviderType:      db.ProviderTypeNico,
		VpcID:             uuid.New(),
		KubernetesVersion: "1.29",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}
