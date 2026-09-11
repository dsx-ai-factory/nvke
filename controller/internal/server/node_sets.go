// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
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
	if dbNodeSet.Capi != nil {
		if _, err := objectFromRawManifest(dbNodeSet.Capi.InfraMachineTemplate); err != nil {
			return api.NodeSet{}, fmt.Errorf(
				"objectFromRawManifest failed for infra.machineTemplate on node set %s (nodePoolID=%s, preview=%q): %w",
				dbNodeSet.ID,
				dbNodeSet.NodePoolID,
				truncateManifestForLog(dbNodeSet.Capi.InfraMachineTemplate, 200),
				err,
			)
		}
	}
	var kubeReservedMemory *string
	if dbNodeSet.KubeReservedMemory != "" {
		kubeReservedMemory = &dbNodeSet.KubeReservedMemory
	}

	return api.NodeSet{
		ID:                   dbNodeSet.ID,
		NodePoolID:           dbNodeSet.NodePoolID,
		DesiredCount:         dbNodeSet.DesiredCount,
		SystemReservedMemory: dbNodeSet.SystemReservedMemory,
		KubeReservedMemory:   kubeReservedMemory,
		KubernetesVersion:    dbNodeSet.KubernetesVersion,
		TeleportEnabled:      &dbNodeSet.TeleportEnabled,
		Nico:                 apiNicoNodeSetProviderFromDB(dbNodeSet.Nico),
		Infra:                apiInfraNodeSetProviderFromDB(dbNodeSet.Capi),
	}, nil
}

func apiInfraNodeSetProviderFromDB(capiNodeSetProvider *db.CAPINodePoolProvider) *api.InfraNodeSetProvider {
	if capiNodeSetProvider == nil {
		return nil
	}
	obj, err := objectFromRawManifest(capiNodeSetProvider.InfraMachineTemplate)
	if err != nil {
		obj = nil
	}
	out := &api.InfraNodeSetProvider{
		MachineTemplate:        obj,
		PreKubeadmCommands:     append([]string(nil), capiNodeSetProvider.PreKubeadmCommands...),
		SpireAttestationMethod: api.SpireAttestationMethodCreate(capiNodeSetProvider.SpireAttestationMethod),
	}
	if apiFiles := apiCloudInitFilesFromDB(capiNodeSetProvider.BootstrapFiles); apiFiles != nil {
		out.BootstrapFiles = *apiFiles
	}
	return out
}

func apiNicoNodeSetProviderFromDB(nicoNodeSetProvider *db.NicoNodePoolProvider) *api.NicoNodeSetProvider {
	if nicoNodeSetProvider == nil {
		return nil
	}
	return &api.NicoNodeSetProvider{
		SubnetIDs:             append([]uuid.UUID(nil), nicoNodeSetProvider.SubnetIDs...),
		VpcPrefixIDs:          append([]uuid.UUID(nil), nicoNodeSetProvider.VpcPrefixIDs...),
		OSProfile:             nicoNodeSetProvider.OSProfile,
		ConfigProfile:         nicoNodeSetProvider.ConfigProfile,
		DiskSelectionStrategy: api.DiskSelectionStrategyCreate(nicoNodeSetProvider.DiskSelectionStrategy),
	}
}

func truncateManifestForLog(raw []byte, limit int) string {
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}
