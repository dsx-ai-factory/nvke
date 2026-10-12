// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
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

func TestOpenVSwitchConfigCreateAndRead(t *testing.T) {
	want := &api.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
	pool, err := dbNodePoolFromAPI(&api.CreateNodePoolRequest{ClusterID: uuid.New(), OpenVSwitch: want}, "1.36.0")
	require.NoError(t, err)
	require.Equal(t, &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}, pool.OpenVSwitch)
	assert.Equal(t, want, apiNodePoolFromDB(&pool).OpenVSwitch)

	spec := controlPlaneConfigSpecFromCreateAPI(&api.CreateClusterControlPlaneConfig{OpenVSwitch: want, Infra: &api.InfraControlPlaneProvider{MachineTemplate: map[string]any{"kind": "ExampleMachineTemplate"}}}, "1.36.0", false, db.ProviderTypeCAPI)
	assert.Equal(t, want, apiControlPlaneConfigFromDB(db.ControlPlaneConfig{ControlPlaneConfigSpec: spec}).OpenVSwitch)
}

func TestOpenVSwitchConfigUpdate(t *testing.T) {
	explicit := &db.OpenVSwitchConfig{UplinkInterface: "eth1", BridgeName: "br-eth1"}
	uplink, bridge := "eth2", "br-eth2"
	replacement := &api.OpenVSwitchUpdateConfig{UplinkInterface: &uplink, BridgeName: &bridge}
	for _, tt := range []struct {
		name    string
		initial *db.OpenVSwitchConfig
		update  *api.OpenVSwitchUpdateConfig
		want    *db.OpenVSwitchConfig
	}{
		{name: "omission preserves explicit selection", initial: explicit, want: explicit},
		{name: "empty object restores automatic selection", initial: explicit, update: &api.OpenVSwitchUpdateConfig{}},
		{name: "pair replaces selection", initial: explicit, update: replacement, want: &db.OpenVSwitchConfig{UplinkInterface: uplink, BridgeName: bridge}},
		{name: "clearing automatic selection is unchanged", update: &api.OpenVSwitchUpdateConfig{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cluster := db.Cluster{ProviderType: db.ProviderTypeCAPI, CniPlugin: db.CNIOvnKubernetes}
			old, pinned := tt.initial.Clone(), tt.initial.Clone()
			pool := db.NodePool{OpenVSwitch: old}
			poolPatch := &api.UpdateNodePoolRequest{OpenVSwitch: tt.update}
			require.NoError(t, (&nodePoolService{}).validateUpdateNodePoolRequest(cluster, poolPatch))
			require.NoError(t, applyUpdateNodePoolRequest(cluster, &pool, poolPatch))
			assert.Equal(t, tt.want, pool.OpenVSwitch)
			assert.Equal(t, pinned, old, "the previous configuration must not be mutated")
			if tt.update == nil {
				assert.Same(t, old, pool.OpenVSwitch)
			}
			poolResponse := apiNodePoolFromDB(&pool)
			assert.Equal(t, apiOpenVSwitchFromDB(tt.want), poolResponse.OpenVSwitch)

			spec := db.ControlPlaneConfigSpec{OpenVSwitch: old}
			patch := &api.UpdateClusterControlPlaneConfig{OpenVSwitch: tt.update}
			service := &clusterService{}
			require.NoError(t, service.applyClusterLevelControlPlaneSpecUpdate(context.Background(), &cluster, patch, &spec))
			assert.Equal(t, tt.want, spec.OpenVSwitch)
			assert.Equal(t, pinned, old, "the pinned configuration must not be mutated")
			assert.Equal(t, tt.update != nil, hasClusterLevelControlPlaneUpdate(patch))
			if tt.update == nil {
				assert.Same(t, old, spec.OpenVSwitch)
			}
			cpResponse := apiControlPlaneConfigFromDB(db.ControlPlaneConfig{ControlPlaneConfigSpec: spec})
			assert.Equal(t, apiOpenVSwitchFromDB(tt.want), cpResponse.OpenVSwitch)
			if tt.want == nil {
				for _, response := range []any{poolResponse, cpResponse} {
					data, err := json.Marshal(response)
					require.NoError(t, err)
					assert.NotContains(t, string(data), `"openVSwitch"`)
				}
				poolBefore, specBefore := pool, spec
				require.NoError(t, applyUpdateNodePoolRequest(cluster, &pool, &api.UpdateNodePoolRequest{OpenVSwitch: &api.OpenVSwitchUpdateConfig{}}))
				require.NoError(t, service.applyClusterLevelControlPlaneSpecUpdate(context.Background(), &cluster, patch, &spec))
				assert.Equal(t, poolBefore, pool, "repeating a clear must be a no-op")
				assert.Equal(t, specBefore, spec, "repeating a clear must be a no-op")
			}
		})
	}
}

func TestOpenVSwitchUpdateNormalization(t *testing.T) {
	uplink, bridge, empty := "eth1", "br-eth1", ""
	for _, tt := range []struct {
		name    string
		update  *api.OpenVSwitchUpdateConfig
		wantNil bool
		wantErr bool
	}{
		{name: "omitted", wantNil: true},
		{name: "empty object", update: &api.OpenVSwitchUpdateConfig{}, wantNil: true},
		{name: "pair", update: &api.OpenVSwitchUpdateConfig{UplinkInterface: &uplink, BridgeName: &bridge}},
		{name: "uplink only", update: &api.OpenVSwitchUpdateConfig{UplinkInterface: &uplink}, wantErr: true},
		{name: "bridge only", update: &api.OpenVSwitchUpdateConfig{BridgeName: &bridge}, wantErr: true},
		{name: "empty uplink only", update: &api.OpenVSwitchUpdateConfig{UplinkInterface: &empty}, wantErr: true},
		{name: "empty names", update: &api.OpenVSwitchUpdateConfig{UplinkInterface: &empty, BridgeName: &empty}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := dbOpenVSwitchFromUpdateAPI(tt.update)
			assert.Equal(t, tt.wantNil, cfg == nil)
			err := validateOpenVSwitchConfig(db.CNIOvnKubernetes, cfg)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if cfg == nil {
				require.NoError(t, validateOpenVSwitchConfig(db.CNICalico, cfg))
			}
		})
	}
}

func TestOpenVSwitchUpdateRequestJSON(t *testing.T) {
	for _, tt := range []struct {
		name string
		body any
		want string
	}{
		{name: "pool reset", body: api.UpdateNodePoolRequest{OpenVSwitch: &api.OpenVSwitchUpdateConfig{}}, want: `{"openVSwitch":{}}`},
		{name: "control-plane reset", body: api.UpdateClusterRequest{ControlPlaneConfig: &api.UpdateClusterControlPlaneConfig{OpenVSwitch: &api.OpenVSwitchUpdateConfig{}}}, want: `{"controlPlaneConfig":{"openVSwitch":{}}}`},
		{name: "pool omission", body: api.UpdateNodePoolRequest{}, want: `{}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.body)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(data))
		})
	}
}

func TestCreateClusterRejectsInitialPoolOpenVSwitchBeforePersistence(t *testing.T) {
	for _, tt := range []struct {
		name    string
		plugin  api.CreateClusterRequestCniPlugin
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
