// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"strings"
	"text/template"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// NodeConfig is the NICo-specific node provisioning data resolved from a
// node set's and node pool's NICo provider payloads. Machine materialization
// and bootstrap rendering both need it.
type NodeConfig struct {
	OSProfile                string
	DiskSelectionStrategy    db.DiskSelectionStrategy
	KubernetesVersion        string
	TeleportEnabled          bool
	InstanceTypeID           uuid.UUID
	SSHKeyGroupIDs           []uuid.UUID
	SubnetIDs                []uuid.UUID
	VpcPrefixIDs             []uuid.UUID
	NodePoolID               uuid.UUID
	IsControlPlaneRole       bool
	InfinibandPartitionID    *uuid.UUID
	NVLinkLogicalPartitionID *uuid.UUID
}

// ResolveNodeConfig resolves NodeConfig from the node set's and node pool's
// NICo provider payloads, validating that every field is present.
func ResolveNodeConfig(nodeSet db.NodeSet, nodePool db.NodePool) (NodeConfig, error) {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return NodeConfig{}, err
	}
	nicoNodeSetProvider, err := nodeSet.NicoProvider()
	if err != nil {
		return NodeConfig{}, err
	}
	if err := validateResolvedNodeConfig(nodeSet, nicoNodeSetProvider, nicoNodePoolProvider); err != nil {
		return NodeConfig{}, err
	}

	config := NodeConfig{
		OSProfile:                nicoNodeSetProvider.OSProfile,
		DiskSelectionStrategy:    nicoNodeSetProvider.DiskSelectionStrategy,
		KubernetesVersion:        nodeSet.KubernetesVersion,
		TeleportEnabled:          nodeSet.TeleportEnabled,
		InstanceTypeID:           nicoNodePoolProvider.InstanceTypeID,
		SSHKeyGroupIDs:           nicoNodePoolProvider.SSHKeyGroupIDs,
		SubnetIDs:                nicoNodePoolProvider.SubnetIDs,
		VpcPrefixIDs:             nicoNodePoolProvider.VpcPrefixIDs,
		NodePoolID:               nodePool.ID,
		IsControlPlaneRole:       nodePool.ControlPlane,
		InfinibandPartitionID:    nicoNodePoolProvider.InfinibandPartitionID,
		NVLinkLogicalPartitionID: nicoNodePoolProvider.NvLinkLogicalPartitionID,
	}

	if config.OSProfile == "" {
		return NodeConfig{}, fmt.Errorf("os profile is empty")
	}
	if config.InstanceTypeID == uuid.Nil {
		return NodeConfig{}, fmt.Errorf("instance type is empty")
	}
	if len(config.SubnetIDs) == 0 && len(config.VpcPrefixIDs) == 0 {
		return NodeConfig{}, fmt.Errorf("both subnetIDs and vpcPrefixIDs are empty")
	}

	return config, nil
}

func validateResolvedNodeConfig(nodeSet db.NodeSet, nicoNodeSetProvider, nicoProvider db.NicoNodePoolProvider) error {
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

// RenderInfraMachine builds the NicoMachine CWE constructs for a NICo-backed
// node. profilesDir is the OS/config profiles directory on disk, used to
// read the OS profile's iPXE script.
func (*Client) RenderInfraMachine(profilesDir, namespace string, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) (client.Object, error) {
	config, err := ResolveNodeConfig(nodeSet, nodePool)
	if err != nil {
		return nil, err
	}
	nicoProvider, err := cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	osProfileDir := path.Join(profilesDir, "osProfiles", config.OSProfile)
	ipxeScript, err := getIpxeScript(osProfileDir, string(config.DiskSelectionStrategy))
	if err != nil {
		return nil, fmt.Errorf("failed to get iPXE script for OS profile %s: %w", config.OSProfile, err)
	}

	instanceLabels := BuildInstanceLabels(node, cluster, config.NodePoolID)

	return buildMachine(
		node,
		cluster,
		nicoProvider,
		namespace,
		nodePool.ControlPlane,
		instanceLabels,
		config.InstanceTypeID,
		config.SSHKeyGroupIDs,
		config.SubnetIDs,
		config.VpcPrefixIDs,
		config.InfinibandPartitionID,
		config.NVLinkLogicalPartitionID,
		ipxeScript,
	)
}

func buildMachine(
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
	interfaces, err := buildMachineInterfaces(subnetIDs, vpcPrefixIDs)
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

	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: namespace,
			Labels:    machineLabels(cluster.Name, controlPlane),
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

func machineLabels(clusterName string, controlPlane bool) map[string]string {
	labels := map[string]string{
		clusterv1.ClusterNameLabel: clusterName,
	}
	if controlPlane {
		labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	return labels
}

// BuildInstanceLabels is the subset of instance labels CWE still writes on
// the CAPI path. CAPNICo fills in topology and machine identity after
// create. Exported because the CAPI client applies it too, for the rare case
// where a CAPI-provider cluster's own template happens to render a
// NicoMachine.
func BuildInstanceLabels(node db.Node, cluster db.Cluster, nodePoolID uuid.UUID) map[string]string {
	labels := map[string]string{
		constants.LabelKeyNodeID:    node.ID.String(),
		constants.LabelKeyClusterID: cluster.ID.String(),
	}
	if nodePoolID != uuid.Nil {
		labels[constants.LabelKeyNodePoolID] = nodePoolID.String()
	}
	return labels
}

func buildMachineInterfaces(subnetIDs, vpcPrefixIDs []uuid.UUID) ([]nicov1.NicoMachineInterface, error) {
	if len(vpcPrefixIDs) > 0 {
		return []nicov1.NicoMachineInterface{{VPCPrefixID: vpcPrefixIDs[0].String()}}, nil
	}
	if len(subnetIDs) > 0 {
		return []nicov1.NicoMachineInterface{{SubnetID: subnetIDs[0].String()}}, nil
	}

	return nil, fmt.Errorf("both subnetIDs and vpcPrefixIDs are empty")
}

func uuidStrings(ids []uuid.UUID) []string {
	strs := make([]string, 0, len(ids))
	for _, id := range ids {
		strs = append(strs, id.String())
	}
	return strs
}

type ipxeData struct {
	DiskSelectionStrategy string
}

func getIpxeScript(osProfileDir, diskSelectionStrategy string) (string, error) {
	ipxeScriptBytes, err := os.ReadFile(path.Join(osProfileDir, "ipxeScript"))
	if err != nil {
		return "", fmt.Errorf("failed to read ipxeScript file: %w", err)
	}

	return renderIpxeScript(string(ipxeScriptBytes), diskSelectionStrategy)
}

func renderIpxeScript(ipxeScriptTemplate, diskSelectionStrategy string) (string, error) {
	tmpl, err := template.New("ipxeScript").Parse(ipxeScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse ipxeScript template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ipxeData{DiskSelectionStrategy: diskSelectionStrategy}); err != nil {
		return "", fmt.Errorf("failed to execute ipxeScript template: %w", err)
	}

	return buf.String(), nil
}
