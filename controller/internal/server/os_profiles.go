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

type osProfileService struct {
	k8sReader ctrlclient.Reader
	logger    *slog.Logger
}

// NewOSProfileService creates a new OS profile service backed by the releases.
func NewOSProfileService(logger *slog.Logger, k8sReader ctrlclient.Reader) *osProfileService {
	return &osProfileService{
		k8sReader: k8sReader,
		logger:    logger,
	}
}

// ListOSProfiles returns the names of the OS profiles that can be selected for
// new clusters and node pools.
func (s *osProfileService) ListOSProfiles(
	ctx context.Context,
	req api.ListOSProfilesRequestObject,
) (api.ListOSProfilesResponseObject, error) {
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

	osProfilesList := make([]api.OSProfile, 0, len(names))
	for _, name := range names {
		osProfilesList = append(osProfilesList, api.OSProfile{Name: name})
	}

	return api.ListOSProfiles200JSONResponse{OsProfiles: osProfilesList}, nil
}
