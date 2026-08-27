// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"maps"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"
	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// Everything in this file serves the one-way adoption of Forge clusters that
// predate CAPI. It is called by ForgeCAPIMigrationController, not by any
// workflow, and is deleted together with that controller once the fleet
// reports no clusters left outside nke-system.

// LookupForgeInstanceForAdoption loads the live Forge instance for a node that
// is about to be adopted. Call this outside the cluster-row transaction so a
// slow Forge/cache miss does not hold the teardown lock.
func (a *Activities) LookupForgeInstanceForAdoption(ctx context.Context, cluster db.Cluster, node db.Node) (*forgesdk.Instance, error) {
	nicoProvider, err := node.NicoProvider()
	if err != nil {
		return nil, err
	}
	if nicoProvider.ForgeInstanceID == "" {
		return nil, fmt.Errorf("node %s has no Forge instance ID", node.ID)
	}
	clusterProvider, err := cluster.NicoProvider()
	if err != nil {
		return nil, err
	}

	instance, err := a.forgeProvider.GetInstanceByID(
		ctx,
		clusterProvider.OrgID,
		clusterProvider.SiteID.String(),
		nicoProvider.ForgeInstanceID,
	)
	if err != nil {
		return nil, fmt.Errorf("get Forge instance %s for adoption: %w", nicoProvider.ForgeInstanceID, err)
	}
	return instance, nil
}

// ApplyAdoptedForgeCAPIMachine always creates paused CAPI objects for an
// existing Forge-backed node without provisioning another instance. instance
// must already have been loaded with LookupForgeInstanceForAdoption.
func (a *Activities) ApplyAdoptedForgeCAPIMachine(
	ctx context.Context,
	cluster db.Cluster,
	node db.Node,
	nodeSet db.NodeSet,
	nodePool db.NodePool,
	instance *forgesdk.Instance,
) error {
	nicoProvider, err := node.NicoProvider()
	if err != nil {
		return err
	}
	if nicoProvider.ForgeInstanceID == "" {
		return fmt.Errorf("node %s has no Forge instance ID", node.ID)
	}

	createCtx := &capiMachineCreateContext{
		node:      node,
		nodeSet:   nodeSet,
		nodePool:  nodePool,
		cluster:   cluster,
		namespace: constants.NamespaceNKESystem,
	}

	nicoMachine, err := buildAdoptedNicoMachineForNode(node, cluster, constants.NamespaceNKESystem, nodePool.ControlPlane, instance)
	if err != nil {
		return err
	}

	// Critical - Setting Spec.ProviderID (with a cleared out status.InstanceID) is what allows CAPNICo to find the Machine and "adopt" it.
	// And NicoMachine should be adopted paused.
	nicoMachine.Spec.ProviderID = "nico://" + instance.GetId()
	capi.SetPausedAnnotation(&nicoMachine.ObjectMeta, true)

	// Bootstrap data is best effort for adoption: the Forge instance is already
	// running and CAPI will not boot it from this Secret, but CAPI still expects
	// the Machine to reference one. Rendering can fail for old nodes whose
	// deprecated OS profiles are no longer packaged.
	userData, err := a.renderTemplateLessForgeCAPIBootstrapUserData(ctx, createCtx)
	if err != nil {
		userData = fmt.Sprintf("#cloud-config\n# Bootstrap data rendering failed while adopting an existing Forge instance into CAPI: %s\n", err)
	}
	bootstrapSecret := buildCAPIBootstrapSecret(createCtx, userData)

	machine, err := buildCAPIMachineForNode(node, nodeSet, nodePool, cluster, constants.NamespaceNKESystem, bootstrapSecret.Name, clusterv1.ContractVersionedObjectReference{
		APIGroup: nicov1.GroupVersion.Group,
		Kind:     nicoMachineConst,
		Name:     nicoMachine.Name,
	})
	if err != nil {
		return err
	}
	capi.SetPausedAnnotation(&machine.ObjectMeta, true)

	// NicoMachine then Secret then Machine: Machine admission Gets
	// infrastructureRef and bootstrap DataSecretName.
	if err := a.k8sClient.Patch(ctx, nicoMachine, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply adopted NicoMachine %s: %w", nicoMachine.Name, err)
	}
	if err := a.k8sClient.Patch(ctx, bootstrapSecret, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply bootstrap secret %s: %w", bootstrapSecret.Name, err)
	}
	if err := a.k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply adopted Machine %s: %w", machine.Name, err)
	}

	return nil
}

// Do not route adoption through buildNicoMachineForNode and then patch a few
// fields. The greenfield builder describes the VM NKE would create from today's
// node pool and node set config. Adoption has the inverse contract: CAPNICo
// imports an already-existing VM only if NicoMachine.spec describes the live
// Forge instance. Legacy Forge-direct nodes can differ from the current config:
// labels are overwritten by WaitForForgeInstanceReady, node pools may have
// changed instance type or SSH keys, iPXE profiles may have drifted, and the
// successful subnet or VPC prefix may not be the first current candidate.
//
// Projecting the spec from the observed instance keeps CAPNICo's import
// validation focused on attaching to the VM that already exists instead of
// recreating the old create request from current NKE config.
func buildAdoptedNicoMachineForNode(
	node db.Node,
	cluster db.Cluster,
	namespace string,
	controlPlane bool,
	instance *forgesdk.Instance,
) (*nicov1.NicoMachine, error) {
	if instance == nil {
		return nil, fmt.Errorf("forge instance is nil")
	}

	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       nicoMachineConst,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: namespace,
			Labels:    capiMachineLabels(cluster.Name, controlPlane),
		},
		Spec: nicov1.NicoMachineSpec{
			VPCID:                instance.GetVpcId(),
			InstanceTypeID:       instance.GetInstanceTypeId(),
			IpxeScript:           instance.GetIpxeScript(),
			Labels:               maps.Clone(instance.GetLabels()),
			SSHKeyGroupIDs:       append([]string(nil), instance.GetSshKeyGroupIds()...),
			Interfaces:           buildAdoptedNicoMachineInterfaces(instance.GetInterfaces()),
			InfinibandInterfaces: buildAdoptedNicoMachineInfiniBandInterfaces(instance.GetInfinibandInterfaces()),
			NVLinkInterfaces:     buildAdoptedNicoMachineNVLinkInterfaces(instance.GetNvLinkInterfaces()),
		},
	}, nil
}

func buildAdoptedNicoMachineInterfaces(instanceInterfaces []forgesdk.Interface) []nicov1.NicoMachineInterface {
	if len(instanceInterfaces) == 0 {
		return nil
	}

	interfaces := make([]nicov1.NicoMachineInterface, 0, len(instanceInterfaces))
	for _, instanceInterface := range instanceInterfaces {
		nicoInterface := nicov1.NicoMachineInterface{
			SubnetID:    instanceInterface.GetSubnetId(),
			VPCPrefixID: instanceInterface.GetVpcPrefixId(),
			IPAddress:   instanceInterface.GetRequestedIpAddress(),
			Device:      instanceInterface.GetDevice(),
		}
		if physical, ok := instanceInterface.GetIsPhysicalOk(); ok && physical != nil {
			nicoInterface.Physical = ptr.To(*physical)
		}
		if deviceInstance, ok := instanceInterface.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			nicoInterface.DeviceInstance = ptr.To(*deviceInstance)
		}
		interfaces = append(interfaces, nicoInterface)
	}
	return interfaces
}

func buildAdoptedNicoMachineInfiniBandInterfaces(instanceInterfaces []forgesdk.InfiniBandInterface) []nicov1.NicoMachineInfiniBandInterface {
	if len(instanceInterfaces) == 0 {
		return nil
	}

	interfaces := make([]nicov1.NicoMachineInfiniBandInterface, 0, len(instanceInterfaces))
	for _, instanceInterface := range instanceInterfaces {
		nicoInterface := nicov1.NicoMachineInfiniBandInterface{
			PartitionID: instanceInterface.GetPartitionId(),
			Device:      instanceInterface.GetDevice(),
		}
		if deviceInstance, ok := instanceInterface.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			nicoInterface.DeviceInstance = ptr.To(*deviceInstance)
		}
		if isPhysical, ok := instanceInterface.GetIsPhysicalOk(); ok && isPhysical != nil {
			nicoInterface.IsPhysical = ptr.To(*isPhysical)
		}
		if virtualFunctionID, ok := instanceInterface.GetVirtualFunctionIdOk(); ok && virtualFunctionID != nil {
			nicoInterface.VirtualFunctionID = ptr.To(*virtualFunctionID)
		}
		interfaces = append(interfaces, nicoInterface)
	}
	return interfaces
}

func buildAdoptedNicoMachineNVLinkInterfaces(instanceInterfaces []forgesdk.NVLinkInterface) []nicov1.NicoMachineNVLinkInterface {
	if len(instanceInterfaces) == 0 {
		return nil
	}

	interfaces := make([]nicov1.NicoMachineNVLinkInterface, 0, len(instanceInterfaces))
	for _, instanceInterface := range instanceInterfaces {
		nicoInterface := nicov1.NicoMachineNVLinkInterface{
			NVLinkLogicalPartitionID: instanceInterface.GetNvLinkLogicalPartitionId(),
		}
		if deviceInstance, ok := instanceInterface.GetDeviceInstanceOk(); ok && deviceInstance != nil {
			nicoInterface.DeviceInstance = ptr.To(*deviceInstance)
		}
		interfaces = append(interfaces, nicoInterface)
	}
	return interfaces
}
