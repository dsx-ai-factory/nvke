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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
)

func (s *nodeSetService) GetNodePoolConfigHistory(ctx context.Context, req api.GetNodePoolConfigHistoryRequestObject) (api.GetNodePoolConfigHistoryResponseObject, error) {
	nodePool, err := s.db.GetNodePool(ctx, req.NodePoolID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetNodePoolConfigHistory404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get node pool: %w", err)
	}

	if s.temporalClient != nil {
		changes, err := s.configHistoryFromTemporal(ctx, nodePool)
		switch {
		case err != nil:
			s.logger.Warn("failed to query Temporal config history, falling back to NodeSet query",
				"nodePoolID", req.NodePoolID, "error", err)
		case len(changes) > 0:
			return api.GetNodePoolConfigHistory200JSONResponse{Changes: changes}, nil
		default:
			s.logger.Debug("Temporal returned no config history, falling back to NodeSet query",
				"nodePoolID", req.NodePoolID)
		}
	}

	return s.configHistoryFromNodeSets(ctx, nodePool)
}

func (s *nodeSetService) configHistoryFromTemporal(ctx context.Context, nodePool db.NodePool) ([]api.NodePoolConfigChange, error) {
	prefix := constants.RecordNodePoolConfigChangeWorkflowPrefix + nodePool.ID.String() + "-"
	query := fmt.Sprintf("WorkflowType = %q AND WorkflowId STARTS_WITH %q",
		constants.RecordNodePoolConfigChangeWorkflowType, prefix)

	var changes []api.NodePoolConfigChange
	var nextPageToken []byte

	for {
		resp, err := s.temporalClient.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:         query,
			PageSize:      100,
			NextPageToken: nextPageToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list Temporal workflows: %w", err)
		}

		for _, exec := range resp.GetExecutions() {
			input, startTime, err := s.fetchWorkflowInput(ctx, exec.GetExecution().GetWorkflowId(), exec.GetExecution().GetRunId())
			if err != nil {
				s.logger.Warn("failed to fetch workflow input",
					"workflowID", exec.GetExecution().GetWorkflowId(), "error", err)
				continue
			}

			isCurrent := nodePool.CurrentNodeSetID != nil && *nodePool.CurrentNodeSetID == input.NewNodeSetID
			changes = append(changes, api.NodePoolConfigChange{
				NodeSetID:     input.NewNodeSetID,
				CreatedAt:     startTime,
				IsCurrent:     isCurrent,
				ChangedFields: configDiffFromInput(input),
			})
		}

		nextPageToken = resp.GetNextPageToken()
		if len(nextPageToken) == 0 {
			break
		}
	}

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].CreatedAt.Before(changes[j].CreatedAt)
	})

	// Auto-provision-disabled pools (e.g. L0 platform-managed control planes) never transition
	// NodeSets, so their audit records carry synthetic NewNodeSetIDs that never match
	// CurrentNodeSetID. Mark the most recent change as current instead.
	if nodePool.AutoProvisionDisabled && len(changes) > 0 {
		for i := range changes {
			changes[i].IsCurrent = false
		}
		changes[len(changes)-1].IsCurrent = true
	}

	return changes, nil
}

// nodePoolConfigChangeInput builds an audit record for a direct config change to a node pool.
// It is used for auto-provision-disabled pools, whose config changes are never turned into a new
// NodeSet by the NodePoolController and would otherwise go unrecorded. NewNodeSetID is synthetic
// (there is no NodeSet transition) and only serves to key and de-duplicate the audit record;
// PrevNodeSetID is set to the pool's current NodeSet so configDiffFromInput renders the diff.
// The bool is true only when an audited field actually changed.
func nodePoolConfigChangeInput(old, updated db.NodePool) (workflows.RecordNodePoolConfigChangeInput, bool) {
	input := workflows.RecordNodePoolConfigChangeInput{
		NodePoolID:    updated.ID,
		NewNodeSetID:  uuid.New(),
		PrevNodeSetID: updated.CurrentNodeSetID,

		OldOSProfile: old.OSProfile,
		NewOSProfile: updated.OSProfile,

		OldConfigProfile: old.ConfigProfile,
		NewConfigProfile: updated.ConfigProfile,

		OldKubernetesVersion: old.KubernetesVersion,
		NewKubernetesVersion: updated.KubernetesVersion,

		OldSystemReservedMemory: old.SystemReservedMemory,
		NewSystemReservedMemory: updated.SystemReservedMemory,

		OldDiskSelectionStrategy: string(old.DiskSelectionStrategy),
		NewDiskSelectionStrategy: string(updated.DiskSelectionStrategy),

		OldCapiSpireAttestationMethod: string(old.CapiSpireAttestationMethod),
		NewCapiSpireAttestationMethod: string(updated.CapiSpireAttestationMethod),

		OldTeleportEnabled: old.TeleportEnabled,
		NewTeleportEnabled: updated.TeleportEnabled,

		OldSubnetIDs: old.SubnetIDs,
		NewSubnetIDs: updated.SubnetIDs,

		OldVpcPrefixIDs: old.VpcPrefixIDs,
		NewVpcPrefixIDs: updated.VpcPrefixIDs,

		OldCapiPreKubeadmCommands: old.CapiPreKubeadmCommands,
		NewCapiPreKubeadmCommands: updated.CapiPreKubeadmCommands,
	}

	return input, len(configDiffFromInput(input)) > 0
}

func (s *nodeSetService) fetchWorkflowInput(ctx context.Context, workflowID, runID string) (workflows.RecordNodePoolConfigChangeInput, time.Time, error) {
	iter := s.temporalClient.GetWorkflowHistory(ctx, workflowID, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return workflows.RecordNodePoolConfigChangeInput{}, time.Time{}, fmt.Errorf("error reading history: %w", err)
		}
		if event.GetEventType() != enums.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED {
			continue
		}
		attrs := event.GetWorkflowExecutionStartedEventAttributes()
		payloads := attrs.GetInput().GetPayloads()
		if len(payloads) == 0 {
			return workflows.RecordNodePoolConfigChangeInput{}, time.Time{}, fmt.Errorf("no input payloads in workflow history")
		}
		var input workflows.RecordNodePoolConfigChangeInput
		if err := json.Unmarshal(payloads[0].GetData(), &input); err != nil {
			return workflows.RecordNodePoolConfigChangeInput{}, time.Time{}, fmt.Errorf("failed to decode workflow input: %w", err)
		}
		return input, event.GetEventTime().AsTime(), nil
	}
	return workflows.RecordNodePoolConfigChangeInput{}, time.Time{}, fmt.Errorf("no started event found in workflow history")
}

func configDiffFromInput(input workflows.RecordNodePoolConfigChangeInput) []api.NodePoolConfigField {
	if input.PrevNodeSetID == nil {
		return []api.NodePoolConfigField{}
	}

	var fields []api.NodePoolConfigField

	addIfChanged := func(name, from, to string) {
		if from != to {
			f := from
			fields = append(fields, api.NodePoolConfigField{Field: name, From: &f, To: to})
		}
	}

	addJSONIfChanged := func(name string, from, to interface{}) {
		fb, _ := json.Marshal(from) //nolint:errchkjson // callers pass only JSON-safe concrete types
		tb, _ := json.Marshal(to)   //nolint:errchkjson // callers pass only JSON-safe concrete types
		if !bytes.Equal(fb, tb) {
			fs := string(fb)
			fields = append(fields, api.NodePoolConfigField{Field: name, From: &fs, To: string(tb)})
		}
	}

	addIfChanged("osProfile", input.OldOSProfile, input.NewOSProfile)
	addIfChanged("configProfile", input.OldConfigProfile, input.NewConfigProfile)
	addIfChanged("kubernetesVersion", input.OldKubernetesVersion, input.NewKubernetesVersion)
	addIfChanged("systemReservedMemory", input.OldSystemReservedMemory, input.NewSystemReservedMemory)
	addIfChanged("diskSelectionStrategy", input.OldDiskSelectionStrategy, input.NewDiskSelectionStrategy)
	addIfChanged("capiSpireAttestationMethod", input.OldCapiSpireAttestationMethod, input.NewCapiSpireAttestationMethod)
	addJSONIfChanged("teleportEnabled", input.OldTeleportEnabled, input.NewTeleportEnabled)
	addJSONIfChanged("subnetIDs", input.OldSubnetIDs, input.NewSubnetIDs)
	addJSONIfChanged("vpcPrefixIDs", input.OldVpcPrefixIDs, input.NewVpcPrefixIDs)
	addJSONIfChanged("capiPreKubeadmCommands", input.OldCapiPreKubeadmCommands, input.NewCapiPreKubeadmCommands)

	return fields
}

func (s *nodeSetService) configHistoryFromNodeSets(ctx context.Context, nodePool db.NodePool) (api.GetNodePoolConfigHistoryResponseObject, error) {
	nodeSets, err := s.db.ListNodeSets(ctx, db.NodeSetFilters{NodePoolID: &nodePool.ID})
	if err != nil {
		return nil, fmt.Errorf("failed to list node sets: %w", err)
	}

	sort.Slice(nodeSets, func(i, j int) bool {
		return nodeSets[i].CreatedAt.Before(nodeSets[j].CreatedAt)
	})

	changes := make([]api.NodePoolConfigChange, 0, len(nodeSets))
	for i := range nodeSets {
		ns := &nodeSets[i]
		var prev *db.NodeSet
		if i > 0 {
			prev = &nodeSets[i-1]
		}
		isCurrent := nodePool.CurrentNodeSetID != nil && *nodePool.CurrentNodeSetID == ns.ID
		changes = append(changes, api.NodePoolConfigChange{
			NodeSetID:     ns.ID,
			CreatedAt:     ns.CreatedAt,
			IsCurrent:     isCurrent,
			ChangedFields: nodeSetConfigDiff(prev, ns),
		})
	}

	return api.GetNodePoolConfigHistory200JSONResponse{Changes: changes}, nil
}

// nodeSetConfigDiff returns the config fields that changed between prev and curr.
// Returns an empty slice when prev is nil (initial snapshot).
func nodeSetConfigDiff(prev, curr *db.NodeSet) []api.NodePoolConfigField {
	if prev == nil {
		return []api.NodePoolConfigField{}
	}

	var fields []api.NodePoolConfigField

	addIfChanged := func(name, fromVal, toVal string) {
		if fromVal != toVal {
			from := fromVal
			fields = append(fields, api.NodePoolConfigField{Field: name, From: &from, To: toVal})
		}
	}

	addJSONIfChanged := func(name string, fromJSON, toJSON json.RawMessage) {
		if !bytes.Equal(fromJSON, toJSON) {
			from := string(fromJSON)
			fields = append(fields, api.NodePoolConfigField{Field: name, From: &from, To: string(toJSON)})
		}
	}

	marshalField := func(v any) json.RawMessage {
		b, _ := json.Marshal(v) //nolint:errchkjson // callers pass only JSON-safe concrete types
		return b
	}

	addIfChanged("osProfile", prev.OSProfile, curr.OSProfile)
	addIfChanged("configProfile", prev.ConfigProfile, curr.ConfigProfile)
	addIfChanged("kubernetesVersion", prev.KubernetesVersion, curr.KubernetesVersion)
	addIfChanged("systemReservedMemory", prev.SystemReservedMemory, curr.SystemReservedMemory)
	addIfChanged("diskSelectionStrategy", string(prev.DiskSelectionStrategy), string(curr.DiskSelectionStrategy))
	addIfChanged("capiSpireAttestationMethod", string(prev.CapiSpireAttestationMethod), string(curr.CapiSpireAttestationMethod))
	addJSONIfChanged("teleportEnabled", marshalField(prev.TeleportEnabled), marshalField(curr.TeleportEnabled))
	addJSONIfChanged("subnetIDs", marshalField(prev.SubnetIDs), marshalField(curr.SubnetIDs))
	addJSONIfChanged("vpcPrefixIDs", marshalField(prev.VpcPrefixIDs), marshalField(curr.VpcPrefixIDs))
	addJSONIfChanged("capiInfraMachineTemplate", prev.CapiInfraMachineTemplate, curr.CapiInfraMachineTemplate)
	addJSONIfChanged("capiBootstrapFiles", marshalField(prev.CapiBootstrapFiles), marshalField(curr.CapiBootstrapFiles))
	addJSONIfChanged("capiPreKubeadmCommands", marshalField(prev.CapiPreKubeadmCommands), marshalField(curr.CapiPreKubeadmCommands))

	return fields
}
