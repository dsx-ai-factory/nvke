// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"fmt"

	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

const (
	maxCombinedJWTAuthenticators = 64
)

// BuildCombinedAuthenticationConfig combines the optional shared management
// provider with operator-configured providers in one AuthenticationConfiguration.
func BuildCombinedAuthenticationConfig(
	clusterName string,
	managementOIDC controllerconfig.ManagementOIDCAuthConfig,
	sharedCABundle []byte,
	providers []controllerconfig.OIDCProvider,
	operatorPolicy OperatorJWTPolicy,
) (*apiserverv1.AuthenticationConfiguration, error) {
	providerCount := len(providers)
	if managementOIDC.Enabled {
		providerCount++
	}

	if providerCount == 0 {
		return nil, fmt.Errorf("at least one shared or operator OIDC provider is required")
	}
	if providerCount > maxCombinedJWTAuthenticators {
		return nil, fmt.Errorf(
			"combined authentication configuration has %d JWT providers, more than the Kubernetes limit of %d",
			providerCount,
			maxCombinedJWTAuthenticators,
		)
	}

	combined := &apiserverv1.AuthenticationConfiguration{
		JWT: make([]apiserverv1.JWTAuthenticator, 0, providerCount),
	}
	if managementOIDC.Enabled {
		shared, err := BuildOIDCAuthenticationConfig(clusterName, config.NvkeOIDCAuth{
			Enabled:      true,
			IssuerURL:    managementOIDC.IssuerURL,
			DiscoveryURL: managementOIDC.DiscoveryURL,
		}, sharedCABundle)
		if err != nil {
			return nil, fmt.Errorf("building shared provider: %w", err)
		}
		combined.JWT = append(combined.JWT, shared.JWT...)

		if operatorPolicy.ManagementJWTTrustDomain == "" {
			if len(providers) == 0 {
				return nil, fmt.Errorf("building shared provider: management JWT trust domain is required when management OIDC is enabled")
			}
			return nil, fmt.Errorf(
				"building operator provider at index 0 (%q): management JWT trust domain is required when management OIDC is enabled",
				providers[0].Name,
			)
		}
		if len(providers) == 0 {
			if err := validateManagementJWTTrustDomain(operatorPolicy.ManagementJWTTrustDomain); err != nil {
				return nil, fmt.Errorf("building shared provider: %w", err)
			}
		}
	}

	for i, provider := range providers {
		if managementOIDC.Enabled && provider.IssuerURL == managementOIDC.IssuerURL {
			return nil, fmt.Errorf("building operator provider at index %d (%q): issuer URL %q duplicates the shared provider", i, provider.Name, provider.IssuerURL)
		}
		authenticator, err := BuildOperatorJWTAuthenticator(provider, operatorPolicy)
		if err != nil {
			return nil, fmt.Errorf("building operator provider at index %d (%q): %w", i, provider.Name, err)
		}
		combined.JWT = append(combined.JWT, authenticator)
	}

	return combined, nil
}
