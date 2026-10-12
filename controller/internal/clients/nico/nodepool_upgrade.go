// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"fmt"
	"log/slog"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/machinerelease"
)

// ResolveNodePoolUpgrade updates nodePool's KubernetesVersion and, if the
// node pool's current release embeds the Kubernetes version rather than
// having it injected at provisioning time, selects a new release that
// matches targetKubernetesVersion while preserving the current release's
// architecture, variant, and Ubuntu version.
func (*Client) ResolveNodePoolUpgrade(
	ctx context.Context,
	reader ctrlclient.Reader,
	nodePool db.NodePool,
	targetKubernetesVersion string,
	logger *slog.Logger,
) (db.NodePool, error) {
	if reader == nil {
		return db.NodePool{}, fmt.Errorf("release reader is not configured")
	}

	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return db.NodePool{}, err
	}
	currentRelease := nicoNodePoolProvider.OSProfile

	// One read serves both: the annotations below decide whether a new release
	// is needed, and the image carries the metadata the selection matches on.
	resolved, err := resolveRelease(ctx, reader, nodePool.ControlPlane, currentRelease)
	if err != nil {
		return db.NodePool{}, fmt.Errorf("failed to resolve the current release of NodePool %s: %w", nodePool.ID, err)
	}
	_, injected, err := machinerelease.ParseKubernetesVersionAnnotations(resolved.release)
	if err != nil {
		return db.NodePool{}, fmt.Errorf("failed to read the KubernetesVersion annotations of NodePool %s: %w", nodePool.ID, err)
	}

	// If the NodePool's release allows the KubernetesVersion to be injected at provisioning time, use the NodePool's KubernetesVersion.
	// Else select the release that embeds the new KubernetesVersion.
	if injected {
		logger.Info("KubernetesVersion is injected at provisioning time in NodePool", "nodePoolKubernetesVersion", nodePool.KubernetesVersion)
		nodePool.KubernetesVersion = targetKubernetesVersion
		return nodePool, nil
	}

	logger.Info("KubernetesVersion is not injected at provisioning time in NodePool - selecting a new release", "nodePoolKubernetesVersion", nodePool.KubernetesVersion)
	// Pass the current release to preserve architecture, variant, and os_version
	newRelease, err := selectUpgradeRelease(ctx, reader, nodePool.ControlPlane, targetKubernetesVersion, currentRelease, resolved.image, logger)
	if err != nil {
		return db.NodePool{}, fmt.Errorf("failed to resolve OSProfile: %w", err)
	}
	logger.Info("KubernetesVersion embedded in the release - selected new release", "newRelease", newRelease)
	// The stored field keeps its name; it holds the release name.
	nicoNodePoolProvider.OSProfile = newRelease
	nodePool.SetNico(nicoNodePoolProvider)
	nodePool.KubernetesVersion = targetKubernetesVersion

	return nodePool, nil
}

// selectUpgradeRelease finds the release for a given Kubernetes version.
// currentImage carries the architecture, variant and ubuntu version the target
// has to match. The caller resolves it, so the current release is read once.
func selectUpgradeRelease(
	ctx context.Context,
	reader ctrlclient.Reader,
	controlPlane bool,
	k8sVersion, currentRelease string,
	currentImage *enginev1.MachineImage,
	logger *slog.Logger,
) (string, error) {
	if currentImage != nil {
		logger.Info("Using metadata from current release",
			"currentRelease", currentRelease,
			"architecture", currentImage.Spec.Architecture,
			"variant", currentImage.Spec.Variant,
			"ubuntuVersion", currentImage.Spec.OperatingSystem.Version)
	}

	name, image, err := machinerelease.SelectUpgradeTarget(ctx, reader, controlPlane, k8sVersion, currentImage)
	if err != nil {
		return "", err
	}

	logger.Info("Selected upgrade release",
		"k8sVersion", k8sVersion,
		"selectedRelease", name,
		"architecture", image.Spec.Architecture,
		"variant", image.Spec.Variant,
		"ubuntuVersion", image.Spec.OperatingSystem.Version,
		"buildDate", image.Spec.BuildDate)

	return name, nil
}
