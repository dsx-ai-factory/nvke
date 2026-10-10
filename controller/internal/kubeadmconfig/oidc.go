// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	corev1 "k8s.io/api/core/v1"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
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

	// clusterInfoPath is the kube-public/cluster-info ConfigMap that
	// `kubeadm join` reads to pin the cluster CA. The read happens before the
	// joining node holds any credential, so it is necessarily anonymous.
	clusterInfoPath = "/api/v1/namespaces/kube-public/configmaps/cluster-info"

	operatorUsernameValidationMessage = "username uses a reserved prefix"
	operatorGroupsValidationMessage   = "groups use a reserved prefix"
)

// publicInfoPaths are the non-resource URLs upstream's own bootstrap policy
// already exposes to system:anonymous: the system:public-info-viewer
// ClusterRole is bound to both system:authenticated and system:unauthenticated
// and grants exactly /healthz, /livez, /readyz, /version and /version/.
//
// The first three are also what kubeadm points the kube-apiserver static Pod's
// startup, liveness and readiness probes at. /version is here because RBAC
// already allowed it and unauthenticated version discovery is load-bearing off
// the probe path -- external load-balancer health checks and client-go
// discovery clients built without a credential both hit it. Dropping it turns
// those into a bare 401 with nothing in the audit log to explain why, which is
// a poor trade for no security gain: the endpoint leaks only the server's
// version string, which the TLS handshake and the probe endpoints already
// imply. Conditions match the request path exactly, so /version and /version/
// need separate entries.
func publicInfoPaths() []string {
	return []string{"/livez", "/readyz", "/healthz", "/version", "/version/"}
}

// anonymousProbeAuthConfig denies anonymous requests everywhere except the
// paths upstream's default RBAC already exposes to system:anonymous (see
// publicInfoPaths -- the probe endpoints plus /version), and the cluster-info
// ConfigMap every control-plane and worker join reads before it has a
// credential.
//
// This is deliberately not `--anonymous-auth=false`. The two are mutually
// exclusive -- see the `Anonymous` field on AuthenticationConfiguration:
// "If present --anonymous-auth must not be set" -- and the flag on its own is
// what regressed cluster bringup. The kubelet issues those probes as plain
// httpGet with no credential, and the kube-apiserver image is distroless, so
// there is no shell or curl to swap in an authenticating exec probe: closing
// anonymous access outright takes the probes down with it and the kubelet
// restarts the API server in a loop. Restricting anonymous access to the probe
// paths reaches the posture the flag was meant to express while leaving the
// control plane able to report its own health.
//
// clusterInfoPath is on the list for the same reason the probes are: bootstrap
// token discovery reads it unauthenticated, so denying it fails every join at
// preflight with "couldn't validate the identity of the API Server: failed to
// request the cluster-info ConfigMap: Unauthorized". Allowing it gives up
// nothing -- the ConfigMap is public by design (cluster CA and endpoint, JWS
// signed), and kubeadm's own kubeadm:bootstrap-signer-clusterinfo RoleBinding
// already grants system:anonymous the read.
func anonymousProbeAuthConfig() *apiserverv1.AnonymousAuthConfig {
	paths := publicInfoPaths()
	conditions := make([]apiserverv1.AnonymousAuthCondition, 0, len(paths)+1)
	for _, path := range paths {
		conditions = append(conditions, apiserverv1.AnonymousAuthCondition{Path: path})
	}
	conditions = append(conditions, apiserverv1.AnonymousAuthCondition{Path: clusterInfoPath})

	return &apiserverv1.AnonymousAuthConfig{Enabled: true, Conditions: conditions}
}

// apiServerAuthenticationConfig synthesizes an anonymous-only document when
// OIDC is off. Every control-plane node gets an AuthenticationConfiguration
// now, not just the OIDC ones, because the anonymous restriction lives in it.
//
// An OIDC config passes through untouched: renderAuthenticationConfig folds the
// anonymous section into whatever it is handed, so the greenfield and
// brownfield paths converge there rather than here.
func apiServerAuthenticationConfig(oidc *OIDCAuthenticationConfig) *OIDCAuthenticationConfig {
	if oidc != nil {
		return oidc
	}

	return &OIDCAuthenticationConfig{
		JWT:       []OIDCProvider{},
		Anonymous: anonymousProbeAuthConfig(),
	}
}

// BuildAnonymousAuthenticationConfig returns the bootstrap anonymous-only
// configuration, or nil when kubernetesVersion does not support it.
func BuildAnonymousAuthenticationConfig(kubernetesVersion string) *OIDCAuthenticationConfig {
	if !supportsAnonymousAuthConfig(kubernetesVersion) {
		return nil
	}
	return apiServerAuthenticationConfig(nil)
}

type OIDCAuthenticationConfig = apiserverv1.AuthenticationConfiguration

type OIDCIssuer = apiserverv1.Issuer

type OIDCProvider = apiserverv1.JWTAuthenticator

type OIDCClaimMappings = apiserverv1.ClaimMappings

type OIDCPrefixedClaimOrExpression = apiserverv1.PrefixedClaimOrExpression

type OIDCClaimOrExpression = apiserverv1.ClaimOrExpression

type OIDCExtraMapping = apiserverv1.ExtraMapping

type OIDCClaimValidationRule = apiserverv1.ClaimValidationRule

type OIDCUserValidationRule = apiserverv1.UserValidationRule

// OperatorJWTPolicy carries platform-owned identity rules that are not part of
// an operator's provider configuration. ManagementJWTTrustDomain must be a
// bare SPIFFE trust domain. ReservedSubjects contains privileged identities
// reissued by a token exchange under subjects that the trust domain cannot
// predict.
type OperatorJWTPolicy struct {
	ManagementJWTTrustDomain string
	ReservedSubjects         []string
}

// GenerateAPIServerAuthenticationConfig renders the AuthenticationConfiguration
// document into the on-node File the greenfield bootstrap stages and the
// brownfield rollout delivers to each control-plane node.
func GenerateAPIServerAuthenticationConfig(config *OIDCAuthenticationConfig, kubernetesVersion string) (File, error) {
	content, err := renderAuthenticationConfig(config, kubernetesVersion)
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

func renderAuthenticationConfig(config *OIDCAuthenticationConfig, kubernetesVersion string) ([]byte, error) {
	if config == nil {
		return nil, fmt.Errorf("rendering AuthenticationConfiguration requires a non-nil config")
	}
	// An anonymous-only document is valid: it is what a non-OIDC cluster gets,
	// and it carries the anonymous probe restriction.
	if len(config.JWT) == 0 && config.Anonymous == nil {
		return nil, fmt.Errorf("rendering AuthenticationConfiguration requires at least one JWT provider")
	}

	doc := *config
	doc.APIVersion = apiserverv1.SchemeGroupVersion.String()
	doc.Kind = authenticationConfigurationKind
	// The single choke point for both the greenfield bootstrap and the
	// brownfield OIDC rollout, so every rendered document carries the anonymous
	// restriction and the two paths stay byte-identical.
	//
	// Below the supported floor the restriction is dropped rather than rendered
	// into a document kube-apiserver cannot parse: an OIDC cluster on 1.29 or
	// 1.31 keeps exactly the config it had before this restriction existed.
	if doc.Anonymous == nil && supportsAnonymousAuthConfig(kubernetesVersion) {
		doc.Anonymous = anonymousProbeAuthConfig()
	}

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

// BuildOperatorJWTAuthenticator maps one validated operator provider to the
// kube-apiserver authentication API. Its reserved-prefix rules run against the
// final username and groups after claim mapping. Policy can reserve a complete
// SPIFFE trust domain and exact subjects used by a token exchange.
func BuildOperatorJWTAuthenticator(
	provider controllerconfig.OIDCProvider,
	policy OperatorJWTPolicy,
) (apiserverv1.JWTAuthenticator, error) {
	usernamePrefix := provider.UsernamePrefix
	authenticator := apiserverv1.JWTAuthenticator{
		Issuer: apiserverv1.Issuer{
			URL:                  provider.IssuerURL,
			Audiences:            append([]string(nil), provider.Audiences...),
			CertificateAuthority: provider.CABundle,
			AudienceMatchPolicy:  apiserverv1.AudienceMatchPolicyMatchAny,
		},
		ClaimMappings: apiserverv1.ClaimMappings{
			Username: apiserverv1.PrefixedClaimOrExpression{
				Claim:  provider.UsernameClaim,
				Prefix: &usernamePrefix,
			},
		},
	}

	if provider.GroupsClaim != "" {
		groupsPrefix := provider.GroupsPrefix
		authenticator.ClaimMappings.Groups = apiserverv1.PrefixedClaimOrExpression{
			Claim:  provider.GroupsClaim,
			Prefix: &groupsPrefix,
		}
	}

	requiredClaimNames := make([]string, 0, len(provider.RequiredClaims))
	for claim := range provider.RequiredClaims {
		requiredClaimNames = append(requiredClaimNames, claim)
	}
	sort.Strings(requiredClaimNames)

	if len(requiredClaimNames) > 0 {
		authenticator.ClaimValidationRules = make([]apiserverv1.ClaimValidationRule, 0, len(requiredClaimNames))
		for _, claim := range requiredClaimNames {
			authenticator.ClaimValidationRules = append(authenticator.ClaimValidationRules, apiserverv1.ClaimValidationRule{
				Claim:         claim,
				RequiredValue: provider.RequiredClaims[claim],
			})
		}
	}

	if err := injectReservedNamespaceValidation(&authenticator, policy); err != nil {
		return apiserverv1.JWTAuthenticator{}, err
	}

	return authenticator, nil
}

func injectReservedNamespaceValidation(authenticator *apiserverv1.JWTAuthenticator, policy OperatorJWTPolicy) error {
	reserved := controllerconfig.ReservedIdentityPrefixes()
	usernameChecks := make([]string, 0, len(reserved))
	groupChecks := make([]string, 0, len(reserved))
	for _, prefix := range reserved {
		quoted := strconv.Quote(prefix)
		usernameChecks = append(usernameChecks, fmt.Sprintf("!user.username.startsWith(%s)", quoted))
		groupChecks = append(groupChecks, fmt.Sprintf("!group.startsWith(%s)", quoted))
	}
	rules := []apiserverv1.UserValidationRule{
		{
			Expression: strings.Join(usernameChecks, " && "),
			Message:    operatorUsernameValidationMessage,
		},
		{
			Expression: fmt.Sprintf("user.groups.all(group, %s)", strings.Join(groupChecks, " && ")),
			Message:    operatorGroupsValidationMessage,
		},
	}

	if policy.ManagementJWTTrustDomain != "" {
		if err := validateManagementJWTTrustDomain(policy.ManagementJWTTrustDomain); err != nil {
			return err
		}

		reservedPrefix := strconv.Quote("spiffe://" + policy.ManagementJWTTrustDomain + "/")
		rules = append(rules,
			apiserverv1.UserValidationRule{
				Expression: fmt.Sprintf("!user.username.startsWith(%s)", reservedPrefix),
				Message:    operatorUsernameValidationMessage,
			},
			apiserverv1.UserValidationRule{
				Expression: fmt.Sprintf("user.groups.all(group, !group.startsWith(%s))", reservedPrefix),
				Message:    operatorGroupsValidationMessage,
			},
		)
	}

	for i, subject := range policy.ReservedSubjects {
		if strings.TrimSpace(subject) == "" {
			return fmt.Errorf("reserved subject at index %d must not be empty", i)
		}

		quoted := strconv.Quote(subject)
		rules = append(rules,
			apiserverv1.UserValidationRule{
				Expression: fmt.Sprintf("user.username != %s", quoted),
				Message:    operatorUsernameValidationMessage,
			},
			apiserverv1.UserValidationRule{
				Expression: fmt.Sprintf("user.groups.all(group, group != %s)", quoted),
				Message:    operatorGroupsValidationMessage,
			},
		)
	}

	authenticator.UserValidationRules = append(authenticator.UserValidationRules, rules...)
	return nil
}

func validateManagementJWTTrustDomain(value string) error {
	if value == "" {
		return fmt.Errorf("management JWT trust domain is required when management OIDC is enabled")
	}

	trustDomain, err := spiffeid.TrustDomainFromString(value)
	if err != nil {
		return fmt.Errorf("invalid management JWT trust domain %q: %w", value, err)
	}
	if trustDomain.Name() != value {
		return fmt.Errorf("management JWT trust domain %q must be a bare SPIFFE trust domain", value)
	}

	return nil
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
