// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// GetClusterControlPlaneConfigHistory returns the control-plane configuration
// change history for a cluster, derived from its append-only control-plane
// configuration snapshots. Each entry shows which rollout-relevant fields changed
// relative to the previous snapshot, so a node's pinnedConfigurationID and the
// cluster's desired configurationID can be resolved to a human-readable change.
func (s *clusterService) GetClusterControlPlaneConfigHistory(
	ctx context.Context,
	req api.GetClusterControlPlaneConfigHistoryRequestObject,
) (api.GetClusterControlPlaneConfigHistoryResponseObject, error) {
	cluster, err := s.db.GetCluster(ctx, req.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetClusterControlPlaneConfigHistory404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	snapshots, err := s.db.ListControlPlaneConfigurationsByCluster(ctx, req.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("failed to list control-plane configurations: %w", err)
	}

	currentID := cluster.ControlPlaneConfig.ConfigurationID
	changes := make([]api.ControlPlaneConfigChange, 0, len(snapshots))
	for i := range snapshots {
		snap := &snapshots[i]
		var prev *db.ControlPlaneConfigSpec
		if i > 0 {
			prev = &snapshots[i-1].Config
		}
		changes = append(changes, api.ControlPlaneConfigChange{
			ConfigurationID: snap.ID,
			CreatedAt:       snap.CreatedAt,
			IsCurrent:       snap.ID == currentID,
			ChangeReason:    snap.ChangeReason,
			CreatedBy:       snap.CreatedBy,
			ChangedFields:   controlPlaneConfigDiff(prev, &snap.Config),
		})
	}

	return api.GetClusterControlPlaneConfigHistory200JSONResponse{Changes: changes}, nil
}

// controlPlaneConfigDiff returns the config fields that changed between prev and
// curr. It reports the rollout-relevant fields (OS profile, config profile,
// Kubernetes version) plus the flat scheduling/behavior knobs, and deliberately
// omits provider blobs (bootstrap files, pre-kubeadm commands, machine
// templates) so the history stays a safe, readable summary. Returns an empty
// slice when prev is nil (the initial snapshot).
func controlPlaneConfigDiff(prev, curr *db.ControlPlaneConfigSpec) []api.ControlPlaneConfigField {
	// Always a non-nil slice so changedFields serializes as [] rather than null:
	// it is a required array field, and the initial snapshot and unchanged
	// snapshots must be indistinguishable in shape from changed ones.
	fields := []api.ControlPlaneConfigField{}
	if prev == nil {
		return fields
	}

	addIfChanged := func(name, fromVal, toVal string) {
		if fromVal != toVal {
			from := fromVal
			fields = append(fields, api.ControlPlaneConfigField{Field: name, From: &from, To: toVal})
		}
	}

	addIfChanged("osProfile", prev.OSProfile, curr.OSProfile)
	addIfChanged("configProfile", prev.ConfigProfile, curr.ConfigProfile)
	addIfChanged("kubernetesVersion", prev.KubernetesVersion, curr.KubernetesVersion)
	addIfChanged("systemReservedMemory", prev.SystemReservedMemory, curr.SystemReservedMemory)
	addIfChanged("kubeReservedMemory", prev.KubeReservedMemory, curr.KubeReservedMemory)
	addIfChanged("teleportEnabled", strconv.FormatBool(prev.TeleportEnabled), strconv.FormatBool(curr.TeleportEnabled))
	addIfChanged("provisioningMonitorDisabled", strconv.FormatBool(prev.ProvisioningMonitorDisabled), strconv.FormatBool(curr.ProvisioningMonitorDisabled))

	return fields
}
