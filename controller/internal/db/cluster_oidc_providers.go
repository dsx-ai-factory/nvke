// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"maps"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

// ClusterOIDCProvider is one pinned provider. The database owns this shape
// rather than storing controllerconfig.OIDCProvider, so a stored snapshot
// stays readable when the configuration file's schema changes.
type ClusterOIDCProvider struct {
	Name           string            `json:"name"`
	IssuerURL      string            `json:"issuerURL"`
	Audiences      []string          `json:"audiences"`
	UsernameClaim  string            `json:"usernameClaim"`
	UsernamePrefix string            `json:"usernamePrefix"`
	GroupsClaim    string            `json:"groupsClaim,omitempty"`
	GroupsPrefix   string            `json:"groupsPrefix,omitempty"`
	RequiredClaims map[string]string `json:"requiredClaims,omitempty"`
	CABundle       string            `json:"caBundle"`
	DiscoveryURL   string            `json:"discoveryURL,omitempty"`
}

// ClusterOIDCProvidersFromConfig converts the fleet-wide providers from
// nke-controller-config into the stored shape. The result is empty rather than
// nil when there are none, and shares no slice or map with providers.
func ClusterOIDCProvidersFromConfig(providers []controllerconfig.OIDCProvider) []ClusterOIDCProvider {
	out := make([]ClusterOIDCProvider, 0, len(providers))
	for _, provider := range providers {
		out = append(out, ClusterOIDCProvider{
			Name:           provider.Name,
			IssuerURL:      provider.IssuerURL,
			Audiences:      append([]string(nil), provider.Audiences...),
			UsernameClaim:  provider.UsernameClaim,
			UsernamePrefix: provider.UsernamePrefix,
			GroupsClaim:    provider.GroupsClaim,
			GroupsPrefix:   provider.GroupsPrefix,
			RequiredClaims: maps.Clone(provider.RequiredClaims),
			CABundle:       provider.CABundle,
			DiscoveryURL:   provider.DiscoveryURL,
		})
	}
	return out
}

// ClusterOIDCProvidersToConfig is the inverse of ClusterOIDCProvidersFromConfig,
// and likewise shares no slice or map with providers.
func ClusterOIDCProvidersToConfig(providers []ClusterOIDCProvider) []controllerconfig.OIDCProvider {
	out := make([]controllerconfig.OIDCProvider, 0, len(providers))
	for _, provider := range providers {
		out = append(out, controllerconfig.OIDCProvider{
			Name:           provider.Name,
			IssuerURL:      provider.IssuerURL,
			Audiences:      append([]string(nil), provider.Audiences...),
			UsernameClaim:  provider.UsernameClaim,
			UsernamePrefix: provider.UsernamePrefix,
			GroupsClaim:    provider.GroupsClaim,
			GroupsPrefix:   provider.GroupsPrefix,
			RequiredClaims: maps.Clone(provider.RequiredClaims),
			CABundle:       provider.CABundle,
			DiscoveryURL:   provider.DiscoveryURL,
		})
	}
	return out
}
