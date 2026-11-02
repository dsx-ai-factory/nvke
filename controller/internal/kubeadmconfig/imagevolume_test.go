// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNeedsImageVolumeFeatureGate(t *testing.T) {
	tests := []struct {
		name              string
		kubernetesVersion string
		want              bool
	}{
		{name: "well below GA", kubernetesVersion: "v1.28.0", want: true},
		{name: "alpha release where the gate landed", kubernetesVersion: "v1.31.0", want: true},
		{name: "beta but still off by default", kubernetesVersion: "v1.33.0", want: true},
		{name: "last release before GA", kubernetesVersion: "v1.35.9", want: true},
		{name: "patch release below GA", kubernetesVersion: "v1.34.9", want: true},
		{name: "GA release needs no flag", kubernetesVersion: "v1.36.0", want: false},
		{name: "GA patch release needs no flag", kubernetesVersion: "v1.36.2", want: false},
		{name: "after GA the flag is removed", kubernetesVersion: "v1.38.0", want: false},
		{name: "version without the v prefix", kubernetesVersion: "1.34.9", want: true},
		// An unparseable or absent version must not add the flag: a missing gate
		// degrades one workload, an unknown gate stops kube-apiserver booting.
		{name: "empty version", kubernetesVersion: "", want: false},
		{name: "unparseable version", kubernetesVersion: "not-a-version", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, needsImageVolumeFeatureGate(tt.kubernetesVersion))
		})
	}
}

func TestImageVolumeKubeletFeatureGates(t *testing.T) {
	t.Run("set below the GA floor", func(t *testing.T) {
		assert.Equal(t, map[string]bool{"ImageVolume": true}, imageVolumeKubeletFeatureGates("v1.34.9"))
	})

	// nil rather than an empty map keeps featureGates out of the rendered YAML.
	t.Run("nil at and above the GA floor", func(t *testing.T) {
		assert.Nil(t, imageVolumeKubeletFeatureGates("v1.36.2"))
	})

	t.Run("nil when the version is unusable", func(t *testing.T) {
		assert.Nil(t, imageVolumeKubeletFeatureGates(""))
	})
}

func TestClusterConfigurationImageVolumeFeatureGate(t *testing.T) {
	tests := []struct {
		name              string
		kubernetesVersion string
		wantFlag          bool
	}{
		{name: "below GA sets the apiserver flag", kubernetesVersion: "v1.34.9", wantFlag: true},
		{name: "at GA leaves the apiserver flag off", kubernetesVersion: "v1.36.2", wantFlag: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validInitConfig()
			cfg.KubernetesVersion = tt.kubernetesVersion

			got := clusterConfiguration(cfg).APIServer.ExtraArgs[featureGatesFlag]

			if tt.wantFlag {
				assert.Equal(t, "ImageVolume=true", got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}

// The apiserver gate and the kubelet gate must move together: the apiserver
// admits the image volume, the kubelet mounts it, and one without the other
// still leaves the workload broken.
func TestImageVolumeGateRenderedOnBothComponents(t *testing.T) {
	cfg := validInitConfig()
	cfg.KubernetesVersion = "v1.34.9"

	artifacts, err := cfg.Render()
	require.NoError(t, err)

	rendered := string(artifacts.ConfigYAML)
	assert.Contains(t, rendered, "- name: feature-gates\n    value: ImageVolume=true", "kube-apiserver gate missing")
	assert.Contains(t, rendered, "featureGates:\n  ImageVolume: true", "kubelet gate missing")
}

func TestJoinKubeletConfigurationImageVolumeFeatureGate(t *testing.T) {
	tests := []struct {
		name              string
		kubernetesVersion string
		want              map[string]bool
	}{
		{name: "below GA", kubernetesVersion: "v1.34.9", want: map[string]bool{"ImageVolume": true}},
		{name: "at GA", kubernetesVersion: "v1.36.2", want: nil},
		// JoinConfig predates this field; an unset version must not change the
		// rendered kubelet configuration for existing callers.
		{name: "unset version", kubernetesVersion: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, joinKubeletConfiguration(tt.kubernetesVersion).FeatureGates)
		})
	}
}
