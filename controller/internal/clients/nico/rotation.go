// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// TryAdvanceStuckMachine advances a templateless NicoMachine in place: it
// tries the network-candidate rotation first, then the instance-type
// rotation. The instance-type walk is cyclic (rotateNicoMachineInstanceType),
// so a node whose preferred types are all unavailable keeps probing them in
// preference order and converges on the highest-preference type once
// capacity returns.
func (*Client) TryAdvanceStuckMachine(ctx context.Context, deps shared.MachineAdvanceDeps, node db.Node, nodeSet db.NodeSet, machine *clusterv1.Machine) (bool, error) {
	shouldRotateNet, err := shouldRotateNicoMachineNetworkCandidate(ctx, deps.K8sClient, machine)
	if err != nil {
		return false, err
	}
	if shouldRotateNet {
		if err := rotateNicoMachineNetworkCandidate(ctx, deps.K8sClient, nodeSet, machine); err != nil {
			return false, err
		}
		return true, nil
	}

	shouldRotateType, err := shouldRotateNicoMachineInstanceType(ctx, deps.K8sClient, machine)
	if err != nil {
		return false, err
	}
	if shouldRotateType {
		advanced, err := rotateNicoMachineInstanceType(ctx, deps, node, machine)
		if err != nil {
			return false, err
		}
		return advanced, nil
	}

	return false, nil
}

func shouldRotateNicoMachineNetworkCandidate(ctx context.Context, k8sClient client.Client, machine *clusterv1.Machine) (bool, error) {
	nicoMachine := &nicov1.NicoMachine{}
	if err := k8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		return false, fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}
	if len(nicoMachine.Spec.Interfaces) == 0 {
		return false, nil
	}

	provisionedCondition := nicoMachineProvisionedCondition(nicoMachine.Status.Conditions)
	if provisionedCondition == nil ||
		provisionedCondition.Status != metav1.ConditionFalse ||
		provisionedCondition.Reason != nicov1.InstanceCreateFailedReason {
		return false, nil
	}
	if provisionedCondition.ObservedGeneration != 0 && provisionedCondition.ObservedGeneration < nicoMachine.Generation {
		return false, nil
	}
	if !isNicoNetworkExhaustionMessage(provisionedCondition.Message) {
		return false, nil
	}

	return true, nil
}

func rotateNicoMachineNetworkCandidate(ctx context.Context, k8sClient client.Client, nodeSet db.NodeSet, machine *clusterv1.Machine) error {
	nicoMachine := &nicov1.NicoMachine{}
	if err := k8sClient.Get(ctx, client.ObjectKey{
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
		return rotateNicoMachineVpcPrefix(ctx, k8sClient, nicoMachine, nicoNodeSetProvider.VpcPrefixIDs)
	}
	if currentInterface.SubnetID != "" {
		if len(nicoNodeSetProvider.SubnetIDs) == 0 {
			return nil
		}
		return rotateNicoMachineSubnet(ctx, k8sClient, nicoMachine, nicoNodeSetProvider.SubnetIDs)
	}

	return nil
}

func rotateNicoMachineVpcPrefix(ctx context.Context, k8sClient client.Client, nicoMachine *nicov1.NicoMachine, vpcPrefixIDs []uuid.UUID) error {
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
		if provisionedCondition := nicoMachineProvisionedCondition(nicoMachine.Status.Conditions); provisionedCondition != nil {
			failureMessage = provisionedCondition.Message
		}
		return fmt.Errorf("all %d VPC prefixes are exhausted, error from the last NicoMachine create attempt: %s", len(vpcPrefixIDs), failureMessage)
	}
	nextVpcPrefixID := vpcPrefixIDs[currentIndex+1].String()

	patchOpts := client.MergeFromWithOptions(nicoMachine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	nicoMachine.Spec.Interfaces[0].VPCPrefixID = nextVpcPrefixID
	nicoMachine.Spec.Interfaces[0].SubnetID = ""
	if err := k8sClient.Patch(ctx, nicoMachine, patchOpts); err != nil {
		return fmt.Errorf("failed to rotate NicoMachine VPC prefix from %s to %s: %w", currentVpcPrefixID, nextVpcPrefixID, err)
	}

	return nil
}

func rotateNicoMachineSubnet(ctx context.Context, k8sClient client.Client, nicoMachine *nicov1.NicoMachine, subnetIDs []uuid.UUID) error {
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
		if provisionedCondition := nicoMachineProvisionedCondition(nicoMachine.Status.Conditions); provisionedCondition != nil {
			failureMessage = provisionedCondition.Message
		}
		return fmt.Errorf("all %d subnets are exhausted, error from the last NicoMachine create attempt: %s", len(subnetIDs), failureMessage)
	}
	nextSubnetID := subnetIDs[currentIndex+1].String()

	patchOpts := client.MergeFromWithOptions(nicoMachine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	nicoMachine.Spec.Interfaces[0].SubnetID = nextSubnetID
	nicoMachine.Spec.Interfaces[0].VPCPrefixID = ""
	if err := k8sClient.Patch(ctx, nicoMachine, patchOpts); err != nil {
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
func shouldRotateNicoMachineInstanceType(ctx context.Context, k8sClient client.Client, machine *clusterv1.Machine) (bool, error) {
	nicoMachine := &nicov1.NicoMachine{}
	if err := k8sClient.Get(ctx, client.ObjectKey{
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

	provisionedCondition := nicoMachineProvisionedCondition(nicoMachine.Status.Conditions)
	if provisionedCondition == nil ||
		provisionedCondition.Status != metav1.ConditionFalse ||
		!isNicoInstanceTypeUnavailableReason(provisionedCondition.Reason) {
		return false, nil
	}
	if provisionedCondition.ObservedGeneration != 0 && provisionedCondition.ObservedGeneration < nicoMachine.Generation {
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
// the walk reads the live spec and needs no node-side bookkeeping. Returns
// whether it actually patched the NicoMachine.
func rotateNicoMachineInstanceType(ctx context.Context, deps shared.MachineAdvanceDeps, node db.Node, machine *clusterv1.Machine) (bool, error) {
	if !node.ControlPlane || node.ControlPlaneConfigurationID == uuid.Nil {
		return false, nil
	}
	snapshot, err := deps.ControlPlaneConfigSnapshot(ctx, node)
	if err != nil {
		return false, err
	}
	nicoProvider, err := snapshot.NicoProvider()
	if err != nil {
		return false, err
	}
	preference := nicoProvider.InstanceTypePreference
	if len(preference) == 0 {
		return false, nil
	}

	nicoMachine := &nicov1.NicoMachine{}
	if err := deps.K8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		return false, fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}
	currentInstanceTypeID := nicoMachine.Spec.InstanceTypeID
	if currentInstanceTypeID == "" {
		return false, nil
	}

	nextInstanceTypeID, err := shared.NextInPreference(preference, currentInstanceTypeID)
	switch {
	case errors.Is(err, shared.ErrPreferenceCurrentNotFound):
		return false, fmt.Errorf("current NicoMachine instance type %s is not in configured instance type preference", currentInstanceTypeID)
	case err != nil:
		return false, err
	}
	if nextInstanceTypeID.String() == currentInstanceTypeID {
		// Single-entry preference (or wrap-around to the same entry): nothing to
		// patch; keep retrying the same type in place until Nico gets capacity.
		return false, nil
	}

	patchOpts := client.MergeFromWithOptions(nicoMachine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	nicoMachine.Spec.InstanceTypeID = nextInstanceTypeID.String()
	if err := deps.K8sClient.Patch(ctx, nicoMachine, patchOpts); err != nil {
		return false, fmt.Errorf("failed to rotate NicoMachine instance type from %s to %s: %w", currentInstanceTypeID, nextInstanceTypeID, err)
	}

	return true, nil
}

// nicoMachineProvisionedCondition returns the NicoMachine MachineProvisioned
// condition, which carries the specific provisioning-failure reason
// (InstanceTypeUnavailable, InstanceTypeNotFound, InstanceCreateFailed, ...).
// The aggregate Ready condition only summarizes those with a generic reason
// (e.g. "NotReady"), so the rotation gates must key off MachineProvisioned to
// see why provisioning actually failed.
func nicoMachineProvisionedCondition(conditions []metav1.Condition) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == nicov1.MachineProvisionedCondition {
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
	// https://github.com/dsx-ai-factory/nke/controller/-/blob/dffb36a8/internal/providers/forge/api/errors.go#L57-L72

	nicoPreflightExhausted := strings.Contains(normalized, "does not have enough ip addresses:") &&
		(strings.Contains(normalized, "subnet ") || strings.Contains(normalized, "vpc prefix "))

	prefixExhausted := strings.Contains(normalized, "prefix:") &&
		strings.Contains(normalized, "has exhausted all address space")

	ipAddressExhausted := strings.Contains(normalized, "ip address exhausted:")

	return nicoPreflightExhausted || prefixExhausted || ipAddressExhausted
}
