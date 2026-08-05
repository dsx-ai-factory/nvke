// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadOSProfileMetadata(t *testing.T) {
	// Create a temporary directory with test metadata
	tmpDir := t.TempDir()
	osProfilesDir := filepath.Join(tmpDir, "osProfiles")
	err := os.MkdirAll(osProfilesDir, 0o755)
	require.NoError(t, err)

	testMetadata := `profiles:
  nke-1.31.12-251119:
    kubernetes_version: "1.31.12"
    architecture: amd64
    build_date: "2025-11-19"
  nke-1.31.9-251119:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2025-11-19"
  nke-1.31.9-251124-24.04-arm64:
    kubernetes_version: "1.31.9"
    architecture: arm64
    build_date: "2025-11-24"
    ubuntu_version: "24.04"
`

	testDeprecated := `profiles:
  nke-deprecated-profile:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2025-11-19"
`

	metadataPath := filepath.Join(osProfilesDir, "metadata.yaml")
	err = os.WriteFile(metadataPath, []byte(testMetadata), 0o644)
	require.NoError(t, err)

	deprecatedPath := filepath.Join(osProfilesDir, "deprecated.yaml")
	err = os.WriteFile(deprecatedPath, []byte(testDeprecated), 0o644)
	require.NoError(t, err)

	// Test loading metadata
	idx, err := LoadOSProfileMetadata(tmpDir)
	require.NoError(t, err)
	require.NotNil(t, idx)

	assert.Len(t, idx.Profiles, 3)

	// Verify specific profile metadata
	meta, ok := idx.GetOSProfileMetadata("nke-1.31.12-251119", false)
	require.True(t, ok)
	assert.Equal(t, "1.31.12", meta.KubernetesVersion)
	assert.Equal(t, "amd64", meta.Architecture)
	assert.Equal(t, "2025-11-19", meta.BuildDate)

	// Verify deprecated-only profile is NOT found without includeDeprecated
	_, ok = idx.GetOSProfileMetadata("nke-deprecated-profile", false)
	require.False(t, ok)

	// Verify deprecated profile metadata
	_, ok = idx.GetOSProfileMetadata("nke-deprecated-profile", true)
	require.True(t, ok)
}

func TestFindOSProfiles(t *testing.T) {
	idx := &MetadataIndex{
		Profiles: map[string]Metadata{
			"nke-1.31.9-251029": {
				KubernetesVersion: "1.31.9",
				Architecture:      "amd64",
				Variant:           "",
				UbuntuVersion:     "22.04",
			},
			"nke-1.31.9-251029-ovn": {
				KubernetesVersion: "1.31.9",
				Architecture:      "amd64",
				Variant:           "ovn",
				UbuntuVersion:     "22.04",
			},
			"nke-1.31.9-251029-osmo": {
				KubernetesVersion: "1.31.9",
				Architecture:      "amd64",
				Variant:           "osmo",
				UbuntuVersion:     "22.04",
			},
			"nke-1.31.9-251029-24.04": {
				KubernetesVersion: "1.31.9",
				Architecture:      "amd64",
				Variant:           "",
				UbuntuVersion:     "24.04",
			},
			"nke-1.31.12-251119-ovn": {
				KubernetesVersion: "1.31.12",
				Architecture:      "amd64",
				Variant:           "ovn",
				UbuntuVersion:     "22.04",
			},
		},
	}

	// Test exact matching with explicit values
	profiles := idx.FindOSProfiles("1.31.9", "amd64", "ovn", "22.04")
	assert.Len(t, profiles, 1)
	assert.Contains(t, profiles, "nke-1.31.9-251029-ovn")
	assert.NotContains(t, profiles, "nke-1.31.12-251119-ovn") // Different version

	// Test with no variant (explicitly passing ubuntu version)
	noVariantProfiles := idx.FindOSProfiles("1.31.9", "amd64", "", "22.04")
	assert.Len(t, noVariantProfiles, 1)
	assert.Contains(t, noVariantProfiles, "nke-1.31.9-251029")
	assert.NotContains(t, noVariantProfiles, "nke-1.31.9-251029-ovn")
	assert.NotContains(t, noVariantProfiles, "nke-1.31.9-251029-osmo")
	assert.NotContains(t, noVariantProfiles, "nke-1.31.9-251029-24.04") // Different ubuntuVersion

	// Test explicit 24.04 ubuntuVersion
	ubuntu2404Profiles := idx.FindOSProfiles("1.31.9", "amd64", "", "24.04")
	assert.Len(t, ubuntu2404Profiles, 1)
	assert.Contains(t, ubuntu2404Profiles, "nke-1.31.9-251029-24.04")
	assert.NotContains(t, ubuntu2404Profiles, "nke-1.31.9-251029") // Different ubuntuVersion

	// Empty architecture and ubuntuVersion do not default; exact match only, so no profiles match
	emptyCriteria := idx.FindOSProfiles("1.31.9", "", "", "")
	assert.Empty(t, emptyCriteria, "empty architecture/ubuntuVersion should match no profiles (no default)")
}
