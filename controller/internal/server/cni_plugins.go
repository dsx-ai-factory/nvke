// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type cniPluginsService struct{}

func NewCNIPluginsService() *cniPluginsService {
	return &cniPluginsService{}
}

func (s *cniPluginsService) ListCNIPlugins(
	_ context.Context,
	_ api.ListCNIPluginsRequestObject,
) (api.ListCNIPluginsResponseObject, error) {
	dbCNIPlugins := db.ListCNIPlugins()

	cniPlugins := make([]string, len(dbCNIPlugins))
	for i, plugin := range dbCNIPlugins {
		cniPlugins[i] = string(plugin)
	}

	return api.ListCNIPlugins200JSONResponse{CniPlugins: cniPlugins}, nil
}
