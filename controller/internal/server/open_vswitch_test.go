// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestValidateOpenVSwitchConfig(t *testing.T) {
	type testCase struct {
		name    string
		plugin  db.CniPlugin
		cfg     *db.OpenVSwitchConfig
		wantErr bool
	}
	tests := make([]testCase, 0, 15)
	tests = append(tests,
		testCase{name: "legacy omission", plugin: db.CNICalico},
		testCase{name: "fabric", plugin: db.CNIOvnKubernetes, cfg: &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}},
		testCase{name: "different CNI", plugin: db.CNICalico, cfg: &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}, wantErr: true},
		testCase{name: "missing uplink", plugin: db.CNIOvnKubernetes, cfg: &db.OpenVSwitchConfig{BridgeName: "br-eth1"}, wantErr: true},
		testCase{name: "missing bridge", plugin: db.CNIOvnKubernetes, cfg: &db.OpenVSwitchConfig{UplinkInterface: "eth1"}, wantErr: true},
		testCase{name: "same interface", plugin: db.CNIOvnKubernetes, cfg: &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "eth1"}, wantErr: true},
	)
	for _, name := range []string{"lo", ".", "..", "-eth1", "eth 1", "eth/1", "eth;id", "ethernet123456789", "éth1"} {
		tests = append(tests, testCase{name: "invalid " + name, plugin: db.CNIOvnKubernetes, cfg: &db.OpenVSwitchConfig{UplinkInterface: name, BridgeName: "br-eth1"}, wantErr: true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateOpenVSwitchConfig(tt.plugin, tt.cfg)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOpenVSwitchAPISchema(t *testing.T) {
	spec, err := api.GetSwagger()
	require.NoError(t, err)
	schema := spec.Components.Schemas["OpenVSwitchConfig"].Value
	for _, tt := range []struct {
		name  string
		value map[string]any
		valid bool
	}{
		{name: "pair", value: map[string]any{openVSwitchUplinkInterfaceField: "eth1", openVSwitchBridgeNameField: "br-eth1"}, valid: true},
		{name: "partial", value: map[string]any{openVSwitchUplinkInterfaceField: "eth1"}},
		{name: "empty", value: map[string]any{}},
		{name: "unknown field", value: map[string]any{openVSwitchUplinkInterfaceField: "eth1", openVSwitchBridgeNameField: "br-eth1", "addresses": []any{}}},
		{name: "loopback", value: map[string]any{openVSwitchUplinkInterfaceField: "lo", openVSwitchBridgeNameField: "br-eth1"}},
		{name: "unsafe", value: map[string]any{openVSwitchUplinkInterfaceField: "eth;id", openVSwitchBridgeNameField: "br-eth1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := schema.VisitJSON(tt.value)
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestOpenVSwitchConfigCreateUpdateAndRead(t *testing.T) {
	want := &api.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
	pool, err := dbNodePoolFromAPI(&api.CreateNodePoolRequest{ClusterID: uuid.New(), OpenVSwitch: want}, "1.36.0")
	require.NoError(t, err)
	require.Equal(t, &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}, pool.OpenVSwitch)
	assert.Equal(t, want, apiNodePoolFromDB(&pool).OpenVSwitch)
	cluster := db.Cluster{ProviderType: db.ProviderTypeCAPI, CniPlugin: db.CNIOvnKubernetes}
	require.NoError(t, applyUpdateNodePoolRequest(cluster, &pool, &api.UpdateNodePoolRequest{}))
	assert.Equal(t, want, apiOpenVSwitchFromDB(pool.OpenVSwitch))
	old := pool.OpenVSwitch
	replacement := &api.OpenVSwitchConfig{UplinkInterface: "eth2", BridgeName: "br-eth2"}
	require.NoError(t, applyUpdateNodePoolRequest(cluster, &pool, &api.UpdateNodePoolRequest{OpenVSwitch: replacement}))
	assert.Equal(t, replacement, apiOpenVSwitchFromDB(pool.OpenVSwitch))
	assert.Equal(t, "eth1", old.UplinkInterface)

	spec := controlPlaneConfigSpecFromCreateAPI(&api.CreateClusterControlPlaneConfig{OpenVSwitch: want, Infra: &api.InfraControlPlaneProvider{MachineTemplate: map[string]any{"kind": "ExampleMachineTemplate"}}}, "1.36.0", false, db.ProviderTypeCAPI)
	assert.Equal(t, want, apiControlPlaneConfigFromDB(db.ControlPlaneConfig{ControlPlaneConfigSpec: spec}).OpenVSwitch)
	service := &clusterService{}
	require.NoError(t, service.applyClusterLevelControlPlaneSpecUpdate(context.Background(), &cluster, &api.UpdateClusterControlPlaneConfig{OpenVSwitch: replacement}, &spec))
	assert.Equal(t, replacement, apiOpenVSwitchFromDB(spec.OpenVSwitch))
	assert.True(t, hasClusterLevelControlPlaneUpdate(&api.UpdateClusterControlPlaneConfig{OpenVSwitch: replacement}))
}

func TestCreateClusterRejectsInitialPoolOpenVSwitchBeforePersistence(t *testing.T) {
	for _, tt := range []struct {
		name    string
		plugin  string
		bridge  string
		message string
	}{
		{name: "non OVN plugin", plugin: "calico", bridge: "br-eth1", message: "requires the ovn-kubernetes CNI plugin"},
		{name: "same uplink and bridge", plugin: "ovn-kubernetes", bridge: "eth1", message: "must differ"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := validForgeCreateClusterRequest()
			req.ControlPlaneConfig = validForgeControlPlaneConfig()
			req.ControlPlaneConfig.DesiredCount = 1
			req.CniPlugin = tt.plugin
			pool := validForgeNodePoolCreateConfig("cp")
			pool.OpenVSwitch = &api.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: tt.bridge}
			req.NodePools = &[]api.NodePoolCreateConfig{pool}
			// No DB is provided: rejecting invalid intent must precede the transaction.
			service := &clusterService{allowLegacyCalicoClusterCreation: true}
			response, err := service.CreateCluster(context.Background(), api.CreateClusterRequestObject{Body: req})
			require.NoError(t, err)
			badRequest, ok := response.(api.CreateCluster400JSONResponse)
			require.True(t, ok, "expected 400, got %T", response)
			assert.Contains(t, badRequest.Message, tt.message)
		})
	}
}
