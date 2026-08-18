// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func (a *Activities) shouldRotateNicoMachineNetworkCandidate(ctx context.Context, machine *clusterv1.Machine) (bool, error) {
	if machine.Spec.InfrastructureRef.APIGroup != nicov1.GroupVersion.Group || machine.Spec.InfrastructureRef.Kind != nicoMachineConst {
		return false, nil
	}

	nicoMachine := &nicov1.NicoMachine{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		return false, fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}
	if len(nicoMachine.Spec.Interfaces) == 0 {
		return false, nil
	}

	readyCondition := nicoMachineReadyCondition(nicoMachine.Status.Conditions)
	if readyCondition == nil ||
		readyCondition.Status != metav1.ConditionFalse ||
		readyCondition.Reason != nicov1.InstanceCreateFailedReason {
		return false, nil
	}
	if readyCondition.ObservedGeneration != 0 && readyCondition.ObservedGeneration < nicoMachine.Generation {
		return false, nil
	}
	if !isNicoNetworkExhaustionMessage(readyCondition.Message) {
		return false, nil
	}

	return true, nil
}

func (a *Activities) rotateNicoMachineNetworkCandidate(ctx context.Context, nodeSet db.NodeSet, machine *clusterv1.Machine) error {
	nicoMachine := &nicov1.NicoMachine{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		return fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}
	if len(nicoMachine.Spec.Interfaces) == 0 {
		return nil
	}

	nicoNodeSetProvider, err := nodeSet.NicoProvider()
	if err != nil {
		return err
	}

	currentInterface := nicoMachine.Spec.Interfaces[0]
	// Template-backed CAPI owns its network config inside the infra template; CWE only
	// rotates candidates when the template-less Forge-CAPI path supplied them.
	if currentInterface.VPCPrefixID != "" {
		if len(nicoNodeSetProvider.VpcPrefixIDs) == 0 {
			return nil
		}
		return a.rotateNicoMachineVpcPrefix(ctx, nicoMachine, nicoNodeSetProvider.VpcPrefixIDs)
	}
	if currentInterface.SubnetID != "" {
		if len(nicoNodeSetProvider.SubnetIDs) == 0 {
			return nil
		}
		return a.rotateNicoMachineSubnet(ctx, nicoMachine, nicoNodeSetProvider.SubnetIDs)
	}

	return nil
}

func (a *Activities) rotateNicoMachineVpcPrefix(ctx context.Context, nicoMachine *nicov1.NicoMachine, vpcPrefixIDs []uuid.UUID) error {
	currentVpcPrefixID := nicoMachine.Spec.Interfaces[0].VPCPrefixID
	currentIndex := -1
	for i, vpcPrefixID := range vpcPrefixIDs {
		if vpcPrefixID.String() == currentVpcPrefixID {
			currentIndex = i
			break
		}
	}
	if currentIndex == -1 {
		return fmt.Errorf("current NicoMachine VPC prefix %s is not in configured VPC prefix candidates", currentVpcPrefixID)
	}
	if currentIndex == len(vpcPrefixIDs)-1 {
		failureMessage := ""
		if readyCondition := nicoMachineReadyCondition(nicoMachine.Status.Conditions); readyCondition != nil {
			failureMessage = readyCondition.Message
		}
		return fmt.Errorf("all %d VPC prefixes are exhausted, error from the last NicoMachine create attempt: %s", len(vpcPrefixIDs), failureMessage)
	}
	nextVpcPrefixID := vpcPrefixIDs[currentIndex+1].String()

	patchOpts := client.MergeFromWithOptions(nicoMachine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	nicoMachine.Spec.Interfaces[0].VPCPrefixID = nextVpcPrefixID
	nicoMachine.Spec.Interfaces[0].SubnetID = ""
	if err := a.k8sClient.Patch(ctx, nicoMachine, patchOpts); err != nil {
		return fmt.Errorf("failed to rotate NicoMachine VPC prefix from %s to %s: %w", currentVpcPrefixID, nextVpcPrefixID, err)
	}

	return nil
}

func (a *Activities) rotateNicoMachineSubnet(ctx context.Context, nicoMachine *nicov1.NicoMachine, subnetIDs []uuid.UUID) error {
	currentSubnetID := nicoMachine.Spec.Interfaces[0].SubnetID
	currentIndex := -1
	for i, subnetID := range subnetIDs {
		if subnetID.String() == currentSubnetID {
			currentIndex = i
			break
		}
	}
	if currentIndex == -1 {
		return fmt.Errorf("current NicoMachine subnet %s is not in configured subnet candidates", currentSubnetID)
	}
	if currentIndex == len(subnetIDs)-1 {
		failureMessage := ""
		if readyCondition := nicoMachineReadyCondition(nicoMachine.Status.Conditions); readyCondition != nil {
			failureMessage = readyCondition.Message
		}
		return fmt.Errorf("all %d subnets are exhausted, error from the last NicoMachine create attempt: %s", len(subnetIDs), failureMessage)
	}
	nextSubnetID := subnetIDs[currentIndex+1].String()

	patchOpts := client.MergeFromWithOptions(nicoMachine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	nicoMachine.Spec.Interfaces[0].SubnetID = nextSubnetID
	nicoMachine.Spec.Interfaces[0].VPCPrefixID = ""
	if err := a.k8sClient.Patch(ctx, nicoMachine, patchOpts); err != nil {
		return fmt.Errorf("failed to rotate NicoMachine subnet from %s to %s: %w", currentSubnetID, nextSubnetID, err)
	}

	return nil
}

// shouldRotateNicoMachineInstanceType reports whether a template-less Forge-CAPI
// NicoMachine failed because its selected instance type has no capacity or does
// not exist, in which case CWE should rotate to the next instance type in the
// node's pinned control plane preference list. Nico surfaces this in the
// NicoMachine Ready condition via a preflight capacity check it runs before
// creating any backing instance, so rotation is an in-place Spec.InstanceTypeID
// patch with nothing to tear down.
func (a *Activities) shouldRotateNicoMachineInstanceType(ctx context.Context, machine *clusterv1.Machine) (bool, error) {
	if machine.Spec.InfrastructureRef.APIGroup != nicov1.GroupVersion.Group || machine.Spec.InfrastructureRef.Kind != nicoMachineConst {
		return false, nil
	}

	nicoMachine := &nicov1.NicoMachine{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		return false, fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}

	// Only template-less machines carry a CWE-selected instance type to rotate;
	// template-backed machines get their instance type from the infra template.
	if nicoMachine.Spec.InstanceTypeID == "" {
		return false, nil
	}

	readyCondition := nicoMachineReadyCondition(nicoMachine.Status.Conditions)
	if readyCondition == nil ||
		readyCondition.Status != metav1.ConditionFalse ||
		!isNicoInstanceTypeUnavailableReason(readyCondition.Reason) {
		return false, nil
	}
	if readyCondition.ObservedGeneration != 0 && readyCondition.ObservedGeneration < nicoMachine.Generation {
		return false, nil
	}

	return true, nil
}

// isNicoInstanceTypeUnavailableReason reports whether a NicoMachine Ready
// condition reason means the requested instance type cannot be satisfied.
// AvailabilityCheckFailedReason is intentionally excluded: it signals a transient
// failure of the availability check itself (e.g. a NICo API error), not that the
// type is unavailable, so the same type should be retried rather than rotated.
func isNicoInstanceTypeUnavailableReason(reason string) bool {
	return reason == nicov1.InstanceTypeUnavailableReason || reason == nicov1.InstanceTypeNotFoundReason
}

// rotateNicoMachineInstanceType advances a NicoMachine to the next instance type
// in the node's pinned preference list. The walk is cyclic (wraps after the last
// entry) and the caller re-probes every poll, so it converges on the
// highest-preference type once that type has capacity rather than sticking on a
// lower-preference fallback. CWE only patches the NicoMachine spec; Nico owns
// availability (its preflight surfaces the Ready condition reacted to here), so
// the walk reads the live spec and needs no node-side bookkeeping.
func (a *Activities) rotateNicoMachineInstanceType(ctx context.Context, node db.Node, machine *clusterv1.Machine) error {
	preference, err := a.controlPlaneInstanceTypePreference(ctx, node)
	if err != nil {
		return err
	}
	if len(preference) == 0 {
		return nil
	}

	nicoMachine := &nicov1.NicoMachine{}
	if err := a.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		return fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}
	currentInstanceTypeID := nicoMachine.Spec.InstanceTypeID
	if currentInstanceTypeID == "" {
		return nil
	}

	nextInstanceTypeID, err := capi.NextInPreference(preference, currentInstanceTypeID)
	switch {
	case errors.Is(err, capi.ErrPreferenceCurrentNotFound):
		return fmt.Errorf("current NicoMachine instance type %s is not in configured instance type preference", currentInstanceTypeID)
	case err != nil:
		return err
	}
	if nextInstanceTypeID.String() == currentInstanceTypeID {
		// Single-entry preference (or wrap-around to the same entry): nothing to
		// patch; keep retrying the same type in place until Nico gets capacity.
		return nil
	}

	patchOpts := client.MergeFromWithOptions(nicoMachine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	nicoMachine.Spec.InstanceTypeID = nextInstanceTypeID.String()
	if err := a.k8sClient.Patch(ctx, nicoMachine, patchOpts); err != nil {
		return fmt.Errorf("failed to rotate NicoMachine instance type from %s to %s: %w", currentInstanceTypeID, nextInstanceTypeID, err)
	}

	return nil
}

// controlPlaneInstanceTypePreference returns the ordered instance type preference
// the node is pinned to. Only control plane nodes provisioned from a control plane
// configuration snapshot carry one; everything else returns an empty list so the
// caller treats rotation as a no-op.
func (a *Activities) controlPlaneInstanceTypePreference(ctx context.Context, node db.Node) ([]uuid.UUID, error) {
	if !node.ControlPlane || node.ControlPlaneConfigurationID == uuid.Nil {
		return nil, nil
	}
	row, err := a.db.GetControlPlaneConfiguration(ctx, node.ControlPlaneConfigurationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get control plane configuration %s: %w", node.ControlPlaneConfigurationID, err)
	}
	nico := row.Config.NicoControlPlaneProvider()
	if nico == nil {
		return nil, fmt.Errorf("nico control-plane provider payload is required")
	}
	return nico.InstanceTypePreference, nil
}

// seedControlPlaneInstanceType fills the initial instance type for a templateless
// control-plane NicoMachine when none is resolved yet. On the Nico path CWE does
// not pre-fill/persist a type onto the node, so this seeds the first pinned
// preference; Nico then owns availability and rotateNicoMachineInstanceType walks
// the rest in place. No-op for nodes that already carry a type or that carry no
// control-plane preference.
func (a *Activities) seedControlPlaneInstanceType(ctx context.Context, createCtx *capiMachineCreateContext) error {
	nicoNodePoolProvider, err := createCtx.nodePool.NicoProvider()
	if err != nil {
		return err
	}
	if nicoNodePoolProvider.InstanceTypeID != uuid.Nil {
		return nil
	}
	preference, err := a.controlPlaneInstanceTypePreference(ctx, createCtx.node)
	if err != nil {
		return err
	}
	if len(preference) == 0 {
		return nil
	}
	nicoNodePoolProvider.InstanceTypeID = preference[0]
	createCtx.nodePool.SetNico(nicoNodePoolProvider)
	return nil
}

func nicoMachineReadyCondition(conditions []metav1.Condition) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == clusterv1.ReadyCondition {
			return &conditions[i]
		}
	}
	return nil
}

func isNicoNetworkExhaustionMessage(message string) bool {
	normalized := strings.ToLower(message)
	// CAPNICo appends NICo's raw error body into the NicoMachine Ready
	// condition. The substrings below come from infra-controller's instance
	// create path: REST preflight emits "Subnet/VPC Prefix ... does not have
	// enough IP addresses", and allocator failures can bubble through the
	// creation workflow as "Prefix: ... has exhausted all address space" or
	// "IP address exhausted: ...". NICo can also point at Forge directly in
	// some environments, which uses the same allocation substrings.
	// https://github.com/NVIDIA/infra-controller/blob/fdb5d34320bdce562bbc18eb958065a047ea8dd6/rest-api/api/pkg/api/handler/instance.go#L540-L629
	// https://github.com/NVIDIA/infra-controller/blob/fdb5d34320bdce562bbc18eb958065a047ea8dd6/crates/api-db/src/ip_allocator.rs#L60
	// https://github.com/NVIDIA/nke/controller/-/blob/dffb36a8/internal/providers/forge/api/errors.go#L57-L72

	nicoPreflightExhausted := strings.Contains(normalized, "does not have enough ip addresses:") &&
		(strings.Contains(normalized, "subnet ") || strings.Contains(normalized, "vpc prefix "))

	prefixExhausted := strings.Contains(normalized, "prefix:") &&
		strings.Contains(normalized, "has exhausted all address space")

	ipAddressExhausted := strings.Contains(normalized, "ip address exhausted:")

	return nicoPreflightExhausted || prefixExhausted || ipAddressExhausted
}
