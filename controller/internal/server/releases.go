// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/machinerelease"
)

type releaseService struct {
	k8sReader ctrlclient.Reader
	logger    *slog.Logger
}

// NewReleaseService creates a new release service backed by the releases.
func NewReleaseService(logger *slog.Logger, k8sReader ctrlclient.Reader) *releaseService {
	return &releaseService{
		k8sReader: k8sReader,
		logger:    logger,
	}
}

// ListReleases returns the names of the releases that can be selected for new
// clusters and node pools.
func (s *releaseService) ListReleases(
	ctx context.Context,
	req api.ListReleasesRequestObject,
) (api.ListReleasesResponseObject, error) {
	if s.k8sReader == nil {
		return nil, errors.New("release reader is not configured")
	}

	var releases enginev1.ClusterReleaseList
	if err := s.k8sReader.List(ctx, &releases); err != nil {
		return nil, fmt.Errorf("failed to list ClusterReleases: %w", err)
	}
	names := make([]string, 0, len(releases.Items))
	for i := range releases.Items {
		if !releases.Items[i].Spec.Lifecycle.Deprecated {
			names = append(names, releases.Items[i].Name)
		}
	}
	slices.Sort(names)

	releaseList := make([]api.Release, 0, len(names))
	for _, name := range names {
		releaseList = append(releaseList, api.Release{Name: name})
	}

	return api.ListReleases200JSONResponse{Releases: releaseList}, nil
}

// validateClusterReleaseSelectable rejects selecting a ClusterRelease for the
// control plane when it is not installed or is deprecated.
func validateClusterReleaseSelectable(ctx context.Context, reader ctrlclient.Reader, name string) error {
	if reader == nil || name == "" {
		return nil
	}
	release, err := machinerelease.GetClusterRelease(ctx, reader, name)
	if err != nil {
		return releaseLookupError(name, err)
	}
	return deprecatedReleaseError(name, release.Spec.Lifecycle.Deprecated)
}

// validateNodePoolReleaseSelectable rejects selecting a NodePoolRelease for a
// worker pool when it is not installed or is deprecated.
func validateNodePoolReleaseSelectable(ctx context.Context, reader ctrlclient.Reader, name string) error {
	if reader == nil || name == "" {
		return nil
	}
	release, err := machinerelease.GetNodePoolRelease(ctx, reader, name)
	if err != nil {
		return releaseLookupError(name, err)
	}
	return deprecatedReleaseError(name, release.Spec.Lifecycle.Deprecated)
}

// validateCreateNodePoolRelease rejects a node pool create that selects a
// NodePoolRelease that is not installed or is deprecated.
func (s *nodePoolService) validateCreateNodePoolRelease(ctx context.Context, req *api.CreateNodePoolRequest) error {
	if req.Nico == nil {
		return nil
	}
	return validateNodePoolReleaseSelectable(ctx, s.releaseReader, req.Nico.Release)
}

// validateUpdateNodePoolRelease rejects switching a node pool to a
// NodePoolRelease that is not installed or is deprecated. Resending the release
// the pool already has passes, so a pool on such a release stays patchable.
func (s *nodePoolService) validateUpdateNodePoolRelease(ctx context.Context, nodePool db.NodePool, req *api.UpdateNodePoolRequest) error {
	if req.Nico == nil || req.Nico.Release == nil {
		return nil
	}
	if nodePool.Nico != nil && nodePool.Nico.Release == *req.Nico.Release {
		return nil
	}
	return validateNodePoolReleaseSelectable(ctx, s.releaseReader, *req.Nico.Release)
}

// validateCreateClusterReleases rejects a cluster create that selects a release
// that is not installed or is deprecated, for its control plane or for any of
// its initial node pools.
func (s *clusterService) validateCreateClusterReleases(ctx context.Context, req *api.CreateClusterRequest) error {
	if req.Nico == nil {
		return nil
	}
	if req.ControlPlaneConfig != nil && req.ControlPlaneConfig.Nico != nil {
		if err := validateClusterReleaseSelectable(ctx, s.releaseReader, req.ControlPlaneConfig.Nico.Release); err != nil {
			return err
		}
	}
	if req.NodePools == nil {
		return nil
	}
	for _, nodePool := range *req.NodePools {
		if nodePool.Nico == nil {
			continue
		}
		if err := validateNodePoolReleaseSelectable(ctx, s.releaseReader, nodePool.Nico.Release); err != nil {
			return err
		}
	}
	return nil
}

// validateControlPlaneReleaseChange rejects switching the control plane to a
// ClusterRelease that is not installed or is deprecated. Resending the current
// release passes.
func (s *clusterService) validateControlPlaneReleaseChange(ctx context.Context, current string, requested *string) error {
	if requested == nil || *requested == current {
		return nil
	}
	return validateClusterReleaseSelectable(ctx, s.releaseReader, *requested)
}

func releaseLookupError(name string, err error) error {
	if errors.Is(err, machinerelease.ErrInvalidRelease) {
		return newBadRequestErrorf("release %q is not installed", name)
	}
	return fmt.Errorf("checking release %q: %w", name, err)
}

func deprecatedReleaseError(name string, deprecated bool) error {
	if !deprecated {
		return nil
	}
	return newBadRequestErrorf("release %q is deprecated and cannot be selected", name)
}
