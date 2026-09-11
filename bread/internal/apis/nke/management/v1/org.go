// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OrgMemberRole is the role a member holds in an org: admin or viewer. The role, like the
// org, comes from Org.spec.members and never from a token claim (docs/PROPOSAL-FRONT-DOOR.md D1, D7).
//
// +kubebuilder:validation:Enum=admin;viewer
type OrgMemberRole string

const (
	// OrgMemberRoleAdmin may read and write the org's tenant kinds (Role nke-admin).
	OrgMemberRoleAdmin OrgMemberRole = "admin"
	// OrgMemberRoleViewer may only get, list and watch them (Role nke-viewer).
	OrgMemberRoleViewer OrgMemberRole = "viewer"
)

// Org binds external identities to an org (D7). It is cluster-scoped and never tenant-facing:
// the API server reads it through its one read-only informer to resolve membership, and the
// onboarding controller reconciles it into the org's namespace, ServiceAccounts, Roles and
// RoleBindings. metadata.name is the opaque, immutable org id; the namespace is `org-<id>`,
// so the id is a DNS label of at most 59 characters (63 minus the prefix).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=orgs,singular=org,scope=Cluster
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 59 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name is the org id: a DNS-1123 label of at most 59 characters"
// +kubebuilder:printcolumn:name="DISPLAY-NAME",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="NAMESPACE",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type Org struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the operator's intent: the human name and the members.
	// +kubebuilder:validation:Required
	Spec OrgSpec `json:"spec"`

	// Status is what the onboarding controller last observed.
	// +kubebuilder:validation:Optional
	Status OrgStatus `json:"status,omitempty"`
}

// OrgList is a list of Orgs.
//
// +kubebuilder:object:root=true
type OrgList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Org `json:"items"`
}

// OrgSpec holds the mutable display name and the members.
type OrgSpec struct {
	// DisplayName is the mutable human name, shown in listings only; renaming it touches
	// neither RBAC nor any stored object.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	DisplayName string `json:"displayName,omitempty"`

	// Members are the identities that may act in the org and the role each holds. A subject
	// is identified by (subject, issuer); the same pair appears at most once.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=subject
	// +listMapKey=issuer
	Members []OrgMember `json:"members,omitempty"`
}

// OrgMember is one identity in an org: the subject as its issuer states it, and its role.
type OrgMember struct {
	// Subject is the external subject exactly as the IdP issues it (the `sub` claim).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Subject string `json:"subject"`

	// Issuer is the token issuer that vouches for Subject.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Issuer string `json:"issuer"`

	// Role is admin or viewer.
	// +kubebuilder:validation:Required
	Role OrgMemberRole `json:"role"`
}

// OrgStatus is what the onboarding controller reports.
type OrgStatus struct {
	// Namespace is the org's namespace once onboarding has created it.
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`
}
