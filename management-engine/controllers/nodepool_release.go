// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Worker release resolution selects the NodePoolRelease and MachineImage a new node pool renders from.

package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// ReleaseSelectionError identifies a node pool whose worker release cannot be used.
// Reason is the NodePoolReleaseResolved condition reason that explains it.
type ReleaseSelectionError struct{ Reason, Message string }

func (e *ReleaseSelectionError) Error() string { return e.Message }

func releaseSelectionError(reason, format string, args ...any) error {
	return &ReleaseSelectionError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// splitNodeProfile splits a pool's <profile>:<tag> node profile into its two halves.
func splitNodeProfile(ref internalv1.NodeProfileReference) (profile, tag string, err error) {
	profile, tag, found := strings.Cut(string(ref), ":")
	if !found {
		return "", "", fmt.Errorf("node profile %q must be <profile>:<tag>", ref)
	}
	if profile == "" || tag == "" {
		return "", "", fmt.Errorf("node profile %q must have a non-empty profile and tag", ref)
	}
	return profile, tag, nil
}

// resolveNodePoolRelease selects the worker release and image a pool renders from.
// A pinned release is fetched by name and may be deprecated. Without a pin, the pool's
// node profile must match exactly one release that is not deprecated.
func (r *NVKEClusterReconciler) resolveNodePoolRelease(ctx context.Context, pool *internalv1.NodePool, pinned *internalv1.ReleaseReference) (*internalv1.NodePoolRelease, *internalv1.MachineImage, error) {
	var release *internalv1.NodePoolRelease
	var err error
	if pinned != nil {
		release, err = r.getPinnedNodePoolRelease(ctx, pinned)
	} else {
		release, err = r.matchNodePoolRelease(ctx, pool)
	}
	if err != nil {
		return nil, nil, err
	}

	var image internalv1.MachineImage
	imageName := string(release.Spec.MachineImageRef.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: imageName}, &image); err != nil {
		return nil, nil, releaseSelectionError(internalv1.ReleaseInvalidReason, "get MachineImage %q for NodePoolRelease %q: %v", imageName, release.Name, err)
	}
	return release, &image, nil
}

func (r *NVKEClusterReconciler) getPinnedNodePoolRelease(ctx context.Context, pinned *internalv1.ReleaseReference) (*internalv1.NodePoolRelease, error) {
	var release internalv1.NodePoolRelease
	if err := r.Get(ctx, client.ObjectKey{Name: string(pinned.Name)}, &release); err != nil {
		reason := internalv1.ReleaseInvalidReason
		if apierrors.IsNotFound(err) {
			reason = internalv1.ReleaseNotFoundReason
		}
		return nil, releaseSelectionError(reason, "get pinned NodePoolRelease %q: %v", pinned.Name, err)
	}
	return &release, nil
}

func (r *NVKEClusterReconciler) matchNodePoolRelease(ctx context.Context, pool *internalv1.NodePool) (*internalv1.NodePoolRelease, error) {
	profile, tag, err := splitNodeProfile(pool.NodeProfile)
	if err != nil {
		return nil, releaseSelectionError(internalv1.ReleaseInvalidReason, "resolve node profile: %v", err)
	}
	var list internalv1.NodePoolReleaseList
	if err := r.List(ctx, &list); err != nil {
		return nil, releaseSelectionError(internalv1.ReleaseInvalidReason, "list NodePoolReleases for node profile %q: %v", pool.NodeProfile, err)
	}
	var matches []*internalv1.NodePoolRelease
	for i := range list.Items {
		if spec := list.Items[i].Spec; spec.Profile == profile && spec.Tag == tag {
			matches = append(matches, &list.Items[i])
		}
	}
	switch len(matches) {
	case 0:
		return nil, releaseSelectionError(internalv1.ReleaseNotFoundReason, "no NodePoolRelease matches node profile %q", pool.NodeProfile)
	case 1:
	default:
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Name)
		}
		// List order is not stable, and the message must not depend on it.
		slices.Sort(names)
		return nil, releaseSelectionError(internalv1.ReleaseInvalidReason, "node profile %q is ambiguous: NodePoolReleases %s all match", pool.NodeProfile, strings.Join(names, ", "))
	}
	if matches[0].Spec.Lifecycle.Deprecated {
		return nil, releaseSelectionError(internalv1.ReleaseDeprecatedReason, "NodePoolRelease %q for node profile %q is deprecated", matches[0].Name, pool.NodeProfile)
	}
	return matches[0], nil
}
