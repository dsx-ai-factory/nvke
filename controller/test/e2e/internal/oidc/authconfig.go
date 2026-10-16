// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	"sigs.k8s.io/yaml"
)

const (
	AuthConfigDir  = "/etc/kubernetes/oidc"
	AuthConfigPath = AuthConfigDir + "/auth-config.yaml"
	authConfigKind = "AuthenticationConfiguration"
)

// ReservedPrefixes are the identity namespaces every fleet-wide provider's
// userValidationRules must refuse. It is a copy of
// controllerconfig.ReservedIdentityPrefixes kept on purpose: a prefix dropped
// there must fail the acceptance test, not quietly drop out of it too.
func ReservedPrefixes() []string {
	return []string{"system:", "kubeadm:", "nke:", "nvke:", "nvidia:"}
}

// ExpectedAuthentication is what a cluster's AuthenticationConfiguration must
// hold: the shared management provider first when Shared is set, then
// Providers in order, each refusing the reserved prefixes and ReservedUsers.
// Shared is the shared provider's issuer, and nil when the cluster has none.
type ExpectedAuthentication struct {
	ClusterName   string
	Shared        *apiserverv1.Issuer
	Providers     []Provider
	ReservedUsers []string
}

// CheckAuthenticationConfig decodes one control-plane node's
// AuthenticationConfiguration and reports the first way it differs from want.
func CheckAuthenticationConfig(data []byte, want ExpectedAuthentication) error {
	var config apiserverv1.AuthenticationConfiguration
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("decode AuthenticationConfiguration: %w", err)
	}
	if want := apiserverv1.SchemeGroupVersion.String(); config.APIVersion != want || config.Kind != authConfigKind {
		return fmt.Errorf("document is %s %s, want %s %s", config.APIVersion, config.Kind, want, authConfigKind)
	}
	count := len(want.Providers)
	if want.Shared != nil {
		count++
	}
	if len(config.JWT) != count {
		return fmt.Errorf("has %d JWT authenticators, want %d", len(config.JWT), count)
	}
	authenticators := config.JWT
	if want.Shared != nil {
		if err := checkSharedAuthenticator(authenticators[0], *want.Shared, want.ClusterName); err != nil {
			return fmt.Errorf("shared provider: %w", err)
		}
		authenticators = authenticators[1:]
	}
	for i, provider := range want.Providers {
		if err := checkProviderAuthenticator(authenticators[i], provider, want.ReservedUsers); err != nil {
			return fmt.Errorf("provider %q: %w", provider.Name, err)
		}
	}
	return nil
}

func checkSharedAuthenticator(authenticator apiserverv1.JWTAuthenticator, want apiserverv1.Issuer, clusterName string) error {
	issuer := authenticator.Issuer
	switch {
	case issuer.URL != want.URL:
		return fmt.Errorf("issuer URL is %q, want %q", issuer.URL, want.URL)
	case deref(issuer.DiscoveryURL) != deref(want.DiscoveryURL):
		return fmt.Errorf("discovery URL is %q, want %q", deref(issuer.DiscoveryURL), deref(want.DiscoveryURL))
	case strings.TrimSpace(issuer.CertificateAuthority) != strings.TrimSpace(want.CertificateAuthority):
		return errors.New("certificate authority differs from the shared provider's CA bundle")
	case !slices.Equal(issuer.Audiences, []string{clusterName}):
		return fmt.Errorf("audiences are %v, want [%s]", issuer.Audiences, clusterName)
	}
	return checkClaim("username", authenticator.ClaimMappings.Username, "sub", "")
}

func checkProviderAuthenticator(authenticator apiserverv1.JWTAuthenticator, provider Provider, reservedUsers []string) error {
	issuer := authenticator.Issuer
	switch {
	case issuer.URL != provider.IssuerURL:
		return fmt.Errorf("issuer URL is %q, want %q", issuer.URL, provider.IssuerURL)
	case !slices.Equal(issuer.Audiences, provider.Audiences):
		return fmt.Errorf("audiences are %v, want %v", issuer.Audiences, provider.Audiences)
	case strings.TrimSpace(issuer.CertificateAuthority) != strings.TrimSpace(provider.CABundle):
		return errors.New("certificate authority differs from the provider's caBundle")
	}
	if err := checkClaim("username", authenticator.ClaimMappings.Username, provider.UsernameClaim, provider.UsernamePrefix); err != nil {
		return err
	}
	if provider.GroupsClaim == "" {
		if groups := authenticator.ClaimMappings.Groups; groups.Claim != "" || groups.Expression != "" {
			return fmt.Errorf("groups are mapped by claim %q and expression %q, want no mapping", groups.Claim, groups.Expression)
		}
	} else if err := checkClaim("groups", authenticator.ClaimMappings.Groups, provider.GroupsClaim, provider.GroupsPrefix); err != nil {
		return err
	}

	required := make(map[string]string, len(authenticator.ClaimValidationRules))
	for _, rule := range authenticator.ClaimValidationRules {
		required[rule.Claim] = rule.RequiredValue
	}
	if len(required) != len(authenticator.ClaimValidationRules) || !maps.Equal(required, provider.RequiredClaims) {
		return fmt.Errorf("claim validation rules are %v, want %v", required, provider.RequiredClaims)
	}

	expressions := make([]string, 0, len(authenticator.UserValidationRules))
	for _, rule := range authenticator.UserValidationRules {
		expressions = append(expressions, rule.Expression)
	}
	all := strings.Join(expressions, "\n")
	for _, reserved := range append(ReservedPrefixes(), reservedUsers...) {
		if !strings.Contains(all, strconv.Quote(reserved)) {
			return fmt.Errorf("user validation rules do not refuse %q", reserved)
		}
	}
	return nil
}

func checkClaim(name string, got apiserverv1.PrefixedClaimOrExpression, claim, prefix string) error {
	if got.Claim != claim {
		return fmt.Errorf("%s claim is %q, want %q", name, got.Claim, claim)
	}
	if got.Prefix == nil || *got.Prefix != prefix {
		return fmt.Errorf("%s prefix is %s, want %q", name, describePrefix(got.Prefix), prefix)
	}
	return nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func describePrefix(prefix *string) string {
	if prefix == nil {
		return "unset"
	}
	return strconv.Quote(*prefix)
}
