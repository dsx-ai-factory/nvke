// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

// ResourceName is a Kubernetes DNS subdomain name, not a provider identifier.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=253
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
type ResourceName string

// MachineImageReference names a cluster-scoped MachineImage. Only the name is
// selectable
type MachineImageReference struct {
	Name ResourceName `json:"name"`
}

// MachineInit is the first-boot configuration a Release pairs with its image.
// It lives on the Release rather than the MachineImage so one image can back
// several Releases whose initialization differs.
type MachineInit struct {
	// UserDataTemplate is rendered with machine-specific values, such as the
	// hostname and the selected Kubernetes version, and the result is stored in
	// the machine's CAPI bootstrap Secret. It must begin with a cloud-init
	// header.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	// +kubebuilder:validation:XValidation:rule="self.trim() != ''",message="userDataTemplate must not be blank"
	// +kubebuilder:validation:XValidation:rule="self.startsWith('#cloud-config\\n') || self.startsWith('#cloud-config\\r\\n') || self.startsWith('#!') || self.startsWith('#include') || self.startsWith('#cloud-boothook') || self.startsWith('#part-handler') || self.startsWith('#cloud-config-archive') || self.startsWith('Content-Type:')",message="userDataTemplate must begin with a cloud-init header"
	// +kubebuilder:validation:XValidation:rule="self.split('{{').size() == self.split('}}').size()",message="userDataTemplate has unbalanced Go template delimiters"
	UserDataTemplate string `json:"userDataTemplate"`
}

// ReleaseLifecycle is how an operator manages an already-published Release
// without altering what that Release provisions.
type ReleaseLifecycle struct {
	// Deprecated allows resources already depending on the Release to keep
	// using it, but rejects new cluster or node pool creation referencing it.
	// +optional
	// +kubebuilder:default=false
	Deprecated bool `json:"deprecated"`
}
