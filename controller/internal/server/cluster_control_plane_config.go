// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func unsupportedControlPlaneConfigFieldError(fieldName, clusterType string) error {
	return fmt.Errorf("controlPlaneConfig.%s is not supported for %s", fieldName, clusterType)
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
	isCAPICluster bool,
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

	if isCAPICluster {
		return validateCreateCAPIControlPlaneConfig(ccpc)
	}
	return validateCreateForgeControlPlaneConfigFields(ccpc)
}

// validateCreateCAPIControlPlaneConfig enforces CAPI control-plane config rules:
// capiInfraMachineTemplate is required and Forge-only fields are rejected.
func validateCreateCAPIControlPlaneConfig(c *api.CreateClusterControlPlaneConfig) error {
	switch {
	case len(c.InstanceTypePreference) > 0:
		return unsupportedControlPlaneConfigFieldError("instanceTypePreference", "CAPI clusters")
	case c.OSProfile != "":
		return unsupportedControlPlaneConfigFieldError("osProfile", "CAPI clusters")
	case len(c.SubnetIDs) > 0:
		return unsupportedControlPlaneConfigFieldError("subnetIDs", "CAPI clusters")
	case len(c.VpcPrefixIDs) > 0:
		return unsupportedControlPlaneConfigFieldError("vpcPrefixIDs", "CAPI clusters")
	case len(c.SSHKeyGroupIDs) > 0:
		return unsupportedControlPlaneConfigFieldError("sshKeyGroupIDs", "CAPI clusters")
	}
	if len(c.CapiInfraMachineTemplate) == 0 || isEmptyManifestObject(c.CapiInfraMachineTemplate) {
		return fmt.Errorf("controlPlaneConfig.capiInfraMachineTemplate is required for CAPI clusters")
	}
	if err := validateCAPIInfrastructureManifest(c.CapiInfraMachineTemplate); err != nil {
		return fmt.Errorf("invalid controlPlaneConfig.capiInfraMachineTemplate: %w", err)
	}
	if err := validateCAPISpireAttestationMethod(string(c.CapiSpireAttestationMethod)); err != nil {
		return err
	}
	return validateCloudInitFiles(c.CapiBootstrapFiles)
}

// validateCreateForgeControlPlaneConfigFields enforces the non-networking Forge
// control-plane config rules: CAPI-only fields are rejected and the required
// Forge fields are present.
func validateCreateForgeControlPlaneConfigFields(c *api.CreateClusterControlPlaneConfig) error {
	switch {
	case len(c.CapiInfraMachineTemplate) > 0:
		return unsupportedControlPlaneConfigFieldError("capiInfraMachineTemplate", "Forge clusters")
	case c.CapiSpireAttestationMethod != "":
		return unsupportedControlPlaneConfigFieldError("capiSpireAttestationMethod", "Forge clusters")
	case len(c.CapiBootstrapFiles) > 0:
		return unsupportedControlPlaneConfigFieldError("capiBootstrapFiles", "Forge clusters")
	case len(c.CapiPreKubeadmCommands) > 0:
		return unsupportedControlPlaneConfigFieldError("capiPreKubeadmCommands", "Forge clusters")
	}
	if len(c.InstanceTypePreference) == 0 {
		return fmt.Errorf("controlPlaneConfig.instanceTypePreference must contain at least one instance type ID for a Forge cluster control plane")
	}
	if err := validateInstanceTypePreferenceIDs(c.InstanceTypePreference); err != nil {
		return err
	}
	if c.OSProfile == "" {
		return fmt.Errorf("controlPlaneConfig.osProfile is required for a Forge cluster control plane")
	}
	if c.ConfigProfile == "" {
		return fmt.Errorf("controlPlaneConfig.configProfile is required for a Forge cluster control plane")
	}
	return nil
}

// validateForgeControlPlaneSpecFields rejects CAPI-only fields on the merged
// control-plane spec of a Forge cluster during an update.
func validateForgeControlPlaneSpecFields(spec db.ControlPlaneConfigSpec) error {
	switch {
	case len(spec.InfraMachineTemplatePreferences) > 0:
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("capiInfraMachineTemplate", "Forge clusters"))
	case spec.CapiSpireAttestationMethod != "":
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("capiSpireAttestationMethod", "Forge clusters"))
	case len(spec.CapiBootstrapFiles) > 0:
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("capiBootstrapFiles", "Forge clusters"))
	case len(spec.CapiPreKubeadmCommands) > 0:
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("capiPreKubeadmCommands", "Forge clusters"))
	}
	return nil
}

// validateCAPIControlPlaneSpecFields rejects Forge-only fields on the merged
// control-plane spec of a CAPI cluster during an update.
func validateCAPIControlPlaneSpecFields(spec db.ControlPlaneConfigSpec) error {
	switch {
	case len(spec.InstanceTypePreference) > 0:
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("instanceTypePreference", "CAPI clusters"))
	case spec.OSProfile != "":
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("osProfile", "CAPI clusters"))
	case len(spec.SubnetIDs) > 0:
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("subnetIDs", "CAPI clusters"))
	case len(spec.VpcPrefixIDs) > 0:
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("vpcPrefixIDs", "CAPI clusters"))
	case len(spec.SSHKeyGroupIDs) > 0:
		return newBadRequestErrorf("%s", unsupportedControlPlaneConfigFieldError("sshKeyGroupIDs", "CAPI clusters"))
	}
	return nil
}

// validateMergedClusterControlPlaneSpec enforces the create-time control-plane
// invariants over the merged spec of a PATCH, so an update cannot clear a required
// field or persist a ControlPlaneConfigSpec that would be rejected on create.
func validateMergedClusterControlPlaneSpec(cluster db.Cluster, spec db.ControlPlaneConfigSpec) error {
	isForge := cluster.SiteID != uuid.Nil
	if isForge {
		if err := validateForgeControlPlaneSpecFields(spec); err != nil {
			return err
		}
		if len(spec.InstanceTypePreference) == 0 {
			return newBadRequestErrorf("controlPlaneConfig.instanceTypePreference must contain at least one instance type ID for a Forge cluster control plane")
		}
		if spec.OSProfile == "" {
			return newBadRequestErrorf("controlPlaneConfig.osProfile is required for a Forge cluster control plane")
		}
		if spec.ConfigProfile == "" {
			return newBadRequestErrorf("controlPlaneConfig.configProfile is required for a Forge cluster control plane")
		}
		return nil
	}

	if err := validateCAPIControlPlaneSpecFields(spec); err != nil {
		return err
	}
	if len(spec.InfraMachineTemplatePreferences) == 0 {
		return newBadRequestErrorf("controlPlaneConfig.capiInfraMachineTemplate is required for CAPI clusters")
	}
	var bootstrapFiles []api.CloudInitFile
	if files := apiCloudInitFilesFromDB(spec.CapiBootstrapFiles); files != nil {
		bootstrapFiles = *files
	}
	if err := validateCloudInitFiles(bootstrapFiles); err != nil {
		return newBadRequestErrorf("%s", err)
	}
	return nil
}

// validateControlPlaneForgeNetworking enforces the same subnet/VPC-prefix
// native-networking rules used for node pools, for the cluster-level control
// plane on a Forge cluster.
func (s *clusterService) validateControlPlaneForgeNetworking(
	ctx context.Context,
	orgID string,
	siteID uuid.UUID,
	subnetIDs []uuid.UUID,
	vpcPrefixIDs []uuid.UUID,
) error {
	forgeSite, err := s.nodePoolService.forgeProvider.GetSiteByID(ctx, orgID, siteID.String())
	if err != nil {
		return fmt.Errorf("failed to get forge site: %w", err)
	}
	capabilities := forgeSite.GetCapabilities()
	isNativeNetworkingEnabled := capabilities.GetNativeNetworking()

	if err := validateForgeNativeNetworking(isNativeNetworkingEnabled, subnetIDs, vpcPrefixIDs, "controlPlaneConfig."); err != nil {
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
	isForge bool,
) db.ControlPlaneConfigSpec {
	// Control plane nodes always use the smallest disk; the strategy is not
	// configurable through the API. CAPI control planes ignore disk selection.
	var diskStrategy db.DiskSelectionStrategy
	if isForge {
		diskStrategy = db.DiskSelectionStrategy(api.DiskSelectionStrategyCreateSmallest)
	}

	teleportEnabled := teleportDefault
	if c.TeleportEnabled != nil {
		teleportEnabled = *c.TeleportEnabled
	}

	provisioningMonitorDisabled := c.ProvisioningMonitorDisabled != nil && *c.ProvisioningMonitorDisabled

	spec := db.ControlPlaneConfigSpec{
		InstanceTypePreference:      dedupeUUIDsPreserveOrder(uuidSliceFromAPI(c.InstanceTypePreference)),
		SubnetIDs:                   uuidSliceDedupe(uuidSliceFromAPI(c.SubnetIDs)),
		VpcPrefixIDs:                uuidSliceDedupe(uuidSliceFromAPI(c.VpcPrefixIDs)),
		OSProfile:                   c.OSProfile,
		DiskSelectionStrategy:       diskStrategy,
		ConfigProfile:               c.ConfigProfile,
		SystemReservedMemory:        c.SystemReservedMemory,
		SSHKeyGroupIDs:              uuidSliceDedupe(uuidSliceFromAPI(c.SSHKeyGroupIDs)),
		NodeLabels:                  append([]string(nil), c.NodeLabels...),
		TeleportEnabled:             teleportEnabled,
		ProvisioningMonitorDisabled: provisioningMonitorDisabled,
		KubernetesVersion:           kubernetesVersion,
		CapiSpireAttestationMethod:  db.SpireAttestationMethod(c.CapiSpireAttestationMethod),
		CapiBootstrapFiles:          dbCloudInitFilesFromAPI(c.CapiBootstrapFiles),
		CapiPreKubeadmCommands:      append([]string(nil), c.CapiPreKubeadmCommands...),
	}
	if len(c.CapiInfraMachineTemplate) > 0 {
		if raw, err := rawManifestFromObject(c.CapiInfraMachineTemplate); err == nil && len(raw) > 0 {
			spec.InfraMachineTemplatePreferences = []json.RawMessage{raw}
		}
	}
	return spec
}
