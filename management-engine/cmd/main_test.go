// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigureNVKEController(t *testing.T) {
	dir := t.TempDir()
	bindingsPath := filepath.Join(dir, "bindings.json")
	invalidBindingsPath := filepath.Join(dir, "invalid-bindings.json")
	missingTokenPath := filepath.Join(dir, "missing-token")
	data := `[{"namespace":"project-a","channel":"stable","controlPlaneClass":"standard","template":{
		"controlPlaneKubernetesVersion":"1.35.1",
		"infra":{"cluster":{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta2","kind":"DockerCluster"}},
		"controlPlaneConfig":{"desiredCount":3,"infra":{"machineTemplate":{
			"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta2","kind":"DockerMachineTemplate"
		}}}
	}}]`
	require.NoError(t, os.WriteFile(bindingsPath, []byte(data), 0o600))
	require.NoError(t, os.WriteFile(invalidBindingsPath, []byte(`[{"unknown":"REDACTED"}]`), 0o600))
	const endpoint = "https://controller.example.com"
	const partialConfiguration = "--nvke-controller-endpoint and --nvke-controller-bindings must be provided together"
	for _, tc := range []struct {
		name         string
		endpoint     string
		tokenFile    string
		bindingsPath string
		wantError    string
	}{
		{"disabled", "", "", "", ""},
		{"endpoint without bindings", endpoint, "", "", partialConfiguration},
		{"bindings without endpoint", "", "", bindingsPath, partialConfiguration},
		{"token without configuration", "", missingTokenPath, "", partialConfiguration},
		{"configured", endpoint, "", bindingsPath, ""},
		{"invalid endpoint", "https://user:REDACTED@controller.example.com", "", bindingsPath, "NVKE controller endpoint"},
		{"missing token", endpoint, missingTokenPath, bindingsPath, "bearer token file"},
		{"invalid bindings", endpoint, "", invalidBindingsPath, "NVKE controller bindings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, bindings, err := configureNVKEController(tc.endpoint, tc.tokenFile, tc.bindingsPath)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.NotContains(t, err.Error(), "REDACTED")
				return
			}
			require.NoError(t, err)
			if tc.endpoint == "" {
				require.Nil(t, service)
				require.Nil(t, bindings)
				return
			}
			require.Equal(t, endpoint+"/api", service.Endpoint())
			require.Len(t, bindings, 1)
		})
	}
}
