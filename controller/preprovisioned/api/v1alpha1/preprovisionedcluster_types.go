// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// HostRole limits which machines can use a host.
// +kubebuilder:validation:Enum=control-plane;worker
type HostRole string

const (
	// HostRoleControlPlane reserves a host for control plane machines.
	HostRoleControlPlane HostRole = "control-plane"

	// HostRoleWorker reserves a host for worker machines.
	HostRoleWorker HostRole = "worker"
)

// PreprovisionedHost describes one host that already exists on the network.
// The host is on, the operating system is installed, and the network already
// assigned the addresses. No controller creates or changes the host.
type PreprovisionedHost struct {
	// Name identifies the host in the inventory. It must be unique in the
	// cluster.
	//
	// The name is a label for operators. It does not become the Kubernetes node
	// name: CWE generates the node name and passes it to kubeadm.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Addresses are the addresses that the network already assigned to this
	// host. The provider copies them to the status of the machine that holds
	// the host, and CWE uses the first internal address to reach the node
	// agent.
	// +kubebuilder:validation:MinItems=1
	Addresses []clusterv1.MachineAddress `json:"addresses"`

	// MachineID is a stable hardware identity for this host: a BMC serial, a
	// system UUID, or anything else that names the machine itself rather than
	// where it currently sits on the network.
	//
	// Give it where you have it. The provider locks a host under every identity
	// it declares, so a machineID makes the lock hold even if the address is
	// reassigned, and makes two inventories collide even when they use
	// different names and different addresses for the same machine.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	MachineID string `json:"machineID,omitempty"`

	// Role limits which machines can use this host. An empty value allows any
	// machine to use it.
	// +optional
	Role HostRole `json:"role,omitempty"`

	// Labels select this host. A machine that sets spec.hostSelector only uses
	// a host whose labels contain every entry of that selector.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// PreprovisionedClusterSpec defines the desired state of PreprovisionedCluster.
//
// The spec holds no control plane endpoint. CWE owns the control plane VIP
// through the controlPlaneVIP field of its own cluster API, and CWE configures
// kube-vip on each control plane node.
type PreprovisionedClusterSpec struct {
	// Hosts is the inventory. The provider gives one host to each machine.
	//
	// Treat the inventory as fixed after the cluster is created. Size it for
	// the cluster that you want.
	// +kubebuilder:validation:MinItems=1
	Hosts []PreprovisionedHost `json:"hosts"`
}

// PreprovisionedClusterInitializationStatus reports the initialization state
// that the Cluster API contract requires.
// +kubebuilder:validation:MinProperties=1
type PreprovisionedClusterInitializationStatus struct {
	// Provisioned is true when the inventory is valid. The core Cluster API
	// machine controller waits for this field before it acts on any machine of
	// the cluster.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// HostAllocation reports one host that a machine holds.
type HostAllocation struct {
	// HostName is the name of the host in the inventory.
	HostName string `json:"hostName"`

	// MachineName is the name of the PreprovisionedMachine that holds the host.
	MachineName string `json:"machineName"`
}

// PreprovisionedClusterStatus defines the observed state of
// PreprovisionedCluster.
type PreprovisionedClusterStatus struct {
	// Initialization reports the state that the Cluster API contract requires.
	// +optional
	Initialization PreprovisionedClusterInitializationStatus `json:"initialization,omitempty,omitzero"`

	// AllocatedHosts reports which machine holds each host.
	//
	// The provider computes this list from the PreprovisionedMachine objects on
	// every reconcile. It exists for operators. No controller reads it, and the
	// provider never uses it to decide an allocation.
	// +optional
	AllocatedHosts []HostAllocation `json:"allocatedHosts,omitempty"`

	// AvailableHosts is the count of hosts that no machine holds.
	//
	// Deliberately not omitempty: an exhausted inventory reports 0, and dropping
	// the field would make "no host is free" look identical to "the provider has
	// not reported yet" — which is exactly the moment an operator needs to tell
	// them apart.
	// +optional
	AvailableHosts int32 `json:"availableHosts"`

	// Conditions describes the current state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=ppc
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned"
// +kubebuilder:printcolumn:name="Hosts",type="integer",JSONPath=".status.availableHosts",description="Hosts that no machine holds"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PreprovisionedCluster is the Schema for the preprovisionedclusters API.
type PreprovisionedCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PreprovisionedClusterSpec   `json:"spec,omitempty"`
	Status PreprovisionedClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PreprovisionedClusterList contains a list of PreprovisionedCluster.
type PreprovisionedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreprovisionedCluster `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &PreprovisionedCluster{}, &PreprovisionedClusterList{})
}

// GetConditions returns the set of conditions for this object.
func (c *PreprovisionedCluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets conditions for this object.
func (c *PreprovisionedCluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// Host returns the inventory entry with the given name.
func (c *PreprovisionedCluster) Host(name string) (PreprovisionedHost, bool) {
	for _, host := range c.Spec.Hosts {
		if host.Name == name {
			return host, true
		}
	}
	return PreprovisionedHost{}, false
}

var (
	_ runtime.Object = &PreprovisionedCluster{}
	_ runtime.Object = &PreprovisionedClusterList{}
)
