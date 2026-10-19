// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigProfileLabels(t *testing.T) {
	tests := []struct {
		name string
		// files maps a path under configProfiles/ to its contents. nil means no configProfiles directory.
		files map[string]string
		// unsetProfilesDir passes an empty profilesDir instead of the test's directory.
		unsetProfilesDir bool
		profiles         []string
		want             map[string]map[string]string
		wantErr          string
	}{
		{
			name: "active profile labels, without the control-plane role label",
			files: map[string]string{
				"cp.yaml": "labels:\n  - node-role.kubernetes.io/control-plane=\n  - nodeGroup=control-plane\n  - node.kubernetes.io/instance-type=cpu\n",
			},
			profiles: []string{"cp"},
			want:     map[string]map[string]string{"cp": {"nodeGroup": "control-plane", "node.kubernetes.io/instance-type": "cpu"}},
		},
		{
			name:     "empty label value is kept",
			files:    map[string]string{"gpu.yaml": "labels:\n  - node-role.kubernetes.io/gpu-worker=\n"},
			profiles: []string{"gpu"},
			want:     map[string]map[string]string{"gpu": {"node-role.kubernetes.io/gpu-worker": ""}},
		},
		{
			name:     "deprecated profile is read when no active profile exists",
			files:    map[string]string{"deprecated/old.yaml": "labels:\n  - nodeGroup=old\n"},
			profiles: []string{"old"},
			want:     map[string]map[string]string{"old": {"nodeGroup": "old"}},
		},
		{
			name: "active profile wins over deprecated profile",
			files: map[string]string{
				"both.yaml":            "labels:\n  - nodeGroup=active\n",
				"deprecated/both.yaml": "labels:\n  - nodeGroup=deprecated\n",
			},
			profiles: []string{"both"},
			want:     map[string]map[string]string{"both": {"nodeGroup": "active"}},
		},
		{
			name:     "profile without labels",
			files:    map[string]string{"hooks-only.yaml": "preJoinNodeReboot: true\n"},
			profiles: []string{"hooks-only"},
			want:     map[string]map[string]string{"hooks-only": {}},
		},
		{
			name:     "retired profile has no labels",
			files:    map[string]string{"other.yaml": "labels:\n  - nodeGroup=other\n"},
			profiles: []string{"retired"},
			want:     map[string]map[string]string{"retired": {}},
		},
		{
			name:     "no referenced profiles does not need the directory",
			files:    nil,
			profiles: nil,
			want:     map[string]map[string]string{},
		},
		{
			name:     "missing directory fails while profiles are referenced",
			files:    nil,
			profiles: []string{"cp"},
			wantErr:  "config profiles [cp] are referenced but",
		},
		{
			name:             "unset profilesDir fails while profiles are referenced",
			unsetProfilesDir: true,
			profiles:         []string{"cp", "gpu"},
			wantErr:          "config profiles [cp gpu] are referenced but profilesDir is not set",
		},
		{
			name:     "label removal entry fails",
			files:    map[string]string{"remove.yaml": "labels:\n  - nodeGroup-\n"},
			profiles: []string{"remove"},
			wantErr:  `config profile "remove": label "nodeGroup-" is not key=value`,
		},
		{
			name:     "invalid label value fails",
			files:    map[string]string{"bad.yaml": "labels:\n  - nodeGroup=not a valid value\n"},
			profiles: []string{"bad"},
			wantErr:  `config profile "bad": invalid value for label "nodeGroup"`,
		},
		{
			name:     "malformed YAML fails",
			files:    map[string]string{"broken.yaml": "labels: [unterminated\n"},
			profiles: []string{"broken"},
			wantErr:  `parsing config profile "broken"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profilesDir := t.TempDir()
			if tt.files != nil {
				require.NoError(t, os.MkdirAll(filepath.Join(profilesDir, "configProfiles", "deprecated"), 0o755))
				for name, contents := range tt.files {
					require.NoError(t, os.WriteFile(filepath.Join(profilesDir, "configProfiles", name), []byte(contents), 0o600))
				}
			}

			if tt.unsetProfilesDir {
				profilesDir = ""
			}

			got, err := loadConfigProfileLabels(profilesDir, tt.profiles)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
