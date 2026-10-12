// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/machinerelease"
)

// resolvedRelease is everything one release read yields: the release itself,
// for its annotations; the first-boot template a node renders; and the image
// it boots, which carries the iPXE script and the metadata an upgrade target
// has to match.
type resolvedRelease struct {
	release          metav1.Object
	userDataTemplate string
	image            *enginev1.MachineImage
}

// resolveRelease reads the release a node pool or node set stores by name, and
// the image it boots. A control-plane node reads a ClusterRelease and a worker
// a NodePoolRelease; there is no fallback between them.
func resolveRelease(ctx context.Context, reader ctrlclient.Reader, controlPlane bool, name string) (resolvedRelease, error) {
	if controlPlane {
		release, image, err := machinerelease.ResolveClusterRelease(ctx, reader, name)
		if err != nil {
			return resolvedRelease{}, fmt.Errorf("resolving OSProfile %q: %w", name, err)
		}
		return resolvedRelease{release: release, userDataTemplate: release.Spec.MachineInit.UserDataTemplate, image: image}, nil
	}
	release, image, err := machinerelease.ResolveNodePoolRelease(ctx, reader, name)
	if err != nil {
		return resolvedRelease{}, fmt.Errorf("resolving OSProfile %q: %w", name, err)
	}
	return resolvedRelease{release: release, userDataTemplate: release.Spec.MachineInit.UserDataTemplate, image: image}, nil
}
