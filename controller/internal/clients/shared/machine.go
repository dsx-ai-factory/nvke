// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package shared holds machine-materialization pieces genuinely common to
// every provider: the dependencies Activities supplies to a provider client's
// CreateMachine, and the generic CAPI Machine/Secret builders neither
// provider's rendering differs on. It also holds the value types a
// capability interface returns, such as TenantAPIAccessMethod. It exists so internal/clients/capi and
// internal/clients/nico can both depend on this without either importing the
// other, and without internal/clients (which imports both, for the factory)
// creating an import cycle.
package shared

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

var (
	// ErrUnsupportedMachineConfig marks a CreateMachine failure caused by the
	// node's own configuration, which no retry can fix.
	ErrUnsupportedMachineConfig = errors.New("unsupported machine configuration")

	// ErrSpireAgentPathPending marks a SPIRE agent ID path the infrastructure
	// provider has not yet reported enough to build.
	ErrSpireAgentPathPending = errors.New("SPIRE agent ID path not yet resolvable")
)

// StampForgeIdentity records the Forge instance and machine IDs a
// NicoMachine reports on its status onto the node's nico payload, never
// overwriting a recorded value. It invents no nico payload from an empty
// observation, so a CAPD machine stays without one.
func StampForgeIdentity(node *db.Node, infraMachine *unstructured.Unstructured) error {
	instanceID, _, err := unstructured.NestedString(infraMachine.Object, "status", "instanceID")
	if err != nil {
		return fmt.Errorf("read %s status.instanceID: %w", infraMachine.GetKind(), err)
	}
	machineID, _, err := unstructured.NestedString(infraMachine.Object, "status", "machineID")
	if err != nil {
		return fmt.Errorf("read %s status.machineID: %w", infraMachine.GetKind(), err)
	}
	if instanceID == "" && machineID == "" && node.Nico == nil {
		return nil
	}
	nico := db.NicoNodeProvider{}
	if node.Nico != nil {
		nico = *node.Nico
	}
	if nico.ForgeInstanceID == "" {
		nico.ForgeInstanceID = instanceID
	}
	if nico.ForgeMachineID == "" {
		nico.ForgeMachineID = machineID
	}
	node.SetNico(nico)
	return nil
}

// MachineCreateDeps bundles what Activities supplies so a provider client's
// CreateMachine can run without the clients package depending on
// Activities-level infrastructure: the k8s client, raw config, and a few
// generic operations (SPIFFE trust bundle lookup, node-agent bootstrap
// files, SPIRE join token minting, control-plane instance-type seeding) that
// only Activities can perform (DB/SPIRE-server access a client doesn't
// have).
//
// This is a fixed shape every provider client draws from — there is no
// per-provider-named field here. Bootstrap-content rendering, which does
// differ by provider, lives entirely inside each client package instead of
// being handed in as a pre-built callback; a client composes its own content
// from the primitives below. Activities populates every field the same way
// regardless of the cluster's provider type, so the cluster's provider type
// is still checked in exactly one place: the factory that selected the
// client.
type MachineCreateDeps struct {
	K8sClient        client.Client
	FieldManagerName string
	ProfilesDir      string
	Namespace        string

	// Config is the subset of Activities' own config a client's bootstrap
	// rendering needs (SPIRE/Teleport/node-agent/DGXC-tenant settings). A
	// client reads only the fields relevant to its own provider.
	Config config.Config

	// ControllerClientCABundle is the PEM bundle a node needs to verify the controller's
	// client certificate when external node-agent mTLS is enabled. Empty when
	// not configured.
	ControllerClientCABundle string

	// GetTrustDomainBundle resolves the local SPIFFE trust domain and bundle.
	// Only called when a node has SpiffeEnabled set.
	GetTrustDomainBundle func(ctx context.Context, cfg config.Config) (trustDomain, trustBundle string, err error)

	// NodeAgentBootstrapFiles returns the cloud-init files that configure the
	// node agent (and, when external mTLS is enabled, the controller CA).
	NodeAgentBootstrapFiles func(node db.Node) ([]db.CloudInitFile, error)

	// CreateSpireJoinToken mints a SPIRE join token for the node, or returns
	// ("", "", nil) if none is needed. Only the CAPI template-backed client
	// calls this today.
	CreateSpireJoinToken func(ctx context.Context, node db.Node) (token, alternateSpiffeIDPath string, err error)

	// SeedControlPlaneInstanceType pre-fills a control-plane node's instance
	// type when the controller didn't set one, returning the (possibly updated) node
	// pool. Only the nico client calls this.
	SeedControlPlaneInstanceType func(ctx context.Context, node db.Node, nodePool db.NodePool) (db.NodePool, error)
}

// MachineAdvanceDeps bundles what Activities supplies so a provider client's
// TryAdvanceStuckMachine can run. Separate from MachineCreateDeps because it
// serves a different caller (waitForCAPIMachineReady, not
// CreateCAPIInfraMachineForNode) with a much smaller set of needs — just the
// k8s client and the one DB-backed lookup only Activities can perform.
type MachineAdvanceDeps struct {
	K8sClient client.Client

	// ControlPlaneConfigSnapshot returns the control-plane-config snapshot
	// pinned to node's ControlPlaneConfigurationID. Only called for a
	// control-plane node that carries one — the caller checks eligibility
	// itself, since that only needs fields already on db.Node. Interpreting
	// the snapshot's contents (e.g. a NICo instance-type preference) is the
	// caller's job too; this is just the DB-backed fetch only Activities can
	// perform.
	ControlPlaneConfigSnapshot func(ctx context.Context, node db.Node) (db.ControlPlaneConfigSpec, error)
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

	labels := db.CloneStringMap(nodePool.MachineLabels)
	maps.Copy(labels, MachineLabels(cluster.Name, nodePool.ControlPlane))
	annotations := db.CloneStringMap(nodePool.MachineAnnotations)
	maps.Copy(annotations, MachineAnnotations(node))

	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
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

// MachineAnnotations returns the controller-owned annotations added to every Machine.
func MachineAnnotations(node db.Node) map[string]string {
	mode := "plaintext"
	switch {
	case node.NodeAgentExternalMTLSEnabled:
		mode = "external-mtls"
	case node.SpiffeEnabled:
		mode = "spiffe"
	}
	if node.NodeAgentTLSJWTEnabled {
		mode += "-jwt"
	}
	// The controller owns drain / kube Node removal in teardown workflows. Tell CAPI to skip its
	// drain and volume-detach waits so Machine deletion is not blocked when the
	// workload API is unreachable.
	return map[string]string{
		keys.MachineAgentConnection.String():               mode,
		keys.MachineHostID.String():                        node.ID.String(),
		clusterv1.ExcludeNodeDrainingAnnotation:            "cwe-controlled",
		clusterv1.ExcludeWaitForNodeVolumeDetachAnnotation: "cwe-controlled",
	}
}
