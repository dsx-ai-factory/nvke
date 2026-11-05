// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

func TestReleaseTemplateVariables(t *testing.T) {
	tests := []struct {
		name  string
		init  internalv1.MachineInit
		image internalv1.MachineImage
		want  map[string]any
	}{
		{
			name: "files, commands and an iPXE script",
			init: internalv1.MachineInit{Infra: &internalv1.InfraBootstrap{
				BootstrapFiles:     []internalv1.BootstrapFile{{Path: "/etc/example", Permissions: "0644", Content: "line one\nline \"two\""}},
				PreKubeadmCommands: []string{"mkdir -p /run/example"},
			}},
			image: internalv1.MachineImage{Spec: internalv1.MachineImageSpec{NICo: internalv1.NICoBoot{IPXEScript: "#!ipxe\nboot \"disk\"\n"}}},
			want: map[string]any{
				"BootstrapFilesJSON":     `[{"path":"/etc/example","permissions":"0644","content":"line one\nline \"two\""}]`,
				"PreKubeadmCommandsJSON": `["mkdir -p /run/example"]`,
				"IPXEScript":             `"#!ipxe\nboot \"disk\"\n"`,
			},
		},
		{
			name:  "no infra bootstrap encodes empty lists",
			init:  internalv1.MachineInit{},
			image: internalv1.MachineImage{},
			want: map[string]any{
				"BootstrapFilesJSON":     `[]`,
				"PreKubeadmCommandsJSON": `[]`,
				"IPXEScript":             `""`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := releaseTemplateVariables(tt.init, &tt.image)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
