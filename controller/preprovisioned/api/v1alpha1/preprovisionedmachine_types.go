// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// PreprovisionedMachineSpec defines the desired state of PreprovisionedMachine.
type PreprovisionedMachineSpec struct {
	// ProviderID binds this machine to one host of the inventory. The provider
	// sets it once, in the form preprovisioned://<cluster-name>/<host-name>.
	// The provider never changes it and never clears it.
	//
	// This field is the record of the allocation. The provider derives the set
	// of used hosts from the providerID of every machine of the cluster, so the
	// binding survives a restart of the provider and needs no lease.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ProviderID string `json:"providerID,omitempty"`

	// HostSelector limits which hosts this machine can use. The provider only
	// uses a host whose labels contain every entry of this selector.
	// +optional
	HostSelector map[string]string `json:"hostSelector,omitempty"`
}

// PreprovisionedMachineInitializationStatus reports the initialization state
// that the Cluster API contract requires.
// +kubebuilder:validation:MinProperties=1
type PreprovisionedMachineInitializationStatus struct {
	// Provisioned is true when the machine holds a host. Nothing is created, so
	// the machine is provisioned as soon as the binding exists.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// PreprovisionedMachineStatus defines the observed state of
// PreprovisionedMachine.
type PreprovisionedMachineStatus struct {
	// Initialization reports the state that the Cluster API contract requires.
	// +optional
	Initialization PreprovisionedMachineInitializationStatus `json:"initialization,omitempty,omitzero"`

	// HostName is the name of the host that this machine holds.
	// +optional
	HostName string `json:"hostName,omitempty"`

	// Addresses are the addresses of that host, copied from the inventory.
	// Cluster API copies them to the Machine, and CWE reads them to find the
	// node agent.
	// +optional
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// Conditions describes the current state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=ppm
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned"
// +kubebuilder:printcolumn:name="Host",type="string",JSONPath=".status.hostName"
// +kubebuilder:printcolumn:name="ProviderID",type="string",JSONPath=".spec.providerID"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PreprovisionedMachine is the Schema for the preprovisionedmachines API.
type PreprovisionedMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PreprovisionedMachineSpec   `json:"spec,omitempty"`
	Status PreprovisionedMachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PreprovisionedMachineList contains a list of PreprovisionedMachine.
type PreprovisionedMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreprovisionedMachine `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &PreprovisionedMachine{}, &PreprovisionedMachineList{})
}

// GetConditions returns the set of conditions for this object.
func (m *PreprovisionedMachine) GetConditions() []metav1.Condition {
	return m.Status.Conditions
}

// SetConditions sets conditions for this object.
func (m *PreprovisionedMachine) SetConditions(conditions []metav1.Condition) {
	m.Status.Conditions = conditions
}

var (
	_ runtime.Object = &PreprovisionedMachine{}
	_ runtime.Object = &PreprovisionedMachineList{}
)
