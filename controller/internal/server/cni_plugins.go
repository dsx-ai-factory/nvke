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
