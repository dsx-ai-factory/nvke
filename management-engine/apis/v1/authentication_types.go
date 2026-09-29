// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1

// ClusterAuthentication models the JWT configuration.
// It is a deliberate subset of Kubernetes AuthenticationConfiguration, not an
// arbitrary configuration passthrough. Issuer trust and reachability are checked
// when configuring the workload cluster.
type ClusterAuthentication struct {
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=60
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.filter(y, y.issuer.url == x.issuer.url).size() == 1)",message="issuer URLs must be unique"
	JWT []JWTAuthenticator `json:"jwt,omitempty"`
}

// JWTAuthenticator maps a trusted issuer's claims to Kubernetes identities.
type JWTAuthenticator struct {
	Issuer        JWTIssuer        `json:"issuer"`
	ClaimMappings JWTClaimMappings `json:"claimMappings"`
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	ClaimValidationRules []ClaimValidationRule `json:"claimValidationRules,omitempty"`
}

// JWTIssuer identifies an HTTPS issuer and the audiences accepted from it.
type JWTIssuer struct {
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^/?#]+[^#]*$`
	URL string `json:"url"`
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Audiences []Audience `json:"audiences"`
}

// Audience is a nonempty JWT audience identifier.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=256
type Audience string

// JWTClaimMappings maps the token's username and optional groups claims.
type JWTClaimMappings struct {
	Username ClaimMapping `json:"username"`
	// +optional
	Groups *ClaimMapping `json:"groups,omitempty"`
}

// ClaimMapping selects a claim and optionally specifies its identity prefix.
// An empty prefix is meaningful and must remain distinct from an omitted prefix.
type ClaimMapping struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Claim string `json:"claim"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Prefix *string `json:"prefix,omitempty"`
}

// ClaimValidationRule requires a claim to equal a configured string.
type ClaimValidationRule struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Claim string `json:"claim"`
	// +kubebuilder:validation:MaxLength=256
	RequiredValue string `json:"requiredValue"`
}
