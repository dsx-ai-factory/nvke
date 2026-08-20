// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// ClaimClusterLabel records which cluster holds a claim.
	ClaimClusterLabel = "preprovisioned.nke.nvidia.com/cluster"

	// ClaimMachineNamespaceLabel records the namespace of the holding machine.
	ClaimMachineNamespaceLabel = "preprovisioned.nke.nvidia.com/machine-namespace"

	// ClaimMachineLabel records the name of the holding machine.
	ClaimMachineLabel = "preprovisioned.nke.nvidia.com/machine"
)

// PreprovisionedHostClaimSpec records who holds one identity of one physical
// host.
//
// The object's existence is the lock. Nothing reads this spec to make a
// decision; it is here so an operator can see who holds what, and so a blocked
// claimant can find the holder and check whether it still exists.
type PreprovisionedHostClaimSpec struct {
	// Identity is the value being locked: a machineID, or an address.
	// +kubebuilder:validation:MinLength=1
	Identity string `json:"identity"`

	// ClusterName is the cluster whose inventory listed the host.
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`

	// MachineNamespace and MachineName locate the PreprovisionedMachine that
	// holds the claim. A claimant that finds this machine gone reclaims the
	// host rather than waiting forever.
	// +kubebuilder:validation:MinLength=1
	MachineNamespace string `json:"machineNamespace"`

	// +kubebuilder:validation:MinLength=1
	MachineName string `json:"machineName"`

	// HostName is the name the inventory gave the host, for operators. Two
	// clusters may well call the same machine different things, which is the
	// whole reason the lock keys on identity instead.
	// +optional
	HostName string `json:"hostName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=cluster-api,shortName=pphc
// +kubebuilder:printcolumn:name="Identity",type="string",JSONPath=".spec.identity"
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterName"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".spec.machineName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PreprovisionedHostClaim locks one identity of one physical host across every
// cluster.
//
// It is cluster-scoped on purpose. A physical machine is a singleton, but each
// cluster carries its own inventory in its own namespace, so a namespaced lock
// could not see across the boundary — which is exactly the case it exists to
// stop. Creating the object is the mutual exclusion: two claimants race, and the
// API server rejects the second with AlreadyExists.
type PreprovisionedHostClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PreprovisionedHostClaimSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// PreprovisionedHostClaimList contains a list of PreprovisionedHostClaim.
type PreprovisionedHostClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreprovisionedHostClaim `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &PreprovisionedHostClaim{}, &PreprovisionedHostClaimList{})
}

// HostClaimName is the object name that locks an identity.
//
// The identity is hashed rather than sanitized. An identity may be an address,
// a serial number or a UUID, and those have no common safe encoding; hashing
// gives one name shape for all of them and cannot collide two different
// identities onto one lock through sanitizing. The readable value stays in
// spec.identity and in the print columns.
func HostClaimName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return "hostclaim-" + hex.EncodeToString(sum[:])
}

// Identities returns every value that identifies this host.
//
// A host is locked under all of them, so two inventories that describe the same
// machine by different fields still collide. An inventory that gives only an
// address locks the address; one that also gives a machineID locks both, and
// overlaps with either.
func (h PreprovisionedHost) Identities() []string {
	identities := make([]string, 0, len(h.Addresses)+1)
	if h.MachineID != "" {
		identities = append(identities, h.MachineID)
	}
	for _, address := range h.Addresses {
		if address.Address != "" {
			identities = append(identities, address.Address)
		}
	}
	return identities
}

var (
	_ runtime.Object = &PreprovisionedHostClaim{}
	_ runtime.Object = &PreprovisionedHostClaimList{}
)
