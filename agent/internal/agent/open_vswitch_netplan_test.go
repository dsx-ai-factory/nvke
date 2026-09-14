// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOpenVSwitchNetplanDocumentCreatesOptionalSections(t *testing.T) {
	document, err := parseOpenVSwitchNetplanDocument([]byte(`network:
  version: 2
  renderer: networkd
`))

	require.NoError(t, err)
	document.ethernets["eth0"] = map[string]any{"dhcp4": true}
	document.bridges["breth0"] = map[string]any{"interfaces": []string{"eth0"}}

	network := document.root["network"].(map[string]any)
	assert.Equal(t, "networkd", network["renderer"])
	assert.Equal(t, document.ethernets, network["ethernets"])
	assert.Equal(t, document.bridges, network["bridges"])
}

func TestParseOpenVSwitchNetplanDocumentRejectsMalformedSections(t *testing.T) {
	testCases := []struct {
		name        string
		config      string
		errorString string
	}{
		{
			name:        "missing network",
			config:      "renderer: networkd\n",
			errorString: "network section is required",
		},
		{
			name:        "network is not a mapping",
			config:      "network: disabled\n",
			errorString: "network section must be a mapping",
		},
		{
			name:        "ethernets is not a mapping",
			config:      "network:\n  ethernets: []\n",
			errorString: "network.ethernets section must be a mapping",
		},
		{
			name:        "bridges is not a mapping",
			config:      "network:\n  bridges: disabled\n",
			errorString: "network.bridges section must be a mapping",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseOpenVSwitchNetplanDocument([]byte(testCase.config))

			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.errorString)
		})
	}
}
