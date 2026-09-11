// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// GetClusterControlPlane returns a read-only view of a cluster's control plane:
// the persisted control-plane configuration plus an aggregate runtime status.
func (s *clusterService) GetClusterControlPlane(
	ctx context.Context,
	req api.GetClusterControlPlaneRequestObject,
) (api.GetClusterControlPlaneResponseObject, error) {
	dbCluster, err := s.db.GetCluster(ctx, req.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetClusterControlPlane404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	controlPlane := true
	cpNodes, err := s.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &req.ClusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list control-plane nodes: %w", err)
	}

	var readyNodeCount int
	for i := range cpNodes {
		if db.BoolFromConditionStatus(cpNodes[i].Conditions.KubernetesReady.Status) {
			readyNodeCount++
		}
	}

	rolloutByNode, err := controlPlaneRolloutByNode(ctx, s.db, cpNodes)
	if err != nil {
		return nil, err
	}

	status := api.ControlPlaneStatus{
		NodeCount:             len(cpNodes),
		ReadyNodeCount:        readyNodeCount,
		KubernetesVersion:     dbCluster.KubernetesVersion,
		ControlPlaneUpgrading: apiConditionFromDB(dbCluster.Conditions.ControlPlaneUpgrading),
		CpNodesRecycling:      apiConditionFromDB(dbCluster.Conditions.CPNodesRecycling),
		Rollout:               controlPlaneRolloutSummary(dbCluster, cpNodes, rolloutByNode),
	}

	return api.GetClusterControlPlane200JSONResponse(api.ClusterControlPlane{
		Config: *apiControlPlaneConfigFromDB(dbCluster.ControlPlaneConfig),
		Status: status,
	}), nil
}

// controlPlaneRolloutSummary rolls the per-node rollout state up to the cluster
// level. It is nil for node-pool-managed control planes (no control-plane
// controller-managed nodes), matching the per-node behavior where those nodes
// carry no rollout status.
//
// Each managed node lands in exactly one of three mutually exclusive buckets, so
// the counts always sum to managedNodeCount:
//   - pending: the node's pinned configuration has drifted from desired and it
//     still needs recycling (db.IsControlPlaneRolloutRequired is true).
//   - upToDate: the node has converged to the desired configuration and is Ready
//     (kubernetesReady is True). Only these nodes are actually running the
//     desired software.
//   - inProgress: the node already carries the desired configuration but is not
//     yet Ready — a freshly recycled node still coming up. Counting these as up
//     to date would overstate converged capacity mid-rollout.
//
// The inProgress boolean reflects the cluster's CPNodesRecycling condition;
// recyclingEnabled reflects the recycling gate that decides whether pending
// drift is reconciled automatically.
func controlPlaneRolloutSummary(
	cluster db.Cluster,
	cpNodes []db.Node,
	rolloutByNode map[uuid.UUID]*api.ControlPlaneRolloutStatus,
) *api.ControlPlaneRolloutSummary {
	var managed, upToDate, pending, inProgress int
	for i := range cpNodes {
		if !cpNodes[i].IsManagedByControlPlaneController() {
			continue
		}
		managed++
		status, ok := rolloutByNode[cpNodes[i].ID]
		if !ok {
			continue
		}
		switch {
		case status.Pending:
			pending++
		case db.BoolFromConditionStatus(cpNodes[i].Conditions.KubernetesReady.Status):
			upToDate++
		default:
			inProgress++
		}
	}
	if managed == 0 {
		return nil
	}
	return &api.ControlPlaneRolloutSummary{
		ManagedNodeCount:    managed,
		UpToDateNodeCount:   upToDate,
		PendingNodeCount:    pending,
		InProgressNodeCount: inProgress,
		InProgress:          db.BoolFromConditionStatus(cluster.Conditions.CPNodesRecycling.Status),
		RecyclingEnabled:    cluster.EnableCPNodeRecycling,
	}
}

// validateCreateClusterLevelControlPlane validates the cluster-level control
// plane configuration supplied on cluster create when control-plane controller
// provisioning is enabled. It rejects control-plane node pools and applies the
// Forge vs CAPI field-split rules that mirror node pools. The Forge
// subnet/VPC-prefix native-networking rules are validated separately (they
// require a Forge site lookup).
func validateCreateClusterLevelControlPlane(
	req *api.CreateClusterRequest,
	nodePools []api.NodePoolCreateConfig,
	providerType db.ProviderType,
) error {
	for _, np := range nodePools {
		if np.ControlPlane {
			return errControlPlaneNodePoolNotAllowed
		}
	}

	ccpc := req.ControlPlaneConfig
	if ccpc == nil {
		return fmt.Errorf("controlPlaneConfig is required when control-plane controller provisioning is enabled")
	}
	if ccpc.DesiredCount < 1 {
		return fmt.Errorf("controlPlaneConfig.desiredCount must be at least 1")
	}
	if ccpc.MaxSurge != nil && *ccpc.MaxSurge > 1 {
		return errClusterMaxSurgeTooHigh
	}

	switch providerType {
	case db.ProviderTypeCAPI:
		return validateCreateCAPIControlPlaneConfig(ccpc)
	case db.ProviderTypeNico:
		return validateCreateForgeControlPlaneConfigFields(ccpc)
	default:
		return fmt.Errorf("unsupported cluster provider")
	}
}

// validateCreateCAPIControlPlaneConfig enforces CAPI control-plane config rules:
// infra.machineTemplate is required and Forge-only fields are rejected.
func validateCreateCAPIControlPlaneConfig(c *api.CreateClusterControlPlaneConfig) error {
	if c.Nico != nil {
		return fmt.Errorf("controlPlaneConfig.nico is not supported, please provide controlPlaneConfig.infra")
	}
	if c.Infra == nil || isEmptyManifestObject(c.Infra.MachineTemplate) {
		return fmt.Errorf("controlPlaneConfig.infra.machineTemplate is required")
	}
	if err := validateCAPIInfrastructureManifest(c.Infra.MachineTemplate); err != nil {
		return fmt.Errorf("invalid controlPlaneConfig.infra.machineTemplate: %w", err)
	}
	if err := validateCAPISpireAttestationMethod(string(c.Infra.SpireAttestationMethod)); err != nil {
		return err
	}
	return validateCloudInitFiles(c.Infra.BootstrapFiles)
}

// validateCreateForgeControlPlaneConfigFields enforces the non-networking Forge
// control-plane config rules: CAPI-only fields are rejected and the required
// Forge fields are present.
func validateCreateForgeControlPlaneConfigFields(c *api.CreateClusterControlPlaneConfig) error {
	if c.Infra != nil {
		return fmt.Errorf("controlPlaneConfig.infra is not supported, please provide controlPlaneConfig.nico")
	}
	if c.Nico == nil {
		return fmt.Errorf("controlPlaneConfig.nico is required for a nico cluster control plane")
	}
	if len(c.Nico.InstanceTypePreference) == 0 {
		return fmt.Errorf("controlPlaneConfig.nico.instanceTypePreference must contain at least one instance type ID for a nico cluster control plane")
	}
	if err := validateInstanceTypePreferenceIDs(c.Nico.InstanceTypePreference); err != nil {
		return err
	}
	if c.Nico.OSProfile == "" {
		return fmt.Errorf("controlPlaneConfig.nico.osProfile is required for a nico cluster control plane")
	}
	if c.Nico.ConfigProfile == "" {
		return fmt.Errorf("controlPlaneConfig.nico.configProfile is required for a nico cluster control plane")
	}
	return nil
}

// validateMergedClusterControlPlaneSpec enforces the create-time control-plane
// invariants over the merged spec of a PATCH, so an update cannot clear a required
// field or persist a ControlPlaneConfigSpec that would be rejected on create.
func validateMergedClusterControlPlaneSpec(cluster db.Cluster, spec db.ControlPlaneConfigSpec) error {
	switch cluster.ProviderType {
	case db.ProviderTypeNico:
		return validateMergedForgeControlPlaneSpec(spec)
	case db.ProviderTypeCAPI:
		return validateMergedCAPIControlPlaneSpec(spec)
	default:
		return fmt.Errorf("unsupported cluster provider")
	}
}

func validateMergedForgeControlPlaneSpec(spec db.ControlPlaneConfigSpec) error {
	if spec.Nico == nil {
		return newBadRequestErrorf("controlPlaneConfig.nico is required for a nico cluster control plane")
	}
	if spec.Capi != nil {
		return newBadRequestErrorf("controlPlaneConfig.infra is not supported, please provide controlPlaneConfig.nico")
	}
	if len(spec.Nico.InstanceTypePreference) == 0 {
		return newBadRequestErrorf("controlPlaneConfig.nico.instanceTypePreference must contain at least one instance type ID for a nico cluster control plane")
	}
	if spec.Nico.OSProfile == "" {
		return newBadRequestErrorf("controlPlaneConfig.nico.osProfile is required for a nico cluster control plane")
	}
	if spec.Nico.ConfigProfile == "" {
		return newBadRequestErrorf("controlPlaneConfig.nico.configProfile is required for a nico cluster control plane")
	}
	return nil
}

func validateMergedCAPIControlPlaneSpec(spec db.ControlPlaneConfigSpec) error {
	if spec.Capi == nil {
		return newBadRequestErrorf("controlPlaneConfig.infra is required")
	}
	if spec.Nico != nil {
		return newBadRequestErrorf("controlPlaneConfig.nico is not supported, please provide controlPlaneConfig.infra")
	}
	if firstInfraMachineTemplateObject(spec.Capi.InfraMachineTemplatePreferences) == nil {
		return newBadRequestErrorf("controlPlaneConfig.infra.machineTemplate is required")
	}
	var bootstrapFiles []api.CloudInitFile
	if files := apiCloudInitFilesFromDB(spec.Capi.BootstrapFiles); files != nil {
		bootstrapFiles = *files
	}
	if err := validateCloudInitFiles(bootstrapFiles); err != nil {
		return newBadRequestErrorf("%s", err)
	}
	return nil
}

func validateControlPlaneNicoNetworking(subnetIDs, vpcPrefixIDs []uuid.UUID) error {
	if err := validateNicoNetworkSelection(subnetIDs, vpcPrefixIDs, "controlPlaneConfig.nico."); err != nil {
		return newBadRequestErrorf("%s", err)
	}
	return nil
}

// controlPlaneConfigSpecFromCreateAPI builds the persisted control-plane config
// snapshot spec from a create request's controlPlaneConfig.
func controlPlaneConfigSpecFromCreateAPI(
	c *api.CreateClusterControlPlaneConfig,
	kubernetesVersion string,
	teleportDefault bool,
	providerType db.ProviderType,
) db.ControlPlaneConfigSpec {
	teleportEnabled := teleportDefault
	if c.TeleportEnabled != nil {
		teleportEnabled = *c.TeleportEnabled
	}

	provisioningMonitorDisabled := c.ProvisioningMonitorDisabled != nil && *c.ProvisioningMonitorDisabled

	spec := db.ControlPlaneConfigSpec{
		SystemReservedMemory:        c.SystemReservedMemory,
		NodeLabels:                  append([]string(nil), c.NodeLabels...),
		TeleportEnabled:             teleportEnabled,
		ProvisioningMonitorDisabled: provisioningMonitorDisabled,
		KubernetesVersion:           kubernetesVersion,
	}

	switch providerType {
	case db.ProviderTypeNico:
		nicoControlPlaneProvider := db.NicoControlPlaneProvider{
			DiskSelectionStrategy: db.DiskSelectionStrategy(api.DiskSelectionStrategyCreateSmallest),
		}
		if c.Nico != nil {
			nicoControlPlaneProvider.InstanceTypePreference = dedupeUUIDsPreserveOrder(uuidSliceFromAPI(c.Nico.InstanceTypePreference))
			nicoControlPlaneProvider.SubnetIDs = uuidSliceDedupe(uuidSliceFromAPI(c.Nico.SubnetIDs))
			nicoControlPlaneProvider.VpcPrefixIDs = uuidSliceDedupe(uuidSliceFromAPI(c.Nico.VpcPrefixIDs))
			nicoControlPlaneProvider.OSProfile = c.Nico.OSProfile
			nicoControlPlaneProvider.ConfigProfile = c.Nico.ConfigProfile
			nicoControlPlaneProvider.SSHKeyGroupIDs = uuidSliceDedupe(uuidSliceFromAPI(c.Nico.SSHKeyGroupIDs))
		}
		spec.SetNico(nicoControlPlaneProvider)
		return spec
	case db.ProviderTypeCAPI:
		capiControlPlaneProvider := db.CAPIControlPlaneProvider{}
		if c.Infra != nil {
			capiControlPlaneProvider.SpireAttestationMethod = db.SpireAttestationMethod(c.Infra.SpireAttestationMethod)
			capiControlPlaneProvider.BootstrapFiles = dbCloudInitFilesFromAPI(c.Infra.BootstrapFiles)
			capiControlPlaneProvider.PreKubeadmCommands = append([]string(nil), c.Infra.PreKubeadmCommands...)
			if prefs, err := infraMachineTemplatePrefsFromAPI(c.Infra.MachineTemplate); err == nil {
				capiControlPlaneProvider.InfraMachineTemplatePreferences = prefs
			}
		}
		spec.SetCapi(capiControlPlaneProvider)
		return spec
	default:
		return db.ControlPlaneConfigSpec{}
	}
}

func apiNicoControlPlaneProviderFromDB(nicoControlPlaneProvider *db.NicoControlPlaneProvider) *api.NicoControlPlaneProvider {
	if nicoControlPlaneProvider == nil {
		return nil
	}
	out := &api.NicoControlPlaneProvider{
		InstanceTypePreference: append([]openapi_types.UUID(nil), nicoControlPlaneProvider.InstanceTypePreference...),
		SubnetIDs:              append([]openapi_types.UUID(nil), nicoControlPlaneProvider.SubnetIDs...),
		VpcPrefixIDs:           append([]openapi_types.UUID(nil), nicoControlPlaneProvider.VpcPrefixIDs...),
		OSProfile:              nicoControlPlaneProvider.OSProfile,
		ConfigProfile:          nicoControlPlaneProvider.ConfigProfile,
		SSHKeyGroupIDs:         append([]openapi_types.UUID(nil), nicoControlPlaneProvider.SSHKeyGroupIDs...),
	}
	if nicoControlPlaneProvider.DiskSelectionStrategy != "" {
		disk := api.DiskSelectionStrategyCreate(nicoControlPlaneProvider.DiskSelectionStrategy)
		out.DiskSelectionStrategy = &disk
	}
	return out
}

func apiInfraControlPlaneProviderFromDB(capiControlPlaneProvider *db.CAPIControlPlaneProvider) *api.InfraControlPlaneProvider {
	if capiControlPlaneProvider == nil {
		return nil
	}

	out := &api.InfraControlPlaneProvider{
		MachineTemplate:        firstInfraMachineTemplateObject(capiControlPlaneProvider.InfraMachineTemplatePreferences),
		SpireAttestationMethod: api.SpireAttestationMethodCreate(capiControlPlaneProvider.SpireAttestationMethod),
		PreKubeadmCommands:     append([]string(nil), capiControlPlaneProvider.PreKubeadmCommands...),
	}
	if files := apiCloudInitFilesFromDB(capiControlPlaneProvider.BootstrapFiles); files != nil {
		out.BootstrapFiles = *files
	}
	return out
}
