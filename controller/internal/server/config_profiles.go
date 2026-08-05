// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/config"
)

type configProfileService struct {
	cfg    config.Config
	logger *slog.Logger
}

// NewConfigProfileService creates a new config profile service.
func NewConfigProfileService(cfg config.Config, logger *slog.Logger) *configProfileService {
	return &configProfileService{
		cfg:    cfg,
		logger: logger,
	}
}

// ListConfigProfiles returns a list of available config profiles.
func (s *configProfileService) ListConfigProfiles(
	ctx context.Context,
	req api.ListConfigProfilesRequestObject,
) (api.ListConfigProfilesResponseObject, error) {
	configProfilesDir := path.Join(s.cfg.ProfilesDir, "configProfiles")
	entries, err := os.ReadDir(configProfilesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read configProfiles directory: %w", err)
	}

	configProfilesList := []api.ConfigProfile{}
	for _, entry := range entries {
		yamlSuffix := ".yaml"
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), yamlSuffix) {
			baseName := strings.TrimSuffix(entry.Name(), yamlSuffix)
			configProfilesList = append(configProfilesList, api.ConfigProfile{Name: baseName})
		}
	}

	return api.ListConfigProfiles200JSONResponse{ConfigProfiles: configProfilesList}, nil
}
