// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ProvisionedHostClaimSpec records who holds one IP address.
//
// The object's existence is the lock. The spec identifies its exact holder so
// a blocked claimant can determine whether that machine still exists.
type ProvisionedHostClaimSpec struct {
	// IPAddress is the canonical address being locked.
	// +kubebuilder:validation:MinLength=1
	IPAddress string `json:"ipAddress"`

	// ClusterName is the cluster whose inventory listed the host.
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`

	// MachineNamespace, MachineName, and MachineUID identify the exact
	// ProvisionedMachine that holds the claim. The UID prevents a new machine
	// with the same namespace and name from inheriting an old claim.
	// +kubebuilder:validation:MinLength=1
	MachineNamespace string `json:"machineNamespace"`

	// +kubebuilder:validation:MinLength=1
	MachineName string `json:"machineName"`

	// +kubebuilder:validation:MinLength=1
	MachineUID string `json:"machineUID"`

	// HostName is the name the inventory gave the host, for operators. Two
	// clusters may well call the same machine different things, which is the
	// whole reason the lock keys on the IP address instead.
	// +optional
	HostName string `json:"hostName,omitempty"`
}

// +kubebuilder:metadata:labels=cluster.x-k8s.io/provider=infrastructure-provisioned
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=cluster-api,shortName=phc
// +kubebuilder:printcolumn:name="IPAddress",type="string",JSONPath=".spec.ipAddress"
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterName"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".spec.machineName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ProvisionedHostClaim locks one IP address across every cluster.
//
// It is cluster-scoped on purpose. A physical machine is a singleton, but each
// cluster carries its own inventory in its own namespace, so a namespaced lock
// could not see across the boundary — which is exactly the case it exists to
// stop. Creating the object is the mutual exclusion: two claimants race, and the
// API server rejects the second with AlreadyExists.
type ProvisionedHostClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProvisionedHostClaimSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ProvisionedHostClaimList contains a list of ProvisionedHostClaim.
type ProvisionedHostClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisionedHostClaim `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &ProvisionedHostClaim{}, &ProvisionedHostClaimList{})
}

// HostClaimName is the object name that locks an IP address.
//
// The IP address is hashed rather than sanitized because IPv6 addresses are
// not valid Kubernetes object names. The readable value stays in
// spec.ipAddress and in the print columns.
func HostClaimName(ipAddress string) string {
	sum := sha256.Sum256([]byte(ipAddress))
	return "hostclaim-" + hex.EncodeToString(sum[:])
}

var (
	_ runtime.Object = &ProvisionedHostClaim{}
	_ runtime.Object = &ProvisionedHostClaimList{}
)
