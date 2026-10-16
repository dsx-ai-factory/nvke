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

// ValidateNodeAgentProfile rejects a SPIFFE-enabled node that would attest
// any way but dsxImds. dsxImds is the one identity a LaunchLayer node has:
// the Compute API reports no TPM endorsement key, and join-token
// attestation is not offered for the production-target provider.
func (*Client) ValidateNodeAgentProfile(node db.Node) error {
	if node.SpiffeEnabled && node.SpireAttestationMethod != db.SpireAttestationMethodDSXIMDS {
		return fmt.Errorf("SPIFFE-enabled LaunchLayer nodes require %s SPIRE attestation, got %s",
			db.SpireAttestationMethodDSXIMDS, node.SpireAttestationMethod)
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

	instanceID, err := readStatusString(ctx, k8sClient, machine.Namespace, machine.Spec.InfrastructureRef, "LaunchLayerMachine", "instanceID")
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
	orgID, err := readStatusString(ctx, k8sClient, cluster.Namespace, cluster.Spec.InfrastructureRef, "LaunchLayerCluster", "vpc", "orgID")
	if err != nil {
		return "", err
	}
	if orgID == "" {
		return "", fmt.Errorf("%w: LaunchLayerCluster %s/%s has no status.vpc.orgID yet",
			shared.ErrSpireAgentPathPending, cluster.Namespace, cluster.Spec.InfrastructureRef.Name)
	}

	return constants.DSXIMDSSpireAgentPath + orgID + "/bm/" + instanceID, nil
}

// readStatusString reads a string status field off the CAPLL object ref
// names, which must be of the given kind.
func readStatusString(ctx context.Context, k8sClient client.Client, namespace string, ref clusterv1.ContractVersionedObjectReference, kind string, fields ...string) (string, error) {
	if ref.APIGroup != infrastructureGroup || ref.Kind != kind || ref.Name == "" {
		return "", fmt.Errorf("infrastructure reference %s/%s %q is not a %s", ref.APIGroup, ref.Kind, ref.Name, kind)
	}
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(infrastructureAPIVersion)
	obj.SetKind(kind)
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, obj); err != nil {
		return "", fmt.Errorf("get %s %s/%s: %w", kind, namespace, ref.Name, err)
	}
	value, _, err := unstructured.NestedString(obj.Object, append([]string{"status"}, fields...)...)
	if err != nil {
		return "", fmt.Errorf("read %s status: %w", kind, err)
	}
	return value, nil
}
