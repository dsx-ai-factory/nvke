// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadStackConfig(t *testing.T) {
	c, err := LoadStackConfig(filepath.Join("testdata", "stack-config.json"))

	require.NoError(t, err)
	assert.Equal(t, StackConfig{
		Mirror:         "mirror.example.internal:5000/nvke",
		ClientNetworks: []string{"10.0.0.0/24", "192.0.2.0/24"},
		Credentials: CredentialSources{
			DatabasePasswordFile: "/run/secrets/nvke/database-password",
			NicoTokenFile:        "/run/secrets/nvke/nico-token",
			NodeUser:             "nvke",
			NodePasswordFile:     "/run/secrets/nvke/node-password",
			SpireCACertFile:      "/run/secrets/nvke/spire-ca.crt",
			SpireCAKeyFile:       "/run/secrets/nvke/spire-ca.key",
		},
	}, c)
}

func TestLoadStackConfigRefuses(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"empty", " \n", "the file is empty"},
		{"not an object", "[]", "stack config:"},
		{"unknown key", `{"mirror": "m", "mirrorr": "m"}`, `unknown field "mirrorr"`},
		{"unknown credentials key", `{"credentials": {"nicoToken": "x"}}`, `unknown field "nicoToken"`},
		{"images", `{"images": {"controller": "x"}}`, "images come only from the artifact file"},
		{"trailing content", `{} {}`, "content after the JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadStackConfig(writeFile(t, "stack-config.json", tt.content))

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
