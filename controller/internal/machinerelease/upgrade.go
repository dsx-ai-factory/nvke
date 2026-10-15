// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package machinerelease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// SelectUpgradeTarget returns the name and image of the newest non-deprecated
// release that can boot kubernetesVersion.
func SelectUpgradeTarget(
	ctx context.Context,
	reader client.Reader,
	controlPlane bool,
	kubernetesVersion string,
	current *enginev1.MachineImage,
) (string, *enginev1.MachineImage, error) {
	var want enginev1.MachineImageSpec
	if current != nil {
		want = current.Spec
	}

	var (
		bestName  string
		bestImage *enginev1.MachineImage
		images    = map[enginev1.ResourceName]*enginev1.MachineImage{}
	)
	consider := func(kind string, release metav1.Object, deprecated bool, ref enginev1.ResourceName) error {
		if deprecated {
			return nil
		}
		image, matched, err := upgradeCandidate(ctx, reader, images, release, ref, kubernetesVersion, want)
		if errors.Is(err, ErrInvalidRelease) {
			slog.WarnContext(ctx, "skipping release that cannot be resolved", "kind", kind, "release", release.GetName(), "error", err)
			return nil
		}
		if err != nil || !matched {
			return err
		}
		if bestImage == nil || newer(release.GetName(), image, bestName, bestImage) {
			bestName, bestImage = release.GetName(), image
		}
		return nil
	}

	if controlPlane {
		var list enginev1.ClusterReleaseList
		if err := reader.List(ctx, &list); err != nil {
			return "", nil, listErr(kindClusterRelease, err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			err := consider(kindClusterRelease, item, item.Spec.Lifecycle.Deprecated, item.Spec.MachineImageRef.Name)
			if err != nil {
				return "", nil, err
			}
		}
	} else {
		var list enginev1.NodePoolReleaseList
		if err := reader.List(ctx, &list); err != nil {
			return "", nil, listErr(kindNodePoolRelease, err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			err := consider(kindNodePoolRelease, item, item.Spec.Lifecycle.Deprecated, item.Spec.MachineImageRef.Name)
			if err != nil {
				return "", nil, err
			}
		}
	}

	if bestImage == nil {
		return "", nil, fmt.Errorf("no release found for Kubernetes version %s (architecture: %s, variant: %s, ubuntuVersion: %s)",
			kubernetesVersion, want.Architecture, want.Variant, want.OperatingSystem.Version)
	}
	return bestName, bestImage, nil
}

// upgradeCandidate reports whether a release can be the upgrade target and, if
// so, returns the image it boots. images caches the images read so far.
func upgradeCandidate(
	ctx context.Context,
	reader client.Reader,
	images map[enginev1.ResourceName]*enginev1.MachineImage,
	release metav1.Object,
	ref enginev1.ResourceName,
	kubernetesVersion string,
	want enginev1.MachineImageSpec,
) (*enginev1.MachineImage, bool, error) {
	version, injected, err := ParseKubernetesVersionAnnotations(release)
	if err != nil {
		return nil, false, err
	}
	if !injected && version != kubernetesVersion {
		return nil, false, nil
	}
	image := images[ref]
	if image == nil {
		if image, err = GetMachineImage(ctx, reader, ref); err != nil {
			return nil, false, err
		}
		images[ref] = image
	}
	if image.Spec.Architecture != want.Architecture || image.Spec.Variant != want.Variant ||
		image.Spec.OperatingSystem.Family != want.OperatingSystem.Family ||
		image.Spec.OperatingSystem.Version != want.OperatingSystem.Version {
		return nil, false, nil
	}
	return image, true, nil
}

// newer reports whether the candidate was built after the current best, with the
// release name as the tie-break.
func newer(name string, image *enginev1.MachineImage, bestName string, bestImage *enginev1.MachineImage) bool {
	if image.Spec.BuildDate != bestImage.Spec.BuildDate {
		return image.Spec.BuildDate > bestImage.Spec.BuildDate
	}
	return name > bestName
}
