// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// ProvisionedMachineSpec defines the desired state of ProvisionedMachine.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.providerID) || (has(self.providerID) && self.providerID == oldSelf.providerID)",message="providerID is immutable once set"
type ProvisionedMachineSpec struct {
	// ProviderID binds this machine to one host of the inventory. The provider
	// sets it once, in the form provisioned://<cluster-name>/<ip-address>.
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

// ProvisionedMachineInitializationStatus reports the initialization state
// that the Cluster API contract requires.
// +kubebuilder:validation:MinProperties=1
type ProvisionedMachineInitializationStatus struct {
	// Provisioned is true when the machine holds a host. Nothing is created, so
	// the machine is provisioned as soon as the binding exists.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// ProvisionedMachineStatus defines the observed state of
// ProvisionedMachine.
type ProvisionedMachineStatus struct {
	// Initialization reports the state that the Cluster API contract requires.
	// +optional
	Initialization ProvisionedMachineInitializationStatus `json:"initialization,omitempty,omitzero"`

	// HostName is the hostname of the host that this machine holds.
	// +optional
	HostName string `json:"hostName,omitempty"`

	// Addresses contain the hostname and internal IP address from the inventory.
	// Cluster API copies them to the Machine, and CWE reads the internal IP to
	// find the node agent.
	// +optional
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// Conditions describes the current state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:metadata:labels=cluster.x-k8s.io/provider=infrastructure-provisioned
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1alpha1
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=pm
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned"
// +kubebuilder:printcolumn:name="Host",type="string",JSONPath=".status.hostName"
// +kubebuilder:printcolumn:name="ProviderID",type="string",JSONPath=".spec.providerID"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ProvisionedMachine is the Schema for the provisionedmachines API.
type ProvisionedMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProvisionedMachineSpec   `json:"spec,omitempty"`
	Status ProvisionedMachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProvisionedMachineList contains a list of ProvisionedMachine.
type ProvisionedMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisionedMachine `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &ProvisionedMachine{}, &ProvisionedMachineList{})
}

// GetConditions returns the set of conditions for this object.
func (m *ProvisionedMachine) GetConditions() []metav1.Condition {
	return m.Status.Conditions
}

// SetConditions sets conditions for this object.
func (m *ProvisionedMachine) SetConditions(conditions []metav1.Condition) {
	m.Status.Conditions = conditions
}

var (
	_ runtime.Object = &ProvisionedMachine{}
	_ runtime.Object = &ProvisionedMachineList{}
)
