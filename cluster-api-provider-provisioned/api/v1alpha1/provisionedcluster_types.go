// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ProvisionedHost describes one host that already exists on the network.
// The host is on, the operating system is installed, and the network already
// assigned its IP address. No controller creates or changes the host.
type ProvisionedHost struct {
	// Hostname is the DNS hostname of the host. It must be unique in the cluster.
	// It does not become the Kubernetes Node name: CWE generates the Node name
	// and passes it to kubeadm.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Hostname string `json:"hostname"`

	// IPAddress is the IP address that the network already assigned to this
	// host. The provider uses its canonical form in the provider ID and copies
	// it to the status of the machine that holds the host. CWE uses it to reach
	// the node agent.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=45
	IPAddress string `json:"ipAddress"`

	// Labels select this host. A machine that sets spec.hostSelector only uses
	// a host whose labels contain every entry of that selector.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// ProvisionedClusterSpec defines the desired state of ProvisionedCluster.
//
// The spec holds no control plane endpoint. CWE owns the control plane VIP
// through the controlPlaneVIP field of its own cluster API, and CWE configures
// kube-vip on each control plane node.
type ProvisionedClusterSpec struct {
	// Hosts is the inventory. The provider gives one host to each machine.
	//
	// Treat the inventory as fixed after the cluster is created. Size it for
	// the cluster that you want.
	// +kubebuilder:validation:MinItems=1
	Hosts []ProvisionedHost `json:"hosts"`
}

// ProvisionedClusterInitializationStatus reports the initialization state
// that the Cluster API contract requires.
// +kubebuilder:validation:MinProperties=1
type ProvisionedClusterInitializationStatus struct {
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

	// MachineName is the name of the ProvisionedMachine that holds the host.
	MachineName string `json:"machineName"`
}

// ProvisionedClusterStatus defines the observed state of
// ProvisionedCluster.
type ProvisionedClusterStatus struct {
	// Initialization reports the state that the Cluster API contract requires.
	// +optional
	Initialization ProvisionedClusterInitializationStatus `json:"initialization,omitempty,omitzero"`

	// AllocatedHosts reports which machine holds each host.
	//
	// The provider computes this list from the ProvisionedMachine objects on
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

// +kubebuilder:metadata:labels=cluster.x-k8s.io/provider=infrastructure-provisioned
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1alpha1
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=pc
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned"
// +kubebuilder:printcolumn:name="Hosts",type="integer",JSONPath=".status.availableHosts",description="Hosts that no machine holds"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ProvisionedCluster is the Schema for the provisionedclusters API.
type ProvisionedCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProvisionedClusterSpec   `json:"spec,omitempty"`
	Status ProvisionedClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProvisionedClusterList contains a list of ProvisionedCluster.
type ProvisionedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisionedCluster `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &ProvisionedCluster{}, &ProvisionedClusterList{})
}

// GetConditions returns the set of conditions for this object.
func (c *ProvisionedCluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets conditions for this object.
func (c *ProvisionedCluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// HostByIPAddress returns the inventory entry with the given canonical IP
// address.
func (c *ProvisionedCluster) HostByIPAddress(ipAddress string) (ProvisionedHost, bool) {
	for _, host := range c.Spec.Hosts {
		if host.CanonicalIPAddress() == ipAddress {
			return host, true
		}
	}
	return ProvisionedHost{}, false
}

var (
	_ runtime.Object = &ProvisionedCluster{}
	_ runtime.Object = &ProvisionedClusterList{}
)
