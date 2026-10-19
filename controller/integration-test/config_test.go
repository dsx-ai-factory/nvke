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
	ControllerAPIURL          string `env:"NVKE_CONTROLLER_API_URL"`
	CreateClusterRequestPath  string `env:"CREATE_CLUSTER_REQUEST_PATH" envDefault:"testdata/default/create-cluster-request.json"`
	CreateNodePoolRequestPath string `env:"CREATE_NODE_POOL_REQUEST_PATH" envDefault:"testdata/default/create-nodepool-request.json"`
	SkipCleanup               bool   `env:"SKIP_CLEANUP" envDefault:"false"`
	ControlPlaneVIP           string `env:"CONTROL_PLANE_VIP"`
}

const (
	// previousControllerAPIURLEnv and legacyControllerAPIURLEnv are deprecated aliases
	// for NVKE_CONTROLLER_API_URL; #1399 removes them.
	previousControllerAPIURLEnv = "NKE_CONTROLLER_API_URL"
	legacyControllerAPIURLEnv   = "CWE_API_URL"
	defaultControllerAPIURL     = "http://localhost:8080/api"
)

// loadIntegrationTestConfig parses the suite's settings from the environment.
// The env library takes one name per field, so the deprecated names and the
// default for ControllerAPIURL are applied here. An empty value counts as unset.
func loadIntegrationTestConfig() (IntegrationTestConfig, error) {
	config, err := env.ParseAs[IntegrationTestConfig]()
	if err != nil {
		return config, fmt.Errorf("parsing the integration test settings: %w", err)
	}
	config.ControllerAPIURL = cmp.Or(config.ControllerAPIURL, os.Getenv(previousControllerAPIURLEnv), os.Getenv(legacyControllerAPIURLEnv), defaultControllerAPIURL)
	return config, nil
}

func TestControllerAPIURL(t *testing.T) {
	const newName, newURL, previousURL, oldURL = "NVKE_CONTROLLER_API_URL", "http://new:8080/api", "http://previous:8080/api", "http://old:8080/api"
	tests := []struct {
		name string
		env  map[string]string // names left out are unset
		want string
	}{
		{name: "new name only", env: map[string]string{newName: newURL}, want: newURL},
		{name: "NKE_CONTROLLER name only", env: map[string]string{previousControllerAPIURLEnv: previousURL}, want: previousURL},
		{name: "CWE name only", env: map[string]string{legacyControllerAPIURLEnv: oldURL}, want: oldURL},
		{name: "no names", want: defaultControllerAPIURL},
		{name: "new name wins over deprecated names", env: map[string]string{newName: newURL, previousControllerAPIURLEnv: previousURL, legacyControllerAPIURLEnv: oldURL}, want: newURL},
		{name: "NKE_CONTROLLER name wins over CWE name", env: map[string]string{previousControllerAPIURLEnv: previousURL, legacyControllerAPIURLEnv: oldURL}, want: previousURL},
		{name: "empty new name does not hide deprecated names", env: map[string]string{newName: "", previousControllerAPIURLEnv: previousURL, legacyControllerAPIURLEnv: oldURL}, want: previousURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{newName, previousControllerAPIURLEnv, legacyControllerAPIURLEnv} {
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
