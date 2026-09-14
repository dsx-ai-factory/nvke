// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/NVIDIA/nke/controller/internal/utils"
)

func TestGetKubernetesVersionFromOSProfileOrDefault(t *testing.T) {
	index := &utils.MetadataIndex{
		Profiles: map[string]utils.Metadata{
			"ubuntu-24.04-amd64":  {KubernetesVersion: "v1.34.8"},
			"no-version-recorded": {},
		},
		DeprecatedProfiles: map[string]utils.Metadata{
			"ubuntu-22.04-amd64": {KubernetesVersion: "v1.31.0"},
		},
	}

	tests := []struct {
		name        string
		osProfile   string
		index       *utils.MetadataIndex
		want        string
		description string
	}{
		{
			name:        "version from the profile metadata",
			osProfile:   "ubuntu-24.04-amd64",
			index:       index,
			want:        "v1.34.8",
			description: "Should prefer the version the OS profile records",
		},
		{
			name:        "deprecated profiles are still consulted",
			osProfile:   "ubuntu-22.04-amd64",
			index:       index,
			want:        "v1.31.0",
			description: "A node pool on a deprecated profile still has a real version",
		},
		{
			name:        "unknown profile falls back to the cluster version",
			osProfile:   "not-in-the-index",
			index:       index,
			want:        "v1.33.0",
			description: "Should fall back rather than leave the column empty",
		},
		{
			name:        "profile with no recorded version falls back",
			osProfile:   "no-version-recorded",
			index:       index,
			want:        "v1.33.0",
			description: "An empty version in the metadata is not a version",
		},
		{
			// The migration skips loading the index when it has no rows to
			// backfill, which is what lets CWE start with no profiles tree.
			// Nothing should reach this function in that case, so this only
			// guards the nil dereference if something ever does.
			name:        "no index loaded",
			osProfile:   "ubuntu-24.04-amd64",
			index:       nil,
			want:        "v1.33.0",
			description: "Should fall back rather than panic when no index was loaded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getKubernetesVersionFromOSProfileOrDefault(tt.osProfile, tt.index, "v1.33.0")
			assert.Equal(t, tt.want, got, tt.description)
		})
	}
}
