// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/nke/controller/internal/config"
)

const (
	authenticationConfigurationKind        = "AuthenticationConfiguration"
	defaultOIDCAuthConfigPath              = "/etc/kubernetes/oidc/auth-config.yaml"
	defaultPrerequisiteFileMode     uint32 = 0o600
	defaultPrerequisiteFileOwner           = "root:root"

	// OIDC username identity is the SPIRE JWT-SVID subject (the bare SPIFFE ID),
	// which is what rbac.yaml binds as kind:User with no prefix.
	oidcUsernameClaim  = "sub"
	oidcUsernamePrefix = ""
)

type OIDCAuthenticationConfig = apiserverv1.AuthenticationConfiguration

type OIDCIssuer = apiserverv1.Issuer

type OIDCProvider = apiserverv1.JWTAuthenticator

type OIDCClaimMappings = apiserverv1.ClaimMappings

type OIDCPrefixedClaimOrExpression = apiserverv1.PrefixedClaimOrExpression

type OIDCClaimOrExpression = apiserverv1.ClaimOrExpression

type OIDCExtraMapping = apiserverv1.ExtraMapping

type OIDCClaimValidationRule = apiserverv1.ClaimValidationRule

type OIDCUserValidationRule = apiserverv1.UserValidationRule

// GenerateAPIServerAuthenticationConfig renders the AuthenticationConfiguration
// document into the on-node File the greenfield bootstrap stages and the
// brownfield rollout delivers to each control-plane node.
func GenerateAPIServerAuthenticationConfig(config *OIDCAuthenticationConfig) (File, error) {
	content, err := renderAuthenticationConfig(config)
	if err != nil {
		return File{}, err
	}

	return File{
		Path:    defaultOIDCAuthConfigPath,
		Content: content,
		Mode:    defaultPrerequisiteFileMode,
		Owner:   defaultPrerequisiteFileOwner,
	}, nil
}

func renderAuthenticationConfig(config *OIDCAuthenticationConfig) ([]byte, error) {
	if config == nil {
		return nil, fmt.Errorf("rendering AuthenticationConfiguration requires a non-nil config")
	}
	if len(config.JWT) == 0 {
		return nil, fmt.Errorf("rendering AuthenticationConfiguration requires at least one JWT provider")
	}

	doc := *config
	doc.APIVersion = apiserverv1.SchemeGroupVersion.String()
	doc.Kind = authenticationConfigurationKind

	rendered, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshaling AuthenticationConfiguration: %w", err)
	}

	return rendered, nil
}

// BuildOIDCAuthenticationConfig expands the site OIDC config plus a resolved CA
// bundle into a single-provider auth config model. The username claim and prefix
// are constants; the empty prefix is emitted explicitly (Prefix: ptr("")) to mean
// "no prefix". It is shared by the greenfield bootstrap path and the brownfield
// enablement rollout so both render byte-identical AuthenticationConfiguration.
func BuildOIDCAuthenticationConfig(clusterName string, oidc config.NkeOIDCAuth, caBundle []byte) (*OIDCAuthenticationConfig, error) {
	if clusterName == "" {
		return nil, fmt.Errorf("cluster name is required when OIDC auth is enabled")
	}
	if oidc.IssuerURL == "" {
		return nil, fmt.Errorf("OIDC issuer URL is required when OIDC auth is enabled")
	}

	usernamePrefix := oidcUsernamePrefix

	var discoveryURL *string
	if oidc.DiscoveryURL != "" {
		d := oidc.DiscoveryURL
		discoveryURL = &d
	}

	authConfig := &OIDCAuthenticationConfig{
		JWT: []OIDCProvider{
			{
				Issuer: OIDCIssuer{
					URL:          oidc.IssuerURL,
					DiscoveryURL: discoveryURL,
					Audiences:    []string{clusterName},
				},
				ClaimMappings: OIDCClaimMappings{
					Username: OIDCPrefixedClaimOrExpression{
						Claim:  oidcUsernameClaim,
						Prefix: &usernamePrefix,
					},
				},
			},
		},
	}
	if len(caBundle) != 0 {
		authConfig.JWT[0].Issuer.CertificateAuthority = string(caBundle)
	}
	return authConfig, nil
}

// ReadOIDCCABundle reads the issuer CA bundle (PEM) from the operator-managed
// Secret named by ref, using a controller-runtime client. It fails closed: an
// incomplete reference, a missing Secret, a missing key, or an empty value all
// return an error rather than yielding an empty bundle.
func ReadOIDCCABundle(ctx context.Context, c client.Client, ref config.OIDCCABundleSecretRef) ([]byte, error) {
	if ref.Namespace == "" || ref.Name == "" || ref.Key == "" {
		return nil, fmt.Errorf("OIDC CA bundle secret reference is incomplete (namespace=%q name=%q key=%q)", ref.Namespace, ref.Name, ref.Key)
	}
	if c == nil {
		return nil, fmt.Errorf("kubernetes client is required to read OIDC CA bundle secret")
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
		return nil, fmt.Errorf("getting OIDC CA bundle secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	raw, ok := secret.Data[ref.Key]
	if !ok {
		return nil, fmt.Errorf("OIDC CA bundle secret %s/%s missing key %q", ref.Namespace, ref.Name, ref.Key)
	}
	caBundle := bytes.TrimSpace(raw)
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("OIDC CA bundle secret %s/%s key %q is empty", ref.Namespace, ref.Name, ref.Key)
	}
	return caBundle, nil
}
