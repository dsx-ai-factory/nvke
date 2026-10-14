// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// buildInfraMachine builds the LaunchLayerMachine the controller constructs
// for a LaunchLayer-backed node, natively from the node set's LaunchLayer
// provider payload — the same construction style NICo uses.
func buildInfraMachine(node db.Node, nodeSet db.NodeSet, cluster db.Cluster, controlPlane bool) (*unstructured.Unstructured, error) {
	provider, err := nodeSet.LaunchLayerProvider()
	if err != nil {
		return nil, fmt.Errorf("failed to read node set launchlayer provider: %w", err)
	}
	if provider.InstanceType == "" {
		return nil, fmt.Errorf("launchlayer node set %s has no instanceType set", nodeSet.ID)
	}

	spec := map[string]any{"instanceType": provider.InstanceType}
	if provider.HostOSProfile != "" {
		spec["hostOSProfile"] = provider.HostOSProfile
	}
	if provider.VMProfile != "" {
		spec["vmProfile"] = provider.VMProfile
	}
	if provider.FulfillmentStrategy != "" {
		spec["fulfillmentStrategy"] = provider.FulfillmentStrategy
	}
	if refs := buildSecurityGroupRefs(provider.SecurityGroupRefs); refs != nil {
		spec["securityGroupRefs"] = refs
	}

	machine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": infrastructureAPIVersion,
		"kind":       "LaunchLayerMachine",
		"metadata": map[string]any{
			nameKey:     node.Name,
			"namespace": "", // set by the caller once namespace is known
			"labels":    shared.MachineLabels(cluster.Name, controlPlane),
		},
		"spec": spec,
	}}
	return machine, nil
}

func buildSecurityGroupRefs(refs db.LaunchLayerSecurityGroupRefs) map[string]any {
	if len(refs.PrimaryNetworkInterface) == 0 && len(refs.GPUNetworkInterface) == 0 {
		return nil
	}
	out := map[string]any{}
	if len(refs.PrimaryNetworkInterface) > 0 {
		out["primaryNetworkInterface"] = toAnySlice(refs.PrimaryNetworkInterface)
	}
	if len(refs.GPUNetworkInterface) > 0 {
		out["gpuNetworkInterface"] = toAnySlice(refs.GPUNetworkInterface)
	}
	return out
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// CreateMachine renders and applies the LaunchLayerMachine, renders and
// applies the bootstrap secret (CAPLL is a pure bootstrap pass-through, so
// SPIRE material has to already be in the cloud-init payload — the same
// reasoning as the CAPI client), and applies the generic CAPI Machine
// pointing at the LaunchLayerMachine.
//
// Unlike CAPI, this never mints a SPIRE join token: CAPLL is the
// production-target provider, and join-token attestation must not be
// reachable for it even as a configurable option. renderBootstrapUserData
// rejects a node configured for join-token attestation outright instead.
func (c *Client) CreateMachine(ctx context.Context, deps shared.MachineCreateDeps, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) error {
	infraMachine, err := buildInfraMachine(node, nodeSet, cluster, nodePool.ControlPlane)
	if err != nil {
		return err
	}
	infraMachine.SetNamespace(deps.Namespace)

	if err := deps.K8sClient.Patch(ctx, infraMachine, client.Apply, client.FieldOwner(deps.FieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply LaunchLayerMachine %s: %w", infraMachine.GetName(), err)
	}

	userData, err := c.renderBootstrapUserData(ctx, deps, node)
	if err != nil {
		return fmt.Errorf("failed to render LaunchLayer bootstrap user data: %w", err)
	}

	bootstrapSecret := shared.BuildBootstrapSecret(node, cluster, deps.Namespace, userData)
	if err := deps.K8sClient.Patch(ctx, bootstrapSecret, client.Apply, client.FieldOwner(deps.FieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply bootstrap secret %s: %w", bootstrapSecret.Name, err)
	}

	machine, err := shared.BuildMachine(node, nodeSet, nodePool, cluster, deps.Namespace, bootstrapSecret.Name, clusterv1.ContractVersionedObjectReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     "LaunchLayerMachine",
		Name:     infraMachine.GetName(),
	})
	if err != nil {
		return err
	}
	return shared.ApplyMachine(ctx, deps.K8sClient, deps.FieldManagerName, machine)
}
