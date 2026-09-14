// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// NicoMachineInterface defines one NICo interface attachment request.
// +kubebuilder:validation:XValidation:rule="has(self.subnetID) != has(self.vpcPrefixID)",message="exactly one of subnetID or vpcPrefixID must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.ipAddress) && has(self.subnetID))",message="ipAddress is only supported with vpcPrefixID interfaces"
type NicoMachineInterface struct {
	// SubnetID is the subnet ID for this attachment.
	// Use SubnetID for Ethernet network virtualization.
	// Exactly one of SubnetID or VPCPrefixID must be set.
	// +optional
	// +kubebuilder:validation:MinLength=1
	SubnetID string `json:"subnetID,omitempty"`

	// VPCPrefixID is the VPC prefix ID for this attachment.
	// Use VPCPrefixID for next-generation networking.
	// Exactly one of SubnetID or VPCPrefixID must be set.
	// +optional
	// +kubebuilder:validation:MinLength=1
	VPCPrefixID string `json:"vpcPrefixID,omitempty"`

	// IPAddress is the explicitly requested IP address for this interface.
	// This is only supported for VPCPrefixID-based interfaces. NICo requires the least-significant host bit to be 1.
	// +optional
	IPAddress string `json:"ipAddress,omitempty"`

	// Physical requests this attachment over a physical interface.
	// +optional
	Physical *bool `json:"physical,omitempty"`

	// Device is the device name to use for this attachment.
	// +optional
	Device string `json:"device,omitempty"`

	// DeviceInstance is the device instance index to use for this attachment.
	// +optional
	DeviceInstance *int32 `json:"deviceInstance,omitempty"`
}

// NicoMachineInfiniBandInterface defines one NICo InfiniBand interface attachment request.
type NicoMachineInfiniBandInterface struct {
	// PartitionID is the ID of the Partition the Interface should attach to.
	// +kubebuilder:validation:MinLength=1
	PartitionID string `json:"partitionID"`

	// Device is the name of the InfiniBand device to use.
	// +optional
	Device string `json:"device,omitempty"`

	// DeviceInstance is the index of the device, used to identify which interface card to attach the Partition to.
	// +optional
	DeviceInstance *int32 `json:"deviceInstance,omitempty"`

	// Vendor is the name of the InfiniBand device vendor, optional.
	// +optional
	Vendor string `json:"vendor,omitempty"`

	// IsPhysical specifies whether this Partition should be attached to the Instance over physical interface.
	// +optional
	IsPhysical *bool `json:"isPhysical,omitempty"`

	// VirtualFunctionID must be specified if isPhysical is false.
	// +optional
	VirtualFunctionID *int32 `json:"virtualFunctionID,omitempty"`
}

// NicoMachineNVLinkInterface defines one NICo NVLink interface attachment request.
type NicoMachineNVLinkInterface struct {
	// NVLinkLogicalPartitionID is the ID of the NVLink Logical Partition the Interface should attach to.
	// +kubebuilder:validation:MinLength=1
	NVLinkLogicalPartitionID string `json:"nvLinkLogicalPartitionID"`

	// DeviceInstance is the GPU index for this NVLink interface. Must be non-negative, unique within the request, and within the GPU count exposed by the selected Machine or Instance Type.
	// +optional
	DeviceInstance *int32 `json:"deviceInstance,omitempty"`
}

// NicoMachineSpec defines the desired state of NicoMachine.
// +kubebuilder:validation:XValidation:rule="has(self.instanceTypeID) != has(self.machineID)",message="exactly one of instanceTypeID or machineID must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.infinibandPartitionID) && has(self.infinibandInterfaces))",message="infinibandPartitionID and infinibandInterfaces are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!has(self.infinibandPartitionID) || has(self.instanceTypeID)",message="infinibandPartitionID requires instanceTypeID"
// +kubebuilder:validation:XValidation:rule="!(has(self.nvLinkLogicalPartitionID) && has(self.nvLinkInterfaces))",message="nvLinkLogicalPartitionID and nvLinkInterfaces are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!has(self.nvLinkLogicalPartitionID) || has(self.instanceTypeID)",message="nvLinkLogicalPartitionID requires instanceTypeID"
type NicoMachineSpec struct {
	// VPCID is the VPC where this provider should create the instance.
	// +kubebuilder:validation:MinLength=1
	VPCID string `json:"vpcID"`

	// InstanceTypeID is the instance type ID to provision.
	// Exactly one of InstanceTypeID or MachineID must be set.
	// +optional
	// +kubebuilder:validation:MinLength=1
	InstanceTypeID string `json:"instanceTypeID,omitempty"`

	// Interfaces are the interface attachment requests for the instance.
	// Use subnet-based attachments for Ethernet network virtualization and VPC prefix-based attachments for next-generation networking.
	// +kubebuilder:validation:MinItems=1
	Interfaces []NicoMachineInterface `json:"interfaces"`

	// InfinibandInterfaces requests that one or more InfiniBand Partitions be attached
	// to the instance.
	// +optional
	InfinibandInterfaces []NicoMachineInfiniBandInterface `json:"infinibandInterfaces,omitempty"`

	// InfinibandPartitionID requests that the partition be attached to every active
	// InfiniBand device exposed by InstanceTypeID.
	// Set this instead of InfinibandInterfaces when every device should use the same partition.
	// +optional
	// +kubebuilder:validation:MinLength=1
	InfinibandPartitionID string `json:"infinibandPartitionID,omitempty"`

	// NVLinkInterfaces requests that one or more NVLink Logical Partitions be attached
	// to the instance.
	// +optional
	NVLinkInterfaces []NicoMachineNVLinkInterface `json:"nvLinkInterfaces,omitempty"`

	// NVLinkLogicalPartitionID requests that the logical partition be attached to
	// every active NVLink device exposed by InstanceTypeID.
	// Set this instead of NVLinkInterfaces when every device should use the same logical partition.
	// +optional
	// +kubebuilder:validation:MinLength=1
	NVLinkLogicalPartitionID string `json:"nvLinkLogicalPartitionID,omitempty"`

	// SSHKeyGroupIDs are the allowed SSH key group IDs for Serial over LAN access.
	// +optional
	SSHKeyGroupIDs []string `json:"sshKeyGroupIDs,omitempty"`

	// IpxeScript is the iPXE script used to boot this instance.
	// +optional
	IpxeScript string `json:"ipxeScript,omitempty"`

	// CloudInitInjectHostname prepends hostname directives to the bootstrap cloud-config.
	// +optional
	CloudInitInjectHostname bool `json:"cloudInitInjectHostname,omitempty"`

	// Labels are applied to the instance.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// MachineID is the machine ID for targeted instance creation.
	// Exactly one of InstanceTypeID or MachineID must be set.
	// +optional
	// +kubebuilder:validation:MinLength=1
	MachineID string `json:"machineID,omitempty"`

	// AllowUnhealthyMachine allows targeted instance creation on a machine in Error status.
	// +optional
	AllowUnhealthyMachine bool `json:"allowUnhealthyMachine,omitempty"`

	// ProviderID is set to `nico://<instance-id>` after the NICo instance is created.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ProviderID string `json:"providerID,omitempty"`
}

// NicoMachineInitializationStatus provides observations of the NicoMachine initialization process.
// +kubebuilder:validation:MinProperties=1
type NicoMachineInitializationStatus struct {
	// Provisioned is true once the backing instance has been created.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// NicoMachineStatus defines the observed state of NicoMachine.
type NicoMachineStatus struct {
	// Initialization provides provisioning observations for the backing instance.
	// +optional
	Initialization NicoMachineInitializationStatus `json:"initialization,omitempty,omitzero"`

	// Conditions describes the current machine state.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Ready is true once the backing instance reaches Ready.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// InstanceID is the instance identifier backing this machine.
	// +optional
	InstanceID string `json:"instanceID,omitempty"`

	// MachineID is the machine ID for this machine.
	// +optional
	MachineID string `json:"machineID,omitempty"`

	// SiteID is the observed Forge/Carbide site ID for this machine.
	// +optional
	SiteID string `json:"siteID,omitempty"`

	// SiteName is the observed Forge/Carbide site name for this machine.
	// +optional
	SiteName string `json:"siteName,omitempty"`

	// VPCID is the observed Forge/Carbide VPC ID for this machine.
	// +optional
	VPCID string `json:"vpcID,omitempty"`

	// VPCName is the observed Forge/Carbide VPC name for this machine.
	// +optional
	VPCName string `json:"vpcName,omitempty"`

	// TpmEkPubHash is the TPM EK public hash for this machine.
	// +optional
	TpmEkPubHash string `json:"tpmEkPubHash,omitempty"`

	// Addresses contains addresses observed on the backing instance.
	// +optional
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// FailureDomain is the domain NICo reports the backing instance was placed in.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	FailureDomain string `json:"failureDomain,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=cluster-api,shortName=nicom
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || self.spec.vpcID == oldSelf.spec.vpcID",message="spec.vpcID is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.instanceTypeID) == has(oldSelf.spec.instanceTypeID) && (!has(self.spec.instanceTypeID) || self.spec.instanceTypeID == oldSelf.spec.instanceTypeID))",message="spec.instanceTypeID is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || self.spec.interfaces == oldSelf.spec.interfaces",message="spec.interfaces is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.infinibandInterfaces) == has(oldSelf.spec.infinibandInterfaces) && (!has(self.spec.infinibandInterfaces) || self.spec.infinibandInterfaces == oldSelf.spec.infinibandInterfaces))",message="spec.infinibandInterfaces is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.infinibandPartitionID) == has(oldSelf.spec.infinibandPartitionID) && (!has(self.spec.infinibandPartitionID) || self.spec.infinibandPartitionID == oldSelf.spec.infinibandPartitionID))",message="spec.infinibandPartitionID is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.nvLinkInterfaces) == has(oldSelf.spec.nvLinkInterfaces) && (!has(self.spec.nvLinkInterfaces) || self.spec.nvLinkInterfaces == oldSelf.spec.nvLinkInterfaces))",message="spec.nvLinkInterfaces is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.nvLinkLogicalPartitionID) == has(oldSelf.spec.nvLinkLogicalPartitionID) && (!has(self.spec.nvLinkLogicalPartitionID) || self.spec.nvLinkLogicalPartitionID == oldSelf.spec.nvLinkLogicalPartitionID))",message="spec.nvLinkLogicalPartitionID is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.sshKeyGroupIDs) == has(oldSelf.spec.sshKeyGroupIDs) && (!has(self.spec.sshKeyGroupIDs) || self.spec.sshKeyGroupIDs == oldSelf.spec.sshKeyGroupIDs))",message="spec.sshKeyGroupIDs is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.ipxeScript) == has(oldSelf.spec.ipxeScript) && (!has(self.spec.ipxeScript) || self.spec.ipxeScript == oldSelf.spec.ipxeScript))",message="spec.ipxeScript is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.cloudInitInjectHostname) == has(oldSelf.spec.cloudInitInjectHostname) && (!has(self.spec.cloudInitInjectHostname) || self.spec.cloudInitInjectHostname == oldSelf.spec.cloudInitInjectHostname))",message="spec.cloudInitInjectHostname is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.labels) == has(oldSelf.spec.labels) && (!has(self.spec.labels) || self.spec.labels == oldSelf.spec.labels))",message="spec.labels is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.machineID) == has(oldSelf.spec.machineID) && (!has(self.spec.machineID) || self.spec.machineID == oldSelf.spec.machineID))",message="spec.machineID is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.allowUnhealthyMachine) == has(oldSelf.spec.allowUnhealthyMachine) && (!has(self.spec.allowUnhealthyMachine) || self.spec.allowUnhealthyMachine == oldSelf.spec.allowUnhealthyMachine))",message="spec.allowUnhealthyMachine is immutable after providerID is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.providerID) || (has(self.spec.providerID) && self.spec.providerID == oldSelf.spec.providerID)",message="spec.providerID is immutable after it is set"
// +kubebuilder:printcolumn:name="Available",type="string",JSONPath=".status.conditions[?(@.type=='Available')].status",description="Current machine infrastructure availability"
// +kubebuilder:printcolumn:name="Synced",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status",description="Machine infrastructure reconciled"
// +kubebuilder:printcolumn:name="Provisioned",type="string",JSONPath=".status.initialization.provisioned",description="Initial machine infrastructure provisioning completed"
// +kubebuilder:printcolumn:name="Instance",type="string",JSONPath=".status.instanceID",description="NICo instance"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NicoMachine is the Schema for the nicomachines API. Once ProviderID is
// assigned, the spec is immutable because the controller does not reconcile
// these create-time settings onto an existing instance.
type NicoMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NicoMachineSpec   `json:"spec,omitempty"`
	Status NicoMachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NicoMachineList contains a list of NicoMachine.
type NicoMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NicoMachine `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &NicoMachine{}, &NicoMachineList{})
}

// GetConditions returns the set of conditions for this object.
func (m *NicoMachine) GetConditions() []metav1.Condition {
	return m.Status.Conditions
}

// GetCondition returns the condition with the given type, or Unknown when it
// has not been reported.
func (m *NicoMachine) GetCondition(conditionType string) metav1.Condition {
	condition := apimeta.FindStatusCondition(m.Status.Conditions, conditionType)
	if condition == nil {
		return metav1.Condition{Type: conditionType, Status: metav1.ConditionUnknown, Reason: UnknownReason}
	}
	return *condition
}

// SetConditions sets conditions for this object.
func (m *NicoMachine) SetConditions(conditions []metav1.Condition) {
	m.Status.Conditions = conditions
}
