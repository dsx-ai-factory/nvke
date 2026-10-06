// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"fmt"
	"log/slog"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/utils"
)

// ResolveNodePoolUpgrade updates nodePool's KubernetesVersion and, if the
// node pool's current OSProfile embeds the Kubernetes version rather than
// having it injected at provisioning time, resolves a new OSProfile that
// matches targetKubernetesVersion while preserving the current profile's
// architecture, variant, and Ubuntu version.
func (*Client) ResolveNodePoolUpgrade(profilesDir string, nodePool db.NodePool, targetKubernetesVersion string, logger *slog.Logger) (db.NodePool, error) {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return db.NodePool{}, err
	}
	osProfile := nicoNodePoolProvider.OSProfile

	kubeVersionInjected, err := utils.IsKubernetesVersionInjected(profilesDir, osProfile)
	if err != nil {
		return db.NodePool{}, fmt.Errorf("failed to get NodePool KubernetesVersion injection status for NodePool %s: %w", nodePool.ID, err)
	}

	// If the NodePool's OSProfile allows the KubernetesVersion to be injected at provisioning time, use the NodePool's KubernetesVersion.
	// Else use the old OSProfile where KubernetesVersion was embedded into the OSProfile.
	if kubeVersionInjected {
		logger.Info("KubernetesVersion is injected at provisioning time in NodePool", "nodePoolKubernetesVersion", nodePool.KubernetesVersion)
		nodePool.KubernetesVersion = targetKubernetesVersion
		return nodePool, nil
	}

	logger.Info("KubernetesVersion is not injected at provisioning time in NodePool - resolving new OSProfile", "nodePoolKubernetesVersion", nodePool.KubernetesVersion)
	// Pass current profile name to preserve architecture, variant, and os_version
	newOSProfile, err := resolveOSProfile(profilesDir, targetKubernetesVersion, osProfile, logger)
	if err != nil {
		return db.NodePool{}, fmt.Errorf("failed to resolve OSProfile: %w", err)
	}
	logger.Info("KubernetesVersion embedded in OSProfile - Resolved new OSProfile", "newOSProfile", newOSProfile)
	nicoNodePoolProvider.OSProfile = newOSProfile
	nodePool.SetNico(nicoNodePoolProvider)
	nodePool.KubernetesVersion = targetKubernetesVersion

	return nodePool, nil
}

// resolveOSProfile finds the newest-built OSProfile matching k8sVersion that
// preserves currentProfileName's architecture, variant, and Ubuntu version.
func resolveOSProfile(profilesDir, k8sVersion, currentProfileName string, logger *slog.Logger) (string, error) {
	metadataIdx, err := utils.LoadOSProfileMetadata(profilesDir)
	if err != nil {
		return "", fmt.Errorf("failed to load OSProfile metadata: %w", err)
	}

	// Get metadata from current profile to preserve architecture, variant, and ubuntu_version.
	var architecture string
	var variant string
	var ubuntuVersion string

	if currentProfileName != "" {
		currentMeta, ok := metadataIdx.GetOSProfileMetadata(currentProfileName, true)
		if !ok {
			return "", fmt.Errorf("current OSProfile %q not found in metadata; cannot resolve target profile without architecture, variant, and ubuntu version (add profile to metadata or fix profile name)", currentProfileName)
		}
		architecture = currentMeta.Architecture
		variant = currentMeta.Variant
		ubuntuVersion = currentMeta.UbuntuVersion
		logger.Info("Using metadata from current profile",
			"currentProfile", currentProfileName,
			"architecture", architecture,
			"variant", variant,
			"ubuntuVersion", ubuntuVersion)
	}

	// Find profiles matching the k8s version and metadata criteria.
	profiles := metadataIdx.FindOSProfiles(k8sVersion, architecture, variant, ubuntuVersion)
	if len(profiles) == 0 {
		return "", fmt.Errorf("no OSProfile found for Kubernetes version %s (architecture: %s, variant: %s, ubuntuVersion: %s)",
			k8sVersion, architecture, variant, ubuntuVersion)
	}

	// Pick the profile with the newest build date
	var profileName string
	var meta utils.Metadata
	var newestDate string

	for name, m := range profiles {
		if profileName == "" || m.BuildDate > newestDate {
			profileName = name
			meta = m
			newestDate = m.BuildDate
		}
	}

	logger.Info("Resolved OSProfile",
		"k8sVersion", k8sVersion,
		"selectedOSProfile", profileName,
		"architecture", meta.Architecture,
		"variant", meta.Variant,
		"ubuntuVersion", meta.UbuntuVersion,
		"buildDate", meta.BuildDate)

	return profileName, nil
}
