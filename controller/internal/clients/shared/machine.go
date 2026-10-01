// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package shared holds machine-materialization pieces genuinely common to
// every provider: the dependencies Activities supplies to a provider client's
// CreateMachine, and the generic CAPI Machine/Secret builders neither
// provider's rendering differs on. It exists so internal/clients/capi and
// internal/clients/nico can both depend on this without either importing the
// other, and without internal/clients (which imports both, for the factory)
// creating an import cycle.
package shared

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// MachineCreateDeps bundles what Activities supplies so a provider client's
// CreateMachine can run without the clients package depending on
// Activities-level infrastructure (k8s field managers aside): SPIFFE/SPIRE,
// Teleport, and node-agent config.
//
// Activities populates every field the same way regardless of the cluster's
// provider type — it does not branch to decide which callbacks to set. Each
// client calls only the callbacks relevant to its own provider and ignores
// the rest, so the cluster's provider type is still checked in exactly one
// place: the factory that selected the client.
type MachineCreateDeps struct {
	K8sClient        client.Client
	FieldManagerName string
	ProfilesDir      string
	Namespace        string

	// CreateSpireJoinToken mints a SPIRE join token for the node, or returns
	// ("", "", nil) if none is needed. Only the CAPI template-backed client
	// calls this today.
	CreateSpireJoinToken func(ctx context.Context, node db.Node) (token, alternateSpiffeIDPath string, err error)

	// RenderCAPIBootstrapUserData renders bootstrap user-data for a
	// template-backed CAPI node. Only the capi client calls this.
	RenderCAPIBootstrapUserData func(ctx context.Context, node db.Node, nodeSet db.NodeSet, spireJoinToken string) (string, error)

	// RenderNicoBootstrapUserData renders bootstrap user-data for a
	// template-less NICo node. Only the nico client calls this.
	RenderNicoBootstrapUserData func(ctx context.Context, node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster) (string, error)

	// SeedControlPlaneInstanceType pre-fills a control-plane node's instance
	// type when CWE didn't set one, returning the (possibly updated) node
	// pool. Only the nico client calls this.
	SeedControlPlaneInstanceType func(ctx context.Context, node db.Node, nodePool db.NodePool) (db.NodePool, error)
}

// BuildBootstrapSecret builds the bootstrap data Secret for a node. Identical
// for every provider — the content differs, not the shape.
func BuildBootstrapSecret(node db.Node, cluster db.Cluster, namespace, userData string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-bootstrap-data", node.Name),
			Namespace: namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"value": []byte(userData),
		},
	}
}

// BuildMachine builds the generic CAPI Machine pointing at a provider's
// already-applied infrastructure object. Identical for every provider — only
// the infrastructureRef differs.
func BuildMachine(node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster, namespace, bootstrapSecretName string, infrastructureRef clusterv1.ContractVersionedObjectReference) (*clusterv1.Machine, error) {
	machineVersion := nodeSet.KubernetesVersion
	if machineVersion == "" {
		machineVersion = cluster.KubernetesVersion
	}
	if machineVersion == "" {
		return nil, fmt.Errorf("kubernetes version is empty for node set %s", nodeSet.ID)
	}
	if !strings.HasPrefix(machineVersion, "v") {
		machineVersion = "v" + machineVersion
	}

	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Namespace:   namespace,
			Labels:      MachineLabels(cluster.Name, nodePool.ControlPlane),
			Annotations: machineAnnotations(node),
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:   cluster.Name,
			FailureDomain: node.FailureDomain,
			Version:       machineVersion,
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: &bootstrapSecretName,
			},
			InfrastructureRef: infrastructureRef,
		},
	}, nil
}

// ApplyMachine creates a CAPI Machine, or reapplies one that already exists
// while guarding against any change to the spec's failure domain. A re-apply
// can race a node's FailureDomain being backfilled in the database, and
// without this guard that race would intermittently clear an
// already-placed domain back to empty.
func ApplyMachine(ctx context.Context, k8sClient client.Client, fieldManagerName string, machine *clusterv1.Machine) error {
	existing := &clusterv1.Machine{}
	switch err := k8sClient.Get(ctx, client.ObjectKeyFromObject(machine), existing); {
	case apierrors.IsNotFound(err):
		// No Machine yet; the apply below creates it with the placement this
		// attempt computed.
	case err != nil:
		return fmt.Errorf("failed to get existing CAPI machine %s: %w", machine.Name, err)
	default:
		machine.Spec.FailureDomain = existing.Spec.FailureDomain
	}

	if err := k8sClient.Patch(ctx, machine, client.Apply, client.FieldOwner(fieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply machine %s: %w", machine.Name, err)
	}
	return nil
}

// MachineLabels returns the cluster-name/control-plane labels every provider
// applies identically, both to the generic CAPI Machine and (for providers
// that have one) their own infra object.
func MachineLabels(clusterName string, controlPlane bool) map[string]string {
	labels := map[string]string{
		clusterv1.ClusterNameLabel: clusterName,
	}
	if controlPlane {
		labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	return labels
}

func machineAnnotations(node db.Node) map[string]string {
	// CWE owns drain / kube Node removal in teardown workflows. Tell CAPI to skip its
	// drain and volume-detach waits so Machine deletion is not blocked when the
	// workload API is unreachable.
	return map[string]string{
		constants.MachineAnnotationKeyHostID:               node.ID.String(),
		clusterv1.ExcludeNodeDrainingAnnotation:            "cwe-controlled",
		clusterv1.ExcludeWaitForNodeVolumeDetachAnnotation: "cwe-controlled",
	}
}
