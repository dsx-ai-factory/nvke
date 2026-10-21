// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// NodeConfig is the NICo-specific node provisioning data resolved from a
// node set's and node pool's NICo provider payloads. Machine materialization
// and bootstrap rendering both need it.
type NodeConfig struct {
	Release                  string
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
// NICo provider payloads. Cluster/NodePool/NodeSet ingress already validates
// these fields at create/update time, so this doesn't re-validate them —
// malformed data is trusted not to reach this layer.
func ResolveNodeConfig(nodeSet db.NodeSet, nodePool db.NodePool) (NodeConfig, error) {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return NodeConfig{}, err
	}
	nicoNodeSetProvider, err := nodeSet.NicoProvider()
	if err != nil {
		return NodeConfig{}, err
	}

	return NodeConfig{
		Release:                  nicoNodeSetProvider.Release,
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
	}, nil
}

// renderInfraMachine builds the NicoMachine the controller constructs for a NICo-backed
// node. ipxeScriptTemplate is the iPXE script of the image the node's release
// boots, which this renders with the node's disk selection strategy.
func (*Client) renderInfraMachine(ipxeScriptTemplate, namespace string, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) (client.Object, error) {
	config, err := ResolveNodeConfig(nodeSet, nodePool)
	if err != nil {
		return nil, err
	}
	nicoProvider, err := cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	ipxeScript, err := renderIpxeScript(ipxeScriptTemplate, string(config.DiskSelectionStrategy))
	if err != nil {
		return nil, fmt.Errorf("failed to render iPXE script for release %s: %w", config.Release, err)
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

// CreateMachine seeds a control-plane instance type if needed, renders and
// applies the NicoMachine, renders and applies the bootstrap secret, and
// applies the generic CAPI Machine pointing at the NicoMachine. There's no
// NICo-equivalent of a SPIRE join token: this path never calls
// deps.CreateSpireJoinToken.
func (c *Client) CreateMachine(ctx context.Context, deps shared.MachineCreateDeps, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) error {
	nodePool, err := deps.SeedControlPlaneInstanceType(ctx, node, nodePool)
	if err != nil {
		return err
	}

	config, err := ResolveNodeConfig(nodeSet, nodePool)
	if err != nil {
		return err
	}
	// Resolve once for both renderers below: two GETs per node instead of four,
	// and the iPXE script and the user data come from the same read.
	resolved, err := resolveRelease(ctx, deps.K8sClient, config.IsControlPlaneRole, config.Release)
	if err != nil {
		return err
	}

	// Image-verification pre-flight: refuse (or warn about) an image with no
	// passing validation verdict before any object is applied. Off by
	// default; see imageverify.go for the modes and the fail-closed rule.
	if err := verifyImage(ctx, deps.Config.ImageVerificationMode, deps.ImageVerifier, resolved.image, node.ID.String(), cluster.ID.String()); err != nil {
		return err
	}

	infraMachineObj, err := c.renderInfraMachine(resolved.image.Spec.NICo.IPXEScript, deps.Namespace, node, nodeSet, nodePool, cluster)
	if err != nil {
		return err
	}
	nicoMachine, ok := infraMachineObj.(*nicov1.NicoMachine)
	if !ok {
		return fmt.Errorf("expected *nicov1.NicoMachine, got %T", infraMachineObj)
	}
	if err := deps.K8sClient.Patch(ctx, nicoMachine, client.Apply, client.FieldOwner(deps.FieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply NicoMachine %s: %w", nicoMachine.Name, err)
	}

	userData, err := c.renderBootstrapUserData(ctx, deps, resolved.userDataTemplate, node, nodeSet, nodePool, cluster)
	if err != nil {
		return err
	}
	bootstrapSecret := shared.BuildBootstrapSecret(node, cluster, deps.Namespace, userData)
	if err := deps.K8sClient.Patch(ctx, bootstrapSecret, client.Apply, client.FieldOwner(deps.FieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply bootstrap secret %s: %w", bootstrapSecret.Name, err)
	}

	machine, err := shared.BuildMachine(node, nodeSet, nodePool, cluster, deps.Namespace, bootstrapSecret.Name, clusterv1.ContractVersionedObjectReference{
		APIGroup: nicov1.GroupVersion.Group,
		Kind:     "NicoMachine",
		Name:     nicoMachine.Name,
	})
	if err != nil {
		return err
	}
	if err := shared.ApplyMachine(ctx, deps.K8sClient, deps.FieldManagerName, machine); err != nil {
		return err
	}

	return nil
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
			Labels:    shared.MachineLabels(cluster.Name, controlPlane),
		},
		Spec: nicov1.NicoMachineSpec{
			VPCID:                    nicoProvider.VpcID.String(),
			InstanceTypeID:           instanceTypeID.String(),
			Interfaces:               interfaces,
			InfinibandPartitionID:    infinibandPartition,
			NVLinkLogicalPartitionID: nvLinkPartition,
			SSHKeyGroupIDs:           uuidStrings(sshKeyGroupIDs),
			IpxeScript:               ipxeScript,
			// Identity labels the controller knows before create. CAPNICo owns machine-id,
			// site-id, site-name, vpc-id, vpc-name, and ip-address.
			Labels: instanceLabels,
		},
	}, nil
}

// BuildInstanceLabels is the subset of instance labels the controller still writes on
// the CAPI path. CAPNICo fills in topology and machine identity after
// create. Exported because the CAPI client applies it too, for the rare case
// where a CAPI-provider cluster's own template happens to render a
// NicoMachine.
func BuildInstanceLabels(node db.Node, cluster db.Cluster, nodePoolID uuid.UUID) map[string]string {
	labels := map[string]string{
		keys.NodeID.String():    node.ID.String(),
		keys.ClusterID.String(): cluster.ID.String(),
	}
	if nodePoolID != uuid.Nil {
		labels[keys.NodePoolID.String()] = nodePoolID.String()
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
