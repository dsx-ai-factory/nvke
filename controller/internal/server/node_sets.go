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
	"errors"
	"fmt"
	"log/slog"

	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type nodeSetService struct {
	logger         *slog.Logger
	db             db.DB
	temporalClient client.Client
}

func NewNodeSetService(logger *slog.Logger, database db.DB, temporalClient client.Client) *nodeSetService {
	return &nodeSetService{
		logger:         logger,
		db:             database,
		temporalClient: temporalClient,
	}
}

func (s *nodeSetService) ListNodeSets(ctx context.Context, req api.ListNodeSetsRequestObject) (api.ListNodeSetsResponseObject, error) {
	filters := db.NodeSetFilters{
		NodePoolID: req.Params.NodePoolID,
	}

	dbNodeSets, err := s.db.ListNodeSets(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list node sets: %w", err)
	}

	apiNodeSets := make([]api.NodeSet, len(dbNodeSets))
	for i := range dbNodeSets {
		apiNodeSets[i], err = apiNodeSetFromDB(&dbNodeSets[i])
		if err != nil {
			return nil, fmt.Errorf("failed to convert node set to API model: %w", err)
		}
	}

	return api.ListNodeSets200JSONResponse{NodeSets: apiNodeSets}, nil
}

func (s *nodeSetService) GetNodeSet(ctx context.Context, req api.GetNodeSetRequestObject) (api.GetNodeSetResponseObject, error) {
	dbNodeSet, err := s.db.GetNodeSet(ctx, req.NodeSetID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetNodeSet404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get node set: %w", err)
	}

	apiNodeSet, err := apiNodeSetFromDB(&dbNodeSet)
	if err != nil {
		return nil, fmt.Errorf("failed to convert node set to API model: %w", err)
	}

	return api.GetNodeSet200JSONResponse(apiNodeSet), nil
}

// apiNodeSetFromDB converts a database NodeSet to an API NodeSet
func apiNodeSetFromDB(dbNodeSet *db.NodeSet) (api.NodeSet, error) {
	if dbNodeSet == nil {
		return api.NodeSet{}, nil
	}
	capiInfraMachineTemplate, err := objectFromRawManifest(dbNodeSet.CapiInfraMachineTemplate)
	if err != nil {
		return api.NodeSet{}, fmt.Errorf(
			"objectFromRawManifest failed for capiInfraMachineTemplate on node set %s (nodePoolID=%s, preview=%q): %w",
			dbNodeSet.ID,
			dbNodeSet.NodePoolID,
			truncateManifestForLog(dbNodeSet.CapiInfraMachineTemplate, 200),
			err,
		)
	}
	var capiInfraMachineTemplatePtr *map[string]interface{}
	if len(capiInfraMachineTemplate) > 0 {
		capiInfraMachineTemplatePtr = &capiInfraMachineTemplate
	}
	var kubeReservedMemory *string
	if dbNodeSet.KubeReservedMemory != "" {
		kubeReservedMemory = &dbNodeSet.KubeReservedMemory
	}

	return api.NodeSet{
		ID:                    dbNodeSet.ID,
		NodePoolID:            dbNodeSet.NodePoolID,
		DesiredCount:          dbNodeSet.DesiredCount,
		OSProfile:             dbNodeSet.OSProfile,
		ConfigProfile:         dbNodeSet.ConfigProfile,
		SystemReservedMemory:  dbNodeSet.SystemReservedMemory,
		KubeReservedMemory:    kubeReservedMemory,
		KubernetesVersion:     dbNodeSet.KubernetesVersion,
		TeleportEnabled:       &dbNodeSet.TeleportEnabled,
		DiskSelectionStrategy: api.DiskSelectionStrategy(dbNodeSet.DiskSelectionStrategy),

		CapiInfraMachineTemplate: capiInfraMachineTemplatePtr,
		CapiBootstrapFiles:       apiCloudInitFilesFromDB(dbNodeSet.CapiBootstrapFiles),
		CapiPreKubeadmCommands:   stringSlicePointer(dbNodeSet.CapiPreKubeadmCommands),
	}, nil
}

func truncateManifestForLog(raw []byte, limit int) string {
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}
