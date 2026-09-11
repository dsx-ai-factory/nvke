// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestUpdateNodePoolKubeVersion_EmptyOSProfile(t *testing.T) {
	t.Parallel()

	nodePoolID := uuid.New()
	storedPool := db.NodePool{
		ID:                nodePoolID,
		KubernetesVersion: "1.34.2",
		Nico: &db.NicoNodePoolProvider{
			OSProfile: "",
		},
	}
	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodePool", mock.Anything, nodePoolID).Return(storedPool, nil)
	mockTx.On("UpdateNodePool", mock.Anything, mock.MatchedBy(func(pool *db.NodePool) bool {
		nico, err := pool.NicoProvider()
		return err == nil &&
			pool.ID == nodePoolID &&
			pool.KubernetesVersion == "1.35.0" &&
			nico.OSProfile == ""
	})).Return(db.NodePool{}, nil)

	r := &reconciler{db: mockTx, logger: slog.Default()}
	err := r.updateNodePoolKubeVersion(
		context.Background(),
		db.Cluster{KubernetesVersion: "1.35.0", ProviderType: db.ProviderTypeCAPI},
		nodePool{NodePool: storedPool},
	)

	require.NoError(t, err)
	mockTx.AssertExpectations(t)
}

func TestResolveOSProfile(t *testing.T) {
	// Create a temporary directory for test profiles
	tmpDir := t.TempDir()
	profilesDir := filepath.Join(tmpDir, "profiles")
	osProfilesDir := filepath.Join(profilesDir, "osProfiles")
	require.NoError(t, os.MkdirAll(osProfilesDir, 0o755))

	// Create test metadata.yaml
	metadataContent := `profiles:
  # AMD64 profiles
  nke-1.31.9-260101:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-01"
    ubuntu_version: "22.04"
    
  nke-1.31.9-260115:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-15"
    ubuntu_version: "22.04"
    
  nke-1.31.9-260120:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-20"
    ubuntu_version: "22.04"
    
  # ARM64 profiles
  nke-1.31.9-260115-arm64:
    kubernetes_version: "1.31.9"
    architecture: arm64
    build_date: "2026-01-15"
    ubuntu_version: "22.04"
    variant: "arm64"
    
  # OVN variant
  nke-1.31.9-260115-ovn:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-15"
    ubuntu_version: "22.04"
    variant: "ovn"
    
  nke-1.31.9-260120-ovn:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-20"
    ubuntu_version: "22.04"
    variant: "ovn"
    
  # OSMO variant
  nke-1.31.9-260110-osmo:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-10"
    ubuntu_version: "22.04"
    variant: "osmo"
    
  # Different K8s version
  nke-1.30.5-260115:
    kubernetes_version: "1.30.5"
    architecture: amd64
    build_date: "2026-01-15"
    ubuntu_version: "22.04"
`
	metadataPath := filepath.Join(osProfilesDir, "metadata.yaml")
	require.NoError(t, os.WriteFile(metadataPath, []byte(metadataContent), 0o644))

	tests := []struct {
		name               string
		k8sVersion         string
		currentProfileName string
		expectedProfile    string
		expectError        bool
		errorContains      string
	}{
		{
			name:               "no current profile - no default to amd64/22.04, returns error (no matching profiles)",
			k8sVersion:         "1.31.9",
			currentProfileName: "",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.31.9",
		},
		{
			name:               "preserve amd64 architecture",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.30.5-260115", // amd64 profile
			expectedProfile:    "nke-1.31.9-260120", // Newest amd64 for target version
			expectError:        false,
		},
		{
			name:               "preserve arm64 architecture",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260115-arm64",
			expectedProfile:    "nke-1.31.9-260115-arm64", // Only arm64 profile available
			expectError:        false,
		},
		{
			name:               "preserve ovn variant",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260115-ovn",
			expectedProfile:    "nke-1.31.9-260120-ovn", // Newest ovn variant
			expectError:        false,
		},
		{
			name:               "preserve osmo variant",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260110-osmo",
			expectedProfile:    "nke-1.31.9-260110-osmo", // Only osmo profile
			expectError:        false,
		},
		{
			name:               "current profile not in metadata - returns error (no guessing)",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-unknown-profile",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "not found in metadata",
		},
		{
			name:               "no matching profile found",
			k8sVersion:         "1.32.0", // Version doesn't exist
			currentProfileName: "",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.32.0",
		},
		{
			name:               "no matching arm64 profile for version",
			k8sVersion:         "1.30.5", // No arm64 profiles for this version
			currentProfileName: "nke-1.31.9-260115-arm64",
			expectedProfile:    "",
			expectError:        true,
			errorContains:      "no OSProfile found for Kubernetes version 1.30.5",
		},
		{
			name:               "pick newest by build_date when multiple matches",
			k8sVersion:         "1.31.9",
			currentProfileName: "nke-1.31.9-260101", // amd64, no variant
			expectedProfile:    "nke-1.31.9-260120", // Newest: 260120 > 260115 > 260101
			expectError:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &reconciler{
				cfg: config.Config{
					ProfilesDir: profilesDir,
				},
				logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
			}

			result, err := r.resolveOSProfile(tt.k8sVersion, tt.currentProfileName)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedProfile, result)
			}
		})
	}
}

func TestResolveOSProfile_MetadataLoadError(t *testing.T) {
	// Test with non-existent profiles directory
	r := &reconciler{
		cfg: config.Config{
			ProfilesDir: "/nonexistent/path",
		},
		logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	_, err := r.resolveOSProfile("1.31.9", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load OSProfile metadata")
}

func TestResolveOSProfile_BuildDateOrdering(t *testing.T) {
	// Test that build_date ordering works correctly (string comparison: "2026-01-20" > "2026-01-15")
	tmpDir := t.TempDir()
	profilesDir := filepath.Join(tmpDir, "profiles")
	osProfilesDir := filepath.Join(profilesDir, "osProfiles")
	require.NoError(t, os.MkdirAll(osProfilesDir, 0o755))

	metadataContent := `profiles:
  nke-1.30.5-260101:
    kubernetes_version: "1.30.5"
    architecture: amd64
    build_date: "2026-01-01"
    ubuntu_version: "22.04"
  nke-1.31.9-260105:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-05"
    ubuntu_version: "22.04"
  nke-1.31.9-260120:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-20"
    ubuntu_version: "22.04"
  nke-1.31.9-260115:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-15"
    ubuntu_version: "22.04"
  nke-1.31.9-260110:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-10"
    ubuntu_version: "22.04"
  nke-1.31.9-260125:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-25"
    ubuntu_version: "22.04"
`
	metadataPath := filepath.Join(osProfilesDir, "metadata.yaml")
	require.NoError(t, os.WriteFile(metadataPath, []byte(metadataContent), 0o644))

	r := &reconciler{
		cfg: config.Config{
			ProfilesDir: profilesDir,
		},
		logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	result, err := r.resolveOSProfile("1.31.9", "nke-1.30.5-260101")
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260125", result, "Should pick profile with newest build_date (2026-01-25)")
}

func TestResolveOSProfile_VariantPreservation(t *testing.T) {
	// Test that variant is correctly preserved when upgrading
	tmpDir := t.TempDir()
	profilesDir := filepath.Join(tmpDir, "profiles")
	osProfilesDir := filepath.Join(profilesDir, "osProfiles")
	require.NoError(t, os.MkdirAll(osProfilesDir, 0o755))

	metadataContent := `profiles:
  # Old version with ovn variant
  nke-1.30.5-260101-ovn:
    kubernetes_version: "1.30.5"
    architecture: amd64
    build_date: "2026-01-01"
    ubuntu_version: "22.04"
    variant: "ovn"
    
  # New version - multiple profiles
  nke-1.31.9-260115:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-15"
    ubuntu_version: "22.04"
    
  nke-1.31.9-260115-ovn:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-15"
    ubuntu_version: "22.04"
    variant: "ovn"
    
  nke-1.31.9-260120-ovn:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-20"
    ubuntu_version: "22.04"
    variant: "ovn"
`
	metadataPath := filepath.Join(osProfilesDir, "metadata.yaml")
	require.NoError(t, os.WriteFile(metadataPath, []byte(metadataContent), 0o644))

	r := &reconciler{
		cfg: config.Config{
			ProfilesDir: profilesDir,
		},
		logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	// Upgrading from ovn variant should select ovn variant in new version
	result, err := r.resolveOSProfile("1.31.9", "nke-1.30.5-260101-ovn")
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260120-ovn", result, "Should preserve 'ovn' variant and pick newest")
}

func TestResolveOSProfile_LoggingBehavior(t *testing.T) {
	// Verify that logging provides useful information
	tmpDir := t.TempDir()
	profilesDir := filepath.Join(tmpDir, "profiles")
	osProfilesDir := filepath.Join(profilesDir, "osProfiles")
	require.NoError(t, os.MkdirAll(osProfilesDir, 0o755))

	metadataContent := `profiles:
  nke-1.30.5-260101-ovn:
    kubernetes_version: "1.30.5"
    architecture: amd64
    build_date: "2026-01-01"
    ubuntu_version: "22.04"
    variant: "ovn"
  nke-1.31.9-260115-ovn:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2026-01-15"
    ubuntu_version: "22.04"
    variant: "ovn"
`
	metadataPath := filepath.Join(osProfilesDir, "metadata.yaml")
	require.NoError(t, os.WriteFile(metadataPath, []byte(metadataContent), 0o644))

	// Use Info level to capture log output
	r := &reconciler{
		cfg: config.Config{
			ProfilesDir: profilesDir,
		},
		logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	result, err := r.resolveOSProfile("1.31.9", "nke-1.30.5-260101-ovn")
	require.NoError(t, err)
	assert.Equal(t, "nke-1.31.9-260115-ovn", result)
	// Note: Log output verification would require capturing logs, which we're not doing here
	// but the test ensures the code path with logging executes without errors
}
