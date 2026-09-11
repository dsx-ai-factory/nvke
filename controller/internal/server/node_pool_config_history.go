// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
				nodePoolIDConst, req.NodePoolID, "error", err)
		case len(changes) > 0:
			return api.GetNodePoolConfigHistory200JSONResponse{Changes: changes}, nil
		default:
			s.logger.Debug("Temporal returned no config history, falling back to NodeSet query",
				nodePoolIDConst, req.NodePoolID)
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
	// A pool carries exactly one payload, so the absent provider reads as the zero
	// value and its fields below compare equal, contributing nothing to the diff.
	oldNico, _ := old.NicoProvider()
	updatedNico, _ := updated.NicoProvider()
	oldCapi, _ := old.CAPIProvider()
	updatedCapi, _ := updated.CAPIProvider()

	// The field names here are a Temporal payload contract — historical workflow
	// inputs are decoded back into this struct by fetchWorkflowInput — so they
	// keep their flat names even though the values now come from the payloads.
	input := workflows.RecordNodePoolConfigChangeInput{
		NodePoolID:    updated.ID,
		NewNodeSetID:  uuid.New(),
		PrevNodeSetID: updated.CurrentNodeSetID,

		OldOSProfile: oldNico.OSProfile,
		NewOSProfile: updatedNico.OSProfile,

		OldConfigProfile: oldNico.ConfigProfile,
		NewConfigProfile: updatedNico.ConfigProfile,

		OldKubernetesVersion: old.KubernetesVersion,
		NewKubernetesVersion: updated.KubernetesVersion,

		OldSystemReservedMemory: old.SystemReservedMemory,
		NewSystemReservedMemory: updated.SystemReservedMemory,

		OldKubeReservedMemory: old.KubeReservedMemory,
		NewKubeReservedMemory: updated.KubeReservedMemory,

		OldDiskSelectionStrategy: string(oldNico.DiskSelectionStrategy),
		NewDiskSelectionStrategy: string(updatedNico.DiskSelectionStrategy),

		OldCapiSpireAttestationMethod: string(oldCapi.SpireAttestationMethod),
		NewCapiSpireAttestationMethod: string(updatedCapi.SpireAttestationMethod),

		OldTeleportEnabled: old.TeleportEnabled,
		NewTeleportEnabled: updated.TeleportEnabled,

		OldSubnetIDs: oldNico.SubnetIDs,
		NewSubnetIDs: updatedNico.SubnetIDs,

		OldVpcPrefixIDs: oldNico.VpcPrefixIDs,
		NewVpcPrefixIDs: updatedNico.VpcPrefixIDs,

		OldCapiPreKubeadmCommands: oldCapi.PreKubeadmCommands,
		NewCapiPreKubeadmCommands: updatedCapi.PreKubeadmCommands,
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
	addIfChanged("kubeReservedMemory", input.OldKubeReservedMemory, input.NewKubeReservedMemory)
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

	// A node set carries exactly one payload, so the absent provider reads as the
	// zero value and its fields below compare equal, adding nothing to the diff.
	prevNico, _ := prev.NicoProvider()
	currNico, _ := curr.NicoProvider()
	prevCapi, _ := prev.CAPIProvider()
	currCapi, _ := curr.CAPIProvider()

	addIfChanged("osProfile", prevNico.OSProfile, currNico.OSProfile)
	addIfChanged("configProfile", prevNico.ConfigProfile, currNico.ConfigProfile)
	addIfChanged("kubernetesVersion", prev.KubernetesVersion, curr.KubernetesVersion)
	addIfChanged("systemReservedMemory", prev.SystemReservedMemory, curr.SystemReservedMemory)
	addIfChanged("kubeReservedMemory", prev.KubeReservedMemory, curr.KubeReservedMemory)
	addIfChanged("diskSelectionStrategy", string(prevNico.DiskSelectionStrategy), string(currNico.DiskSelectionStrategy))
	addIfChanged("capiSpireAttestationMethod", string(prevCapi.SpireAttestationMethod), string(currCapi.SpireAttestationMethod))
	addJSONIfChanged("teleportEnabled", marshalField(prev.TeleportEnabled), marshalField(curr.TeleportEnabled))
	addJSONIfChanged("subnetIDs", marshalField(prevNico.SubnetIDs), marshalField(currNico.SubnetIDs))
	addJSONIfChanged("vpcPrefixIDs", marshalField(prevNico.VpcPrefixIDs), marshalField(currNico.VpcPrefixIDs))
	addJSONIfChanged("capiInfraMachineTemplate", prevCapi.InfraMachineTemplate, currCapi.InfraMachineTemplate)
	addJSONIfChanged("capiBootstrapFiles", marshalField(prevCapi.BootstrapFiles), marshalField(currCapi.BootstrapFiles))
	addJSONIfChanged("capiPreKubeadmCommands", marshalField(prevCapi.PreKubeadmCommands), marshalField(currCapi.PreKubeadmCommands))

	return fields
}
