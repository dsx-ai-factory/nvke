// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package kubeadmconfig

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	kubeadmv1beta3 "github.com/NVIDIA/nke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
	kubeadmv1beta4 "github.com/NVIDIA/nke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta4"
)

const (
	// OIDCAuthConfigPath is the on-node path of the apiserver AuthenticationConfiguration
	// file. The greenfield path sets it via apiServer.extraArgs["authentication-config"];
	// the brownfield write-back mirrors the same value into the persisted kubeadm-config.
	OIDCAuthConfigPath = defaultOIDCAuthConfigPath

	// AuthConfigFlagName is the kube-apiserver flag (no leading dashes) that
	// points at the AuthenticationConfiguration file. It is the single source of
	// truth for the flag name across the greenfield renderer, the brownfield
	// write-back, and the on-node rollout operation, so all three converge on one
	// manifest.
	AuthConfigFlagName = "authentication-config"
	// AuthConfigVolumeName is the static-pod volume/volumeMount name for the
	// auth-config directory. Shared for the same convergence reason as
	// AuthConfigFlagName.
	AuthConfigVolumeName = "oidc-auth-config"
)

// MergeOIDCIntoClusterConfiguration takes the kubeadm ClusterConfiguration YAML
// document held in kube-system/kubeadm-config and returns it augmented with the
// OIDC apiserver settings the greenfield path applies: the authentication-config
// extraArg and the oidc-auth-config extraVolume.
//
// It decodes into the kubeadm struct matching the document's apiVersion (v1beta3
// or v1beta4), sets the arg and volume, and re-marshals — so the emitted
// extraArgs shape always matches the apiVersion (a v1beta3 map or a v1beta4
// list), which a later `kubeadm upgrade`/join re-render requires. The merge is
// idempotent and byte-stable: re-running it on its own output yields identical
// bytes and reports changed=false, so the caller can skip the ConfigMap write.
//
// The typed round-trip drops any field absent from the vendored kubeadm types;
// that is safe here because kubeadm authors this ConfigMap in one of the two
// supported versions, whose full ClusterConfiguration is mirrored under
// thirdparty/kubeadmtypes.
//
// Returns an error for malformed YAML, an unexpected document kind, or an
// unsupported apiVersion.
func MergeOIDCIntoClusterConfiguration(clusterConfigYAML []byte) ([]byte, bool, error) {
	var meta metav1.TypeMeta
	if err := yaml.Unmarshal(clusterConfigYAML, &meta); err != nil {
		return nil, false, fmt.Errorf("parsing ClusterConfiguration YAML: %w", err)
	}
	// The kubeadm apiVersion group is shared by ClusterConfiguration,
	// InitConfiguration, and JoinConfiguration, so gate on Kind too: a document of
	// another kind would otherwise pass the version switch and be silently
	// truncated by the typed round-trip instead of failing fast.
	if meta.Kind != kubeadmv1beta3.ClusterConfigurationKind {
		return nil, false, fmt.Errorf("unsupported kubeadm document kind %q (want %q)",
			meta.Kind, kubeadmv1beta3.ClusterConfigurationKind)
	}
	switch meta.APIVersion {
	case kubeadmv1beta3.APIVersion:
		return mergeV1beta3(clusterConfigYAML)
	case kubeadmv1beta4.APIVersion:
		return mergeV1beta4(clusterConfigYAML)
	default:
		return nil, false, fmt.Errorf("unsupported kubeadm ClusterConfiguration apiVersion %q (want %s or %s)",
			meta.APIVersion, kubeadmv1beta3.APIVersion, kubeadmv1beta4.APIVersion)
	}
}

// mergeV1beta3 applies the OIDC arg (map-shaped extraArgs) and volume to a
// v1beta3 ClusterConfiguration.
func mergeV1beta3(clusterConfigYAML []byte) ([]byte, bool, error) {
	var cc kubeadmv1beta3.ClusterConfiguration
	if err := yaml.Unmarshal(clusterConfigYAML, &cc); err != nil {
		return nil, false, fmt.Errorf("parsing v1beta3 ClusterConfiguration: %w", err)
	}

	args, argChanged := setArgV1beta3(cc.APIServer.ExtraArgs, AuthConfigFlagName, OIDCAuthConfigPath)
	cc.APIServer.ExtraArgs = args

	vols, volChanged := upsertVolumeV1beta3(cc.APIServer.ExtraVolumes, authenticationConfigVolume())
	cc.APIServer.ExtraVolumes = vols

	return marshalMerged(cc, argChanged || volChanged)
}

// mergeV1beta4 applies the OIDC arg (list-shaped extraArgs) and volume to a
// v1beta4 ClusterConfiguration.
func mergeV1beta4(clusterConfigYAML []byte) ([]byte, bool, error) {
	var cc kubeadmv1beta4.ClusterConfiguration
	if err := yaml.Unmarshal(clusterConfigYAML, &cc); err != nil {
		return nil, false, fmt.Errorf("parsing v1beta4 ClusterConfiguration: %w", err)
	}

	args, argChanged := upsertArgV1beta4(cc.APIServer.ExtraArgs, AuthConfigFlagName, OIDCAuthConfigPath)
	cc.APIServer.ExtraArgs = args

	vols, volChanged := upsertVolumeV1beta4(cc.APIServer.ExtraVolumes, authenticationConfigVolumeV1beta4())
	cc.APIServer.ExtraVolumes = vols

	return marshalMerged(cc, argChanged || volChanged)
}

func marshalMerged(cc any, changed bool) ([]byte, bool, error) {
	out, err := yaml.Marshal(cc)
	if err != nil {
		return nil, false, fmt.Errorf("serializing merged ClusterConfiguration: %w", err)
	}
	return out, changed, nil
}

// setArgV1beta3 sets name=value in a v1beta3 extraArgs map (creating it if nil).
// Reports whether the map changed.
func setArgV1beta3(args map[string]string, name, value string) (map[string]string, bool) {
	if args[name] == value {
		return args, false
	}
	if args == nil {
		args = map[string]string{}
	}
	args[name] = value
	return args, true
}

// upsertArgV1beta4 sets name=value in a v1beta4 extraArgs list: it replaces a
// same-named entry in place (so the merge is idempotent) or appends a new one.
// Reports whether the list changed.
func upsertArgV1beta4(args []kubeadmv1beta4.Arg, name, value string) ([]kubeadmv1beta4.Arg, bool) {
	for i := range args {
		if args[i].Name == name {
			if args[i].Value == value {
				return args, false
			}
			args[i].Value = value
			return args, true
		}
	}
	return append(args, kubeadmv1beta4.Arg{Name: name, Value: value}), true
}

// upsertVolumeV1beta3 ensures desired is present in a v1beta3 extraVolumes list,
// replacing a same-named entry so the desired fields win. Reports whether the
// list changed.
func upsertVolumeV1beta3(vols []kubeadmv1beta3.HostPathMount, desired kubeadmv1beta3.HostPathMount) ([]kubeadmv1beta3.HostPathMount, bool) {
	for i := range vols {
		if vols[i].Name == desired.Name {
			if vols[i] == desired {
				return vols, false
			}
			vols[i] = desired
			return vols, true
		}
	}
	return append(vols, desired), true
}

// upsertVolumeV1beta4 is upsertVolumeV1beta3 for the v1beta4 HostPathMount type.
func upsertVolumeV1beta4(vols []kubeadmv1beta4.HostPathMount, desired kubeadmv1beta4.HostPathMount) ([]kubeadmv1beta4.HostPathMount, bool) {
	for i := range vols {
		if vols[i].Name == desired.Name {
			if vols[i] == desired {
				return vols, false
			}
			vols[i] = desired
			return vols, true
		}
	}
	return append(vols, desired), true
}

// authenticationConfigVolumeV1beta4 mirrors authenticationConfigVolume() (the
// greenfield v1beta3 render) for the v1beta4 HostPathMount type, so both merge
// paths and the greenfield render converge on the same apiserver volume.
func authenticationConfigVolumeV1beta4() kubeadmv1beta4.HostPathMount {
	return kubeadmv1beta4.HostPathMount{
		Name:      AuthConfigVolumeName,
		HostPath:  defaultOIDCConfigDirectory,
		MountPath: defaultOIDCConfigDirectory,
		PathType:  corev1.HostPathDirectoryOrCreate,
		ReadOnly:  true,
	}
}
