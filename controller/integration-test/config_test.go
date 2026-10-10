// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The suite's settings, and the test of how they resolve. Unlike
// integration_test.go this file carries no integration_test build tag, so the
// unit test lane runs TestControllerAPIURL; the live suite stays behind the tag.

package main

import (
	"cmp"
	"fmt"
	"os"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/require"
)

type IntegrationTestConfig struct {
	ControllerAPIURL          string `env:"NKE_CONTROLLER_API_URL"`
	CreateClusterRequestPath  string `env:"CREATE_CLUSTER_REQUEST_PATH" envDefault:"testdata/default/create-cluster-request.json"`
	CreateNodePoolRequestPath string `env:"CREATE_NODE_POOL_REQUEST_PATH" envDefault:"testdata/default/create-nodepool-request.json"`
	SkipCleanup               bool   `env:"SKIP_CLEANUP" envDefault:"false"`
	ControlPlaneVIP           string `env:"CONTROL_PLANE_VIP"`
}

const (
	// legacyControllerAPIURLEnv is a deprecated alias for NKE_CONTROLLER_API_URL;
	// nke#1399 removes it.
	legacyControllerAPIURLEnv = "CWE_API_URL"
	defaultControllerAPIURL   = "http://localhost:8080/api"
)

// loadIntegrationTestConfig parses the suite's settings from the environment.
// The env library takes one name per field, so the deprecated name and the
// default for ControllerAPIURL are applied here. An empty value counts as unset.
func loadIntegrationTestConfig() (IntegrationTestConfig, error) {
	config, err := env.ParseAs[IntegrationTestConfig]()
	if err != nil {
		return config, fmt.Errorf("parsing the integration test settings: %w", err)
	}
	config.ControllerAPIURL = cmp.Or(config.ControllerAPIURL, os.Getenv(legacyControllerAPIURLEnv), defaultControllerAPIURL)
	return config, nil
}

func TestControllerAPIURL(t *testing.T) {
	const newName, newURL, oldURL = "NKE_CONTROLLER_API_URL", "http://new:8080/api", "http://old:8080/api"
	tests := []struct {
		name string
		env  map[string]string // names left out are unset
		want string
	}{
		{name: "new name only", env: map[string]string{newName: newURL}, want: newURL},
		{name: "deprecated name only", env: map[string]string{legacyControllerAPIURLEnv: oldURL}, want: oldURL},
		{name: "neither name", want: defaultControllerAPIURL},
		{name: "new name wins over deprecated name", env: map[string]string{newName: newURL, legacyControllerAPIURLEnv: oldURL}, want: newURL},
		{name: "empty new name does not hide deprecated name", env: map[string]string{newName: "", legacyControllerAPIURLEnv: oldURL}, want: oldURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{newName, legacyControllerAPIURLEnv} {
				t.Setenv(name, "") // restores the caller's value afterwards
				require.NoError(t, os.Unsetenv(name))
			}
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			config, err := loadIntegrationTestConfig()
			require.NoError(t, err)
			require.Equal(t, tt.want, config.ControllerAPIURL)
		})
	}
}
