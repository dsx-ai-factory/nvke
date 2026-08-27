// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/teleport"
)

type templateLessForgeCAPINodeConfig struct {
	osProfile                string
	diskSelectionStrategy    db.DiskSelectionStrategy
	kubernetesVersion        string
	teleportEnabled          bool
	instanceTypeID           uuid.UUID
	sshKeyGroupIDs           []uuid.UUID
	subnetIDs                []uuid.UUID
	vpcPrefixIDs             []uuid.UUID
	nodePoolID               uuid.UUID
	isControlPlaneRole       bool
	infinibandPartitionID    *uuid.UUID
	nvLinkLogicalPartitionID *uuid.UUID
}

func (a *Activities) buildNicoMachineForNode(createCtx *capiMachineCreateContext) (*nicov1.NicoMachine, error) {
	provisioningConfig, err := resolveTemplateLessForgeCAPINodeConfig(createCtx)
	if err != nil {
		return nil, err
	}
	nicoProvider, err := createCtx.cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	osProfileDir := path.Join(a.cfg.ProfilesDir, "osProfiles", provisioningConfig.osProfile)
	ipxeScript, err := getIpxeScript(osProfileDir, string(provisioningConfig.diskSelectionStrategy))
	if err != nil {
		return nil, fmt.Errorf("failed to get iPXE script for OS profile %s: %w", provisioningConfig.osProfile, err)
	}

	instanceLabels := buildNicoInstanceLabels(createCtx.node, createCtx.cluster, provisioningConfig.nodePoolID)

	nicoMachine, err := buildNicoMachine(
		createCtx.node,
		createCtx.cluster,
		nicoProvider,
		createCtx.namespace,
		createCtx.nodePool.ControlPlane,
		instanceLabels,
		provisioningConfig.instanceTypeID,
		provisioningConfig.sshKeyGroupIDs,
		provisioningConfig.subnetIDs,
		provisioningConfig.vpcPrefixIDs,
		provisioningConfig.infinibandPartitionID,
		provisioningConfig.nvLinkLogicalPartitionID,
		ipxeScript,
	)
	if err != nil {
		return nil, err
	}

	return nicoMachine, nil
}

func (a *Activities) renderTemplateLessForgeCAPIBootstrapUserData(ctx context.Context, createCtx *capiMachineCreateContext) (string, error) {
	provisioningConfig, err := resolveTemplateLessForgeCAPINodeConfig(createCtx)
	if err != nil {
		return "", err
	}

	osProfileDir := path.Join(a.cfg.ProfilesDir, "osProfiles", provisioningConfig.osProfile)
	teleportCfg, err := a.nicoMachineTeleportConfig(createCtx, provisioningConfig.teleportEnabled, provisioningConfig.nodePoolID, provisioningConfig.isControlPlaneRole)
	if err != nil {
		return "", err
	}
	userData, err := a.getUserData(ctx, osProfileDir, createCtx.node, provisioningConfig.diskSelectionStrategy, provisioningConfig.kubernetesVersion, teleportCfg, createCtx.cluster)
	if err != nil {
		return "", fmt.Errorf("failed to get user data for OS profile %s: %w", provisioningConfig.osProfile, err)
	}

	return userData, nil
}

func resolveTemplateLessForgeCAPINodeConfig(createCtx *capiMachineCreateContext) (templateLessForgeCAPINodeConfig, error) {
	return resolveTemplateLessForgeNodeConfig(createCtx.nodeSet, createCtx.nodePool)
}

func resolveTemplateLessForgeNodeConfig(nodeSet db.NodeSet, nodePool db.NodePool) (templateLessForgeCAPINodeConfig, error) {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return templateLessForgeCAPINodeConfig{}, err
	}
	nicoNodeSetProvider, err := nodeSet.NicoProvider()
	if err != nil {
		return templateLessForgeCAPINodeConfig{}, err
	}
	if err := validateResolvedTemplateLessForgeNodeContext(nodeSet, nicoNodeSetProvider, nicoNodePoolProvider); err != nil {
		return templateLessForgeCAPINodeConfig{}, err
	}

	provisioningConfig := templateLessForgeCAPINodeConfig{
		osProfile:                nicoNodeSetProvider.OSProfile,
		diskSelectionStrategy:    nicoNodeSetProvider.DiskSelectionStrategy,
		kubernetesVersion:        nodeSet.KubernetesVersion,
		teleportEnabled:          nodeSet.TeleportEnabled,
		instanceTypeID:           nicoNodePoolProvider.InstanceTypeID,
		sshKeyGroupIDs:           nicoNodePoolProvider.SSHKeyGroupIDs,
		subnetIDs:                nicoNodePoolProvider.SubnetIDs,
		vpcPrefixIDs:             nicoNodePoolProvider.VpcPrefixIDs,
		nodePoolID:               nodePool.ID,
		isControlPlaneRole:       nodePool.ControlPlane,
		infinibandPartitionID:    nicoNodePoolProvider.InfinibandPartitionID,
		nvLinkLogicalPartitionID: nicoNodePoolProvider.NvLinkLogicalPartitionID,
	}

	if provisioningConfig.osProfile == "" {
		return templateLessForgeCAPINodeConfig{}, fmt.Errorf("os profile is empty")
	}
	if provisioningConfig.instanceTypeID == uuid.Nil {
		return templateLessForgeCAPINodeConfig{}, fmt.Errorf("instance type is empty")
	}
	if len(provisioningConfig.subnetIDs) == 0 && len(provisioningConfig.vpcPrefixIDs) == 0 {
		return templateLessForgeCAPINodeConfig{}, fmt.Errorf("both subnetIDs and vpcPrefixIDs are empty")
	}

	return provisioningConfig, nil
}

func validateResolvedTemplateLessForgeNodeContext(nodeSet db.NodeSet, nicoNodeSetProvider, nicoProvider db.NicoNodePoolProvider) error {
	missingFields := make([]string, 0, 4)

	if nicoNodeSetProvider.OSProfile == "" {
		missingFields = append(missingFields, "nodeSet.osProfile")
	}
	if nodeSet.KubernetesVersion == "" {
		missingFields = append(missingFields, "nodeSet.kubernetesVersion")
	}
	if nicoProvider.InstanceTypeID == uuid.Nil {
		missingFields = append(missingFields, "nodePool.instanceTypeID")
	}
	if len(nicoProvider.SubnetIDs) == 0 && len(nicoProvider.VpcPrefixIDs) == 0 {
		missingFields = append(missingFields, "nodePool.subnetIDs|vpcPrefixIDs")
	}

	if len(missingFields) > 0 {
		return fmt.Errorf("resolved node context is incomplete: missing %s", strings.Join(missingFields, ", "))
	}

	return nil
}

func (a *Activities) nicoMachineTeleportConfig(createCtx *capiMachineCreateContext, teleportEnabled bool, nodePoolID uuid.UUID, isControlPlaneRole bool) (*TeleportUserDataConfig, error) {
	if !a.cfg.Teleport.Enabled || !createCtx.cluster.TeleportEnabled || !teleportEnabled {
		return &TeleportUserDataConfig{}, nil
	}
	nicoProvider, err := createCtx.cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	return &TeleportUserDataConfig{
		Enabled:      true,
		TokenName:    teleport.TPMTokenName(createCtx.node.ID),
		ProxyAddress: a.cfg.Teleport.ProxyAddress,
		JoinMethod:   string(teleport.JoinMethodTPM),
		Labels:       buildTeleportLabels(createCtx.node, createCtx.cluster, nicoProvider, nodePoolID, isControlPlaneRole, a.cfg.NKESiteControllerName, a.cfg.Teleport.Environment),
	}, nil
}

func buildNicoMachine(
	node db.Node,
	cluster db.Cluster,
	nicoProvider db.NicoClusterProvider,
	namespace string,
	controlPlane bool,
	instanceLabels map[string]string,
	instanceTypeID uuid.UUID,
	sshKeyGroupIDs []uuid.UUID,
	subnetIDs []uuid.UUID,
	vpcPrefixIDs []uuid.UUID,
	infinibandPartitionID *uuid.UUID,
	nvLinkLogicalPartitionID *uuid.UUID,
	ipxeScript string,
) (*nicov1.NicoMachine, error) {
	interfaces, err := buildNicoMachineInterfaces(subnetIDs, vpcPrefixIDs)
	if err != nil {
		return nil, err
	}

	infinibandPartition := ""
	if infinibandPartitionID != nil {
		infinibandPartition = infinibandPartitionID.String()
	}
	nvLinkPartition := ""
	if nvLinkLogicalPartitionID != nil {
		nvLinkPartition = nvLinkLogicalPartitionID.String()
	}

	labels := capiMachineLabels(cluster.Name, controlPlane)
	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       nicoMachineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: nicov1.NicoMachineSpec{
			VPCID:                    nicoProvider.VpcID.String(),
			InstanceTypeID:           instanceTypeID.String(),
			Interfaces:               interfaces,
			InfinibandPartitionID:    infinibandPartition,
			NVLinkLogicalPartitionID: nvLinkPartition,
			SSHKeyGroupIDs:           uuidStrings(sshKeyGroupIDs),
			IpxeScript:               ipxeScript,
			// Identity labels CWE knows before create. CAPNICo owns machine-id,
			// site-id, site-name, vpc-id, vpc-name, and ip-address.
			Labels: instanceLabels,
		},
	}, nil
}

// buildNicoInstanceLabels is the subset of instance labels CWE still writes on
// the CAPI path. CAPNICo fills in topology and machine identity after create.
func buildNicoInstanceLabels(node db.Node, cluster db.Cluster, nodePoolID uuid.UUID) map[string]string {
	labels := map[string]string{
		constants.LabelKeyNodeID:    node.ID.String(),
		constants.LabelKeyClusterID: cluster.ID.String(),
	}
	if nodePoolID != uuid.Nil {
		labels[constants.LabelKeyNodePoolID] = nodePoolID.String()
	}
	return labels
}

func buildNicoMachineInterfaces(subnetIDs, vpcPrefixIDs []uuid.UUID) ([]nicov1.NicoMachineInterface, error) {
	if len(vpcPrefixIDs) > 0 {
		return []nicov1.NicoMachineInterface{{VPCPrefixID: vpcPrefixIDs[0].String()}}, nil
	}
	if len(subnetIDs) > 0 {
		return []nicov1.NicoMachineInterface{{SubnetID: subnetIDs[0].String()}}, nil
	}

	return nil, fmt.Errorf("both subnetIDs and vpcPrefixIDs are empty")
}

func uuidStrings(ids []uuid.UUID) []string {
	strings := make([]string, 0, len(ids))
	for _, id := range ids {
		strings = append(strings, id.String())
	}
	return strings
}
