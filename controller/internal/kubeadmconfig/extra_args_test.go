// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	kubeadmv1beta3 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
	kubeadmv1beta4 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta4"
)

const (
	imageVolumeVersion   = "v1.35.0"
	noImageVolumeVersion = "v1.36.0"
)

func TestValidateExtraArgs(t *testing.T) {
	tests := []struct {
		name    string
		version string
		args    ExtraArgs
		wantErr string
	}{
		{name: "no arguments", version: imageVolumeVersion},
		{
			name:    "flags NVKE doesn't set on each component",
			version: imageVolumeVersion,
			args: ExtraArgs{
				APIServer:         map[string]string{"runtime-config": "resource.k8s.io/v1beta2=true"},
				ControllerManager: map[string]string{"concurrent-deployment-syncs": "10"},
				Scheduler:         map[string]string{"v": "3"},
			},
		},
		{
			name:    "a controller-manager flag is not reserved on the scheduler",
			version: imageVolumeVersion,
			args:    ExtraArgs{Scheduler: map[string]string{"secondary-node-eviction-rate": "1"}},
		},
		{
			name:    "kube-apiserver flag NVKE sets",
			version: imageVolumeVersion,
			args:    ExtraArgs{APIServer: map[string]string{"tls-cipher-suites": "TLS_RSA_WITH_AES_128_CBC_SHA"}},
			wantErr: `apiServer.extraArgs: flag "tls-cipher-suites" is set by NVKE`,
		},
		{
			name:    "kube-controller-manager flag NVKE sets",
			version: imageVolumeVersion,
			args:    ExtraArgs{ControllerManager: map[string]string{"secondary-node-eviction-rate": "1"}},
			wantErr: `controllerManager.extraArgs: flag "secondary-node-eviction-rate" is set by NVKE`,
		},
		{
			name:    "kube-scheduler flag NVKE sets",
			version: imageVolumeVersion,
			args:    ExtraArgs{Scheduler: map[string]string{"profiling": "true"}},
			wantErr: `scheduler.extraArgs: flag "profiling" is set by NVKE`,
		},
		{
			name:    "underscore spelling of a flag NVKE sets",
			version: imageVolumeVersion,
			args:    ExtraArgs{APIServer: map[string]string{"enable_admission_plugins": "AlwaysAdmit"}},
			wantErr: `flag name "enable_admission_plugins" must be lowercase words joined by hyphens`,
		},
		{
			name:    "leading dashes",
			version: imageVolumeVersion,
			args:    ExtraArgs{APIServer: map[string]string{"--profiling": "true"}},
			wantErr: `flag name "--profiling"`,
		},
		{
			name:    "uppercase flag name",
			version: imageVolumeVersion,
			args:    ExtraArgs{APIServer: map[string]string{"Profiling": "true"}},
			wantErr: `flag name "Profiling"`,
		},
		{
			name:    "empty flag name",
			version: imageVolumeVersion,
			args:    ExtraArgs{APIServer: map[string]string{"": "x"}},
			wantErr: `flag name ""`,
		},
		{
			name:    "flag name containing =",
			version: imageVolumeVersion,
			args:    ExtraArgs{APIServer: map[string]string{"profiling=true": "x"}},
			wantErr: `flag name "profiling=true"`,
		},
		{
			name:    "empty feature-gates",
			version: imageVolumeVersion,
			args:    ExtraArgs{Scheduler: map[string]string{featureGatesFlag: " "}},
			wantErr: "scheduler.extraArgs: feature-gates is empty",
		},
		{
			name:    "feature gate without a value",
			version: imageVolumeVersion,
			args:    ExtraArgs{Scheduler: map[string]string{featureGatesFlag: "SchedulerAsyncAPICalls"}},
			wantErr: `feature-gates entry "SchedulerAsyncAPICalls" is not Name=true or Name=false`,
		},
		{
			name:    "feature gate value other than true or false",
			version: imageVolumeVersion,
			args:    ExtraArgs{Scheduler: map[string]string{featureGatesFlag: "SchedulerAsyncAPICalls=1"}},
			wantErr: `feature-gates entry "SchedulerAsyncAPICalls=1"`,
		},
		{
			name:    "empty feature gate entry",
			version: imageVolumeVersion,
			args:    ExtraArgs{Scheduler: map[string]string{featureGatesFlag: "SchedulerAsyncAPICalls=true,"}},
			wantErr: `feature-gates entry ""`,
		},
		{
			name:    "feature gate set twice",
			version: imageVolumeVersion,
			args:    ExtraArgs{Scheduler: map[string]string{featureGatesFlag: "SchedulerAsyncAPICalls=true,SchedulerAsyncAPICalls=false"}},
			wantErr: `feature gate "SchedulerAsyncAPICalls" is set more than once`,
		},
		{
			name:    "gate contradicting NVKE's ImageVolume",
			version: imageVolumeVersion,
			args:    ExtraArgs{APIServer: map[string]string{featureGatesFlag: "ImageVolume=false"}},
			wantErr: `apiServer.extraArgs: feature gate "ImageVolume" contradicts NVKE, which sets it to true on Kubernetes v1.35.0`,
		},
		{
			name:    "ImageVolume=false once NVKE no longer sets it",
			version: noImageVolumeVersion,
			args:    ExtraArgs{APIServer: map[string]string{featureGatesFlag: "ImageVolume=false"}},
		},
		{
			name:    "ImageVolume=false on a component NVKE doesn't set it on",
			version: imageVolumeVersion,
			args:    ExtraArgs{Scheduler: map[string]string{featureGatesFlag: "ImageVolume=false"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateExtraArgs(tt.version, tt.args)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestApplyExtraArgsFeatureGates(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		operator string
		want     string
	}{
		{
			name:     "merged with NVKE's gate and sorted",
			version:  imageVolumeVersion,
			operator: "SELinuxMount=true, MaxUnavailableStatefulSet=false",
			want:     "ImageVolume=true,MaxUnavailableStatefulSet=false,SELinuxMount=true",
		},
		{
			name:     "agreeing with NVKE's gate is not duplicated",
			version:  imageVolumeVersion,
			operator: "ImageVolume=true",
			want:     "ImageVolume=true",
		},
		{
			name:     "operator gates only once NVKE sets none",
			version:  noImageVolumeVersion,
			operator: "SELinuxMount=true,MaxUnavailableStatefulSet=true",
			want:     "MaxUnavailableStatefulSet=true,SELinuxMount=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validInitConfig()
			cfg.KubernetesVersion = tt.version
			cc := clusterConfiguration(cfg)
			require.NoError(t, applyExtraArgs(&cc, tt.version, ExtraArgs{
				APIServer: map[string]string{featureGatesFlag: tt.operator},
			}))
			assert.Equal(t, tt.want, cc.APIServer.ExtraArgs[featureGatesFlag])
		})
	}
}

// TestNVKESetFlagsMatchRenderer fails when the renderer sets a flag that
// nvkeSetFlags doesn't list, so an operator could override it, and when
// nvkeSetFlags lists a flag the renderer never sets, so the list has gone stale.
func TestNVKESetFlagsMatchRenderer(t *testing.T) {
	rendered := map[component]map[string]bool{}
	record := func(c component, names ...string) {
		if rendered[c] == nil {
			rendered[c] = map[string]bool{}
		}
		for _, name := range names {
			rendered[c][name] = true
		}
	}

	for _, cfg := range rendererConfigurations() {
		cc := clusterConfiguration(cfg)
		record(componentAPIServer, slices.Collect(maps.Keys(cc.APIServer.ExtraArgs))...)
		record(componentControllerManager, slices.Collect(maps.Keys(cc.ControllerManager.ExtraArgs))...)
		record(componentScheduler, slices.Collect(maps.Keys(cc.Scheduler.ExtraArgs))...)

		// The kubeadm-config rewrites set flags after bootstrap, in either shape.
		doc := any(cc)
		if usesKubeadmV1beta4(cfg.KubernetesVersion) {
			doc = clusterConfigurationV1beta4(cc)
		}
		configYAML, err := yaml.Marshal(doc)
		require.NoError(t, err)
		for _, merge := range []func([]byte) ([]byte, bool, error){
			MergeOIDCIntoClusterConfiguration,
			MergeEncryptionIntoClusterConfiguration,
			MergeBootstrapDefragIntoClusterConfiguration,
		} {
			merged, _, err := merge(configYAML)
			require.NoError(t, err)
			for c, names := range renderedFlagNames(t, merged, cfg.KubernetesVersion) {
				record(c, names...)
			}
		}
	}

	for c, names := range rendered {
		for name := range names {
			if name == featureGatesFlag {
				continue
			}
			assert.Contains(t, nvkeSetFlags(c), name, "the renderer sets %s on %s, which is missing from nvkeSetFlags", name, c)
		}
	}
	for _, c := range []component{componentAPIServer, componentControllerManager, componentScheduler} {
		for _, name := range nvkeSetFlags(c) {
			assert.True(t, rendered[c][name], "nvkeSetFlags lists %s on %s, which the renderer never sets", name, c)
		}
	}
}

// rendererConfigurations covers every input that changes the flags
// clusterConfiguration sets.
func rendererConfigurations() []*InitConfig {
	versions := []string{"v1.30.14", unsupportedK8sVersion, supportedK8sVersion, imageVolumeVersion, noImageVolumeVersion}
	out := make([]*InitConfig, 0, len(versions)*8)
	for _, version := range versions {
		for _, encryption := range []bool{false, true} {
			for _, auth := range []*OIDCAuthenticationConfig{nil, testAuthenticationConfig()} {
				for _, containerMode := range []bool{false, true} {
					cfg := validInitConfig()
					cfg.KubernetesVersion = version
					cfg.EnableEncryption = encryption
					cfg.AuthenticationConfig = auth
					cfg.KubeadmContainerMode = containerMode
					cfg.SecondaryNodeEvictionRate = DefaultSecondaryNodeEvictionRate
					cfg.LargeClusterSizeThreshold = DefaultLargeClusterSizeThreshold
					out = append(out, cfg)
				}
			}
		}
	}
	return out
}

func renderedFlagNames(t *testing.T, clusterConfigYAML []byte, kubernetesVersion string) map[component][]string {
	t.Helper()
	if !usesKubeadmV1beta4(kubernetesVersion) {
		var cc kubeadmv1beta3.ClusterConfiguration
		require.NoError(t, yaml.Unmarshal(clusterConfigYAML, &cc))
		return map[component][]string{
			componentAPIServer:         slices.Collect(maps.Keys(cc.APIServer.ExtraArgs)),
			componentControllerManager: slices.Collect(maps.Keys(cc.ControllerManager.ExtraArgs)),
			componentScheduler:         slices.Collect(maps.Keys(cc.Scheduler.ExtraArgs)),
		}
	}
	var cc kubeadmv1beta4.ClusterConfiguration
	require.NoError(t, yaml.Unmarshal(clusterConfigYAML, &cc))
	names := func(args []kubeadmv1beta4.Arg) []string {
		out := make([]string, 0, len(args))
		for _, arg := range args {
			out = append(out, arg.Name)
		}
		return out
	}
	return map[component][]string{
		componentAPIServer:         names(cc.APIServer.ExtraArgs),
		componentControllerManager: names(cc.ControllerManager.ExtraArgs),
		componentScheduler:         names(cc.Scheduler.ExtraArgs),
	}
}

// testExtraArgs sets different arguments on each component for the golden files.
func testExtraArgs() ExtraArgs {
	return ExtraArgs{
		APIServer: map[string]string{
			featureGatesFlag: "MaxUnavailableStatefulSet=true",
			"runtime-config": "resource.k8s.io/v1beta2=true",
		},
		ControllerManager: map[string]string{
			featureGatesFlag:              "MaxUnavailableStatefulSet=true",
			"concurrent-deployment-syncs": "10",
		},
		Scheduler: map[string]string{
			featureGatesFlag: "SchedulerAsyncAPICalls=true",
		},
	}
}
