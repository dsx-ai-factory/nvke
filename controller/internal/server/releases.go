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
