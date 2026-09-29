// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestClusterOVNKubernetesConfig(t *testing.T) {
	svc := server.NewClusterService(discardLogger(), nil, nil, config.TeleportConfig{}, false, nil, false)
	for _, tt := range []struct {
		name   string
		plugin api.CreateClusterRequestCniPlugin
		config *api.OVNKubernetesConfig
		err    string
	}{
		{name: "existing OVN request", plugin: "ovn-kubernetes"},
		{name: "existing Calico request", plugin: "calico"},
		{name: "empty settings", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{}},
		{name: "explicit false", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{AllowNoUplink: testutil.PtrTo(false)}},
		{
			name: "all settings", plugin: "ovn-kubernetes",
			config: &api.OVNKubernetesConfig{
				GatewayInterface: testutil.PtrTo("brfab0"),
				AllowNoUplink:    testutil.PtrTo(true),
				GatewayMode:      testutil.PtrTo(api.OVNKubernetesConfigGatewayModeLocal),
				MTU:              testutil.PtrTo(8900),
			},
		},
		{name: "shared mode", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{GatewayMode: testutil.PtrTo(api.OVNKubernetesConfigGatewayModeShared)}},
		{name: "minimum MTU", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{MTU: testutil.PtrTo(576)}},
		{name: "maximum MTU", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{MTU: testutil.PtrTo(65535)}},
		{name: "wrong plugin", plugin: "calico", config: &api.OVNKubernetesConfig{}, err: "requires the ovn-kubernetes CNI"},
		{name: "empty interface", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{GatewayInterface: testutil.PtrTo("")}, err: "gatewayInterface"},
		{name: "long interface", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{GatewayInterface: testutil.PtrTo(strings.Repeat("a", 16))}, err: "gatewayInterface"},
		{name: "loopback", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{GatewayInterface: testutil.PtrTo("lo")}, err: "gatewayInterface"},
		{name: "options in interface", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{GatewayInterface: testutil.PtrTo("br0 --foo")}, err: "gatewayInterface"},
		{name: "Helm assignments in interface", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{GatewayInterface: testutil.PtrTo("br0,mtu=9000")}, err: "gatewayInterface"},
		{name: "invalid mode", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{GatewayMode: testutil.PtrTo(api.OVNKubernetesConfigGatewayMode("disabled"))}, err: "gatewayMode"},
		{name: "small MTU", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{MTU: testutil.PtrTo(575)}, err: "mtu"},
		{name: "large MTU", plugin: "ovn-kubernetes", config: &api.OVNKubernetesConfig{MTU: testutil.PtrTo(65536)}, err: "mtu"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := baseNicoClusterCreateRequest()
			req.CniPlugin = tt.plugin
			req.OVNKubernetes = tt.config
			err := svc.ValidateCreateClusterRequestFields(req)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			stored, err := server.DBClusterFromAPI(req)
			require.NoError(t, err)
			response := server.APIClusterFromDB(&stored)
			require.Equal(t, tt.config, response.OVNKubernetes)
			raw, err := json.Marshal(response)
			require.NoError(t, err)
			if tt.config == nil {
				require.NotContains(t, string(raw), "ovnKubernetes")
			}
			var decoded api.Cluster
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.Equal(t, tt.config, decoded.OVNKubernetes)
		})
	}
}

func TestOpenAPIValidator_OVNKubernetesRejectsInvalidSettings(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config string
	}{
		{name: "unknown option", config: `{"gatewayOptions":"--arbitrary-option"}`},
		{name: "invalid gateway mode", config: `{"gatewayMode":"disabled"}`},
		{name: "invalid interface", config: `{"gatewayInterface":"br0,mtu=9000"}`},
		{name: "nonboolean flag", config: `{"allowNoUplink":"true"}`},
		{name: "fractional MTU", config: `{"mtu":1400.5}`},
		{name: "out of range MTU", config: `{"mtu":65536}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := openAPIForgeCreateClusterRequest()
			body.CniPlugin = "ovn-kubernetes"
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &fields))
			fields["ovnKubernetes"] = json.RawMessage(tt.config)
			raw, err = json.Marshal(fields)
			require.NoError(t, err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/clusters", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			newTestServerWithValidator(t, &testutil.MockDB{}).ServeHTTP(response, req)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "ovnKubernetes")
		})
	}
}

func TestOpenAPIValidator_OVNKubernetesIsImmutable(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPatch,
		"/api/clusters/550e8400-e29b-41d4-a716-446655440000",
		strings.NewReader(`{"ovnKubernetes":{"mtu":1400}}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newTestServerWithValidator(t, &testutil.MockDB{}).ServeHTTP(response, req)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "ovnKubernetes")
}
