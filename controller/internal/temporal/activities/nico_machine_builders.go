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
	"k8s.io/utils/ptr"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/kube"
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

func (a *Activities) buildNicoMachineForNode(ctx context.Context, createCtx *capiMachineCreateContext) (*nicov1.NicoMachine, error) {
	provisioningConfig, err := resolveTemplateLessForgeCAPINodeConfig(createCtx)
	if err != nil {
		return nil, err
	}
	nicoProvider, err := createCtx.cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	var instanceType GetInstanceTypeOutput
	if provisioningConfig.infinibandPartitionID != nil || provisioningConfig.nvLinkLogicalPartitionID != nil {
		instanceType, err = a.getInstanceType(ctx, nicoProvider.OrgID, nicoProvider.SiteID.String(), provisioningConfig.instanceTypeID)
		if err != nil {
			return nil, fmt.Errorf("failed to get instance type: %w", err)
		}
	}

	infinibandInterfaces, err := buildNicoMachineInfinibandInterfaces(provisioningConfig.instanceTypeID, provisioningConfig.infinibandPartitionID, instanceType)
	if err != nil {
		return nil, err
	}
	nvLinkInterfaces, err := buildNicoMachineNVLinkInterfaces(provisioningConfig.instanceTypeID, provisioningConfig.nvLinkLogicalPartitionID, instanceType)
	if err != nil {
		return nil, err
	}

	osProfileDir := path.Join(a.cfg.ProfilesDir, "osProfiles", provisioningConfig.osProfile)
	ipxeScript, err := getIpxeScript(osProfileDir, string(provisioningConfig.diskSelectionStrategy))
	if err != nil {
		return nil, fmt.Errorf("failed to get iPXE script for OS profile %s: %w", provisioningConfig.osProfile, err)
	}

	// CAPNICo forwards NicoMachine.spec.labels to the Forge instance create
	// payload, so resolve the VPC name here when the Forge cache is available.
	vpcName := ""
	if a.forgeProvider != nil {
		vpcName = a.resolveVPCName(ctx, nicoProvider.OrgID, nicoProvider.SiteID.String(), nicoProvider.VpcID.String())
	}

	instanceLabels, err := buildNicoInstanceLabels(createCtx.node, createCtx.cluster, nicoProvider, provisioningConfig.nodePoolID, a.cfg.NKESiteControllerName, vpcName)
	if err != nil {
		return nil, err
	}

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
		infinibandInterfaces,
		nvLinkInterfaces,
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
	infinibandInterfaces []nicov1.NicoMachineInfiniBandInterface,
	nvLinkInterfaces []nicov1.NicoMachineNVLinkInterface,
	ipxeScript string,
) (*nicov1.NicoMachine, error) {
	interfaces, err := buildNicoMachineInterfaces(subnetIDs, vpcPrefixIDs)
	if err != nil {
		return nil, err
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
			VPCID:                nicoProvider.VpcID.String(),
			InstanceTypeID:       instanceTypeID.String(),
			Interfaces:           interfaces,
			InfinibandInterfaces: infinibandInterfaces,
			NVLinkInterfaces:     nvLinkInterfaces,
			SSHKeyGroupIDs:       uuidStrings(sshKeyGroupIDs),
			IpxeScript:           ipxeScript,
			// These labels are part of the VM payload contract for CAPNICo-based machines.
			Labels: instanceLabels,
		},
	}, nil
}

// buildNicoInstanceLabels builds the labels CWE can know before CAPNICo creates
// the backing VM. CAPNICo adds the machine-id later because Forge only returns
// that value after instance creation.
func buildNicoInstanceLabels(node db.Node, cluster db.Cluster, nicoProvider db.NicoClusterProvider, nodePoolID uuid.UUID, siteName, vpcName string) (map[string]string, error) {
	nicoNodeProvider, err := node.NicoProvider()
	if err != nil {
		return nil, err
	}

	labels := map[string]string{
		constants.LabelKeyNodeID:     node.ID.String(),
		constants.LabelKeyClusterID:  cluster.ID.String(),
		constants.LabelKeyNodeSiteID: nicoProvider.SiteID.String(),
		constants.LabelKeyNodeVPCID:  nicoProvider.VpcID.String(),
	}
	if nodePoolID != uuid.Nil {
		labels[constants.LabelKeyNodePoolID] = nodePoolID.String()
	}
	if nicoNodeProvider.ForgeMachineID != "" {
		labels[constants.LabelKeyMachineID] = nicoNodeProvider.ForgeMachineID
	}
	if name := kube.NormalizeLabelValue(siteName); name != "" {
		labels[constants.LabelKeyNodeSiteName] = name
	}
	if name := kube.NormalizeLabelValue(vpcName); name != "" {
		labels[constants.LabelKeyNodeVPCName] = name
	}
	return labels, nil
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

func buildNicoMachineInfinibandInterfaces(instanceTypeID uuid.UUID, infinibandPartitionID *uuid.UUID, instanceType GetInstanceTypeOutput) ([]nicov1.NicoMachineInfiniBandInterface, error) {
	if infinibandPartitionID == nil {
		return nil, nil
	}
	if instanceType.InfiniBandDeviceName == "" {
		return nil, fmt.Errorf("instance type %s does not support InfiniBand", instanceTypeID)
	}
	if len(instanceType.InfiniBandActiveDeviceIDs) == 0 {
		return nil, fmt.Errorf("instance type %s does not support InfiniBand", instanceTypeID)
	}

	nicoInterfaces := make([]nicov1.NicoMachineInfiniBandInterface, 0, len(instanceType.InfiniBandActiveDeviceIDs))
	for _, deviceID := range instanceType.InfiniBandActiveDeviceIDs {
		nicoInterfaces = append(nicoInterfaces, nicov1.NicoMachineInfiniBandInterface{
			PartitionID:    infinibandPartitionID.String(),
			Device:         instanceType.InfiniBandDeviceName,
			DeviceInstance: ptr.To(deviceID),
			IsPhysical:     ptr.To(true),
		})
	}
	return nicoInterfaces, nil
}

func buildNicoMachineNVLinkInterfaces(instanceTypeID uuid.UUID, nvLinkLogicalPartitionID *uuid.UUID, instanceType GetInstanceTypeOutput) ([]nicov1.NicoMachineNVLinkInterface, error) {
	if nvLinkLogicalPartitionID == nil {
		return nil, nil
	}
	if instanceType.NVLinkInterfaceCount == 0 {
		return nil, fmt.Errorf("instance type %s does not support NVLink Interface", instanceTypeID)
	}

	activeDeviceIDs := getNVLinkActiveDeviceIDs(instanceType)
	nicoInterfaces := make([]nicov1.NicoMachineNVLinkInterface, 0, len(activeDeviceIDs))
	for _, deviceID := range activeDeviceIDs {
		nicoInterfaces = append(nicoInterfaces, nicov1.NicoMachineNVLinkInterface{
			NVLinkLogicalPartitionID: nvLinkLogicalPartitionID.String(),
			DeviceInstance:           ptr.To(deviceID),
		})
	}
	return nicoInterfaces, nil
}

func uuidStrings(ids []uuid.UUID) []string {
	strings := make([]string, 0, len(ids))
	for _, id := range ids {
		strings = append(strings, id.String())
	}
	return strings
}
