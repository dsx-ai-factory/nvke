// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ValidateNodeAgentProfile rejects a vmProfile pool on a SPIFFE-enabled
// deployment without external mTLS. LaunchLayer nodes attest through dsx_imds
// (see attestThroughDSXImds), and DSX issues that identity to bare-metal
// instances only; external mTLS carries the node-agent connection without an
// SVID, so VMs work there.
func (*Client) ValidateNodeAgentProfile(profile db.Node, nodePool db.NodePool) error {
	if !profile.SpiffeEnabled {
		return nil
	}
	if nodePool.LaunchLayer != nil && nodePool.LaunchLayer.VMProfile != "" && !profile.NodeAgentExternalMTLSEnabled {
		return fmt.Errorf("SPIFFE-enabled LaunchLayer nodes without external mTLS require hostOSProfile: "+
			"%s attestation is not available to vmProfile %q instances", db.SpireAttestationMethodDSXIMDS, nodePool.LaunchLayer.VMProfile)
	}
	return nil
}

// SpireAgentPath returns the dsx_imds agent ID path DSX gives a LaunchLayer
// bare-metal instance, /spire/agent/dsx_imds/tenant/<orgID>/bm/<instanceID>,
// from LaunchLayerCluster.status.vpc.orgID and
// LaunchLayerMachine.status.instanceID. A node without SPIFFE, or attesting
// any other way, has no path from here.
func (*Client) SpireAgentPath(ctx context.Context, k8sClient client.Client, node db.Node, machine *clusterv1.Machine) (string, error) {
	if !node.SpiffeEnabled || node.SpireAttestationMethod != db.SpireAttestationMethodDSXIMDS {
		return "", nil
	}

	instanceID, err := readStatusString(ctx, k8sClient, machine.Namespace, machine.Spec.InfrastructureRef.Name, "LaunchLayerMachine", "instanceID")
	if err != nil {
		return "", err
	}
	if instanceID == "" {
		return "", fmt.Errorf("%w: LaunchLayerMachine %s/%s has no status.instanceID yet",
			shared.ErrSpireAgentPathPending, machine.Namespace, machine.Spec.InfrastructureRef.Name)
	}

	cluster, err := util.GetClusterByName(ctx, k8sClient, machine.Namespace, machine.Spec.ClusterName)
	if err != nil {
		return "", fmt.Errorf("get cluster for Machine %s/%s: %w", machine.Namespace, machine.Name, err)
	}
	orgID, err := readStatusString(ctx, k8sClient, cluster.Namespace, cluster.Spec.InfrastructureRef.Name, "LaunchLayerCluster", "vpc", "orgID")
	if err != nil {
		return "", err
	}
	if orgID == "" {
		return "", fmt.Errorf("%w: LaunchLayerCluster %s/%s has no status.vpc.orgID yet",
			shared.ErrSpireAgentPathPending, cluster.Namespace, cluster.Spec.InfrastructureRef.Name)
	}

	return constants.DSXIMDSSpireAgentPath + orgID + "/bm/" + instanceID, nil
}

// readStatusString reads a string status field off the named CAPLL object.
// NVKE writes the infrastructure references it follows, so they need no check.
func readStatusString(ctx context.Context, k8sClient client.Client, namespace, name, kind string, fields ...string) (string, error) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(infrastructureAPIVersion)
	obj.SetKind(kind)
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return "", fmt.Errorf("get %s %s/%s: %w", kind, namespace, name, err)
	}
	value, _, err := unstructured.NestedString(obj.Object, append([]string{"status"}, fields...)...)
	if err != nil {
		return "", fmt.Errorf("read %s status: %w", kind, err)
	}
	return value, nil
}
