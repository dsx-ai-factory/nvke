// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"

	"github.com/dsx-ai-factory/nke/controller/api"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
)

type osProfileService struct {
	cfg    config.Config
	logger *slog.Logger
}

// NewOSProfileService creates a new OS profile service.
func NewOSProfileService(cfg config.Config, logger *slog.Logger) *osProfileService {
	return &osProfileService{
		cfg:    cfg,
		logger: logger,
	}
}

// ListOSProfiles returns a list of available OS profiles.
func (s *osProfileService) ListOSProfiles(
	ctx context.Context,
	req api.ListOSProfilesRequestObject,
) (api.ListOSProfilesResponseObject, error) {
	osProfiles, err := os.ReadDir(path.Join(s.cfg.ProfilesDir, "osProfiles"))
	if err != nil {
		return nil, fmt.Errorf("failed to read osProfiles directory: %w", err)
	}

	osProfilesList := []api.OSProfile{}
	for _, osProfile := range osProfiles {
		if osProfile.IsDir() && osProfile.Name() != "deprecated" {
			osProfilesList = append(osProfilesList, api.OSProfile{Name: osProfile.Name()})
		}
	}

	return api.ListOSProfiles200JSONResponse{OsProfiles: osProfilesList}, nil
}
