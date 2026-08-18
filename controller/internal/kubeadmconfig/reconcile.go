// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	return reconcileClusterConfiguration(ctx, c, "merging OIDC", MergeOIDCIntoClusterConfiguration, false)
}

// ValidateEncryptionInClusterConfiguration verifies that the persisted kubeadm
// ClusterConfiguration can be reconciled to the same API encryption settings
// emitted for greenfield clusters, without writing the ConfigMap.
func ValidateEncryptionInClusterConfiguration(ctx context.Context, c client.Client) error {
	_, err := reconcileClusterConfiguration(ctx, c, "validating API encryption", MergeEncryptionIntoClusterConfiguration, true)
	return err
}

// EnsureEncryptionInClusterConfiguration reconciles the persisted kubeadm
// ClusterConfiguration to the same API encryption settings emitted for
// greenfield clusters.
func EnsureEncryptionInClusterConfiguration(ctx context.Context, c client.Client) (bool, error) {
	return reconcileClusterConfiguration(ctx, c, "merging API encryption", MergeEncryptionIntoClusterConfiguration, false)
}

// EtcdQuotaInClusterConfiguration reads quota-backend-bytes from the persisted
// kubeadm ClusterConfiguration without changing the ConfigMap.
func EtcdQuotaInClusterConfiguration(ctx context.Context, c client.Client) (string, error) {
	_, raw, err := getClusterConfiguration(ctx, c)
	if err != nil {
		return "", err
	}
	quota, err := EtcdQuotaFromClusterConfiguration(raw)
	if err != nil {
		return "", fmt.Errorf("reading local etcd quota from ClusterConfiguration: %w", err)
	}
	return quota, nil
}

// EnsureKubernetesVersionInClusterConfiguration updates kubernetesVersion in
// the persisted kubeadm ClusterConfiguration only when it differs from version.
func EnsureKubernetesVersionInClusterConfiguration(ctx context.Context, c client.Client, version string) (bool, error) {
	return reconcileClusterConfiguration(
		ctx,
		c,
		"updating Kubernetes version",
		func(raw []byte) ([]byte, bool, error) {
			return SetKubernetesVersionInClusterConfiguration(raw, version)
		},
		false,
	)
}

type clusterConfigurationMutation func([]byte) ([]byte, bool, error)

func reconcileClusterConfiguration(
	ctx context.Context,
	c client.Client,
	operation string,
	mutation clusterConfigurationMutation,
	validateOnly bool,
) (bool, error) {
	cm, raw, err := getClusterConfiguration(ctx, c)
	if err != nil {
		return false, err
	}

	merged, changed, err := mutation(raw)
	if err != nil {
		return false, fmt.Errorf("%s in ClusterConfiguration: %w", operation, err)
	}
	if !changed || validateOnly {
		return changed, nil
	}

	cm.Data[clusterConfigKey] = string(merged)
	if err = c.Update(ctx, cm); err != nil {
		return false, fmt.Errorf("updating %s/%s: %w", kubeadmConfigNamespace, kubeadmConfigName, err)
	}
	return true, nil
}

func getClusterConfiguration(ctx context.Context, c client.Client) (*corev1.ConfigMap, []byte, error) {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: kubeadmConfigNamespace, Name: kubeadmConfigName}
	if err := c.Get(ctx, key, &cm); err != nil {
		return nil, nil, fmt.Errorf("getting %s/%s: %w", kubeadmConfigNamespace, kubeadmConfigName, err)
	}

	raw, ok := cm.Data[clusterConfigKey]
	if !ok || raw == "" {
		return nil, nil, fmt.Errorf("%s/%s has an empty or missing %s data key", kubeadmConfigNamespace, kubeadmConfigName, clusterConfigKey)
	}
	return &cm, []byte(raw), nil
}
