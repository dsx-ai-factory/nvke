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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kubeadmConfigNamespace = "kube-system"
	kubeadmConfigName      = "kubeadm-config"
	// clusterConfigKey is the kubeadm-config ConfigMap data key holding the
	// ClusterConfiguration YAML that kubeadm re-renders the control plane from.
	clusterConfigKey = "ClusterConfiguration"
)

// EnsureOIDCInClusterConfiguration reads kube-system/kubeadm-config via c, merges
// the OIDC apiserver settings into its ClusterConfiguration, and writes it back
// only when the merge changed something. It is idempotent: a cluster already
// carrying the settings is a no-op (changed=false).
//
// No kubeadm-config-validate gate: greenfield renders the byte-identical
// extraArg+volume via typed structs with no validate (init.go), so parity needs
// none.
func EnsureOIDCInClusterConfiguration(ctx context.Context, c client.Client) (bool, error) {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: kubeadmConfigNamespace, Name: kubeadmConfigName}
	if err := c.Get(ctx, key, &cm); err != nil {
		return false, fmt.Errorf("getting %s/%s: %w", kubeadmConfigNamespace, kubeadmConfigName, err)
	}

	raw, ok := cm.Data[clusterConfigKey]
	if !ok || raw == "" {
		return false, fmt.Errorf("%s/%s has an empty or missing %s data key", kubeadmConfigNamespace, kubeadmConfigName, clusterConfigKey)
	}

	merged, changed, err := MergeOIDCIntoClusterConfiguration([]byte(raw))
	if err != nil {
		return false, fmt.Errorf("merging OIDC into ClusterConfiguration: %w", err)
	}
	if !changed {
		return false, nil
	}

	// cm.Data is non-nil here: reaching this point required a successful read of
	// cm.Data[clusterConfigKey] above.
	cm.Data[clusterConfigKey] = string(merged)
	if err = c.Update(ctx, &cm); err != nil {
		return false, fmt.Errorf("updating %s/%s: %w", kubeadmConfigNamespace, kubeadmConfigName, err)
	}
	return true, nil
}
