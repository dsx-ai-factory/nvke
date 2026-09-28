// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controllerconfig is the file mounted from the nke-controller-config
// ConfigMap, read once at controller startup in place of the environment
// variables config.Config parses today.
package controllerconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	yamlv3 "gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// Config is the top-level shape of nke-controller-config's config.yaml key. It
// mirrors every field of internal/config.Config the chart sets, except the ones
// that keep the environment-variable path: secretKeyRef-backed credentials,
// POD_NAMESPACE and SPIRE_SERVER_ADDRESS (both can come from the downward API),
// and AUTH_CONFIG_PATH. The package tests hold the full classification and fail
// when config.Config gains a field it does not cover.
type Config struct {
	FieldManager            FieldManagerConfig `json:"fieldManager"`
	ManagedByLabelValue     string             `json:"managedByLabelValue"`
	MetricsListenAddress    string             `json:"metricsListenAddress"`
	SupportedInfraProviders []string           `json:"supportedInfraProviders"`
	Tracing                 TracingConfig      `json:"tracing"`
	CertRenewal             CertRenewalConfig  `json:"certRenewal"`
	KubeadmImageRepository  string             `json:"kubeadmImageRepository"`
	CNIImageRegistry        string             `json:"cniImageRegistry"`
	InstallCNIAfterInit     bool               `json:"installCNIAfterInit"`
	NicoControllersEnabled  bool               `json:"nicoControllersEnabled"`
	ProfilesDir             string             `json:"profilesDir"`
	RunbooksDir             string             `json:"runbooksDir"`
	TenantAccessEnabled     bool               `json:"tenantAccessEnabled"`
	NodeAgent               NodeAgentConfig    `json:"nodeAgent"`
	OIDC                    OIDCConfig         `json:"oidc"`
	Temporal                TemporalConfig     `json:"temporal"`
	Spire                   SpireConfig        `json:"spire"`
	Teleport                TeleportConfig     `json:"teleport"`
	PermissiveMode          bool               `json:"permissiveMode"`
}

// FieldManagerConfig groups the Server-Side-Apply field manager names, mirroring
// the FieldManager* prefix in config.Config.
type FieldManagerConfig struct {
	Name                   string `json:"name"`
	RebootMachineName      string `json:"rebootMachineName"`
	MachineHealthIssueName string `json:"machineHealthIssueName"`
}

// TracingConfig configures OpenTelemetry tracing export, mirroring
// config.TracingConfig.
type TracingConfig struct {
	Enabled     bool   `json:"enabled"`
	Endpoint    string `json:"endpoint"`
	Insecure    bool   `json:"insecure"`
	ServiceName string `json:"serviceName"`
}

// CertRenewalConfig groups the CertRenewal* prefix in config.Config.
type CertRenewalConfig struct {
	ControllerEnabled bool            `json:"controllerEnabled"`
	Interval          metav1.Duration `json:"interval"`
	ThresholdDays     int             `json:"thresholdDays"`
}

// NodeAgentConfig mirrors config.NodeAgentConfig, plus
// config.Config.NodeAgentExternalMTLSEnabled, which shares its NODE_AGENT_
// prefix. Port is left out: the chart has never set it.
type NodeAgentConfig struct {
	SpireAttestationMethod string        `json:"spireAttestationMethod"`
	DSXImds                DSXImdsConfig `json:"dsxImds"`
	TLSJWT                 TLSJWTConfig  `json:"tlsJWT"`
	ExternalMTLSEnabled    bool          `json:"externalMTLSEnabled"`
}

// DSXImdsConfig mirrors config.DSXImdsConfig.
type DSXImdsConfig struct {
	Endpoint       string `json:"endpoint"`
	PluginChecksum string `json:"pluginChecksum"`
	OperatorTenant string `json:"operatorTenant"`
}

// TLSJWTConfig mirrors config.TLSJWTConfig.
type TLSJWTConfig struct {
	Enabled                bool     `json:"enabled"`
	Issuer                 string   `json:"issuer"`
	JWKSURI                string   `json:"jwksURI"`
	Algorithms             []string `json:"algorithms"`
	SubjectTrustDomain     string   `json:"subjectTrustDomain"`
	DSXIdentityExchangeURL string   `json:"dsxIdentityExchangeURL"`
	SubjectTenant          string   `json:"subjectTenant"`
	SubjectService         string   `json:"subjectService"`
}

// OIDCConfig carries the management-plane shared provider and the fleet-wide
// provider list this design adds.
type OIDCConfig struct {
	ManagementOIDCAuth ManagementOIDCAuthConfig `json:"managementOIDCAuth"`
	Providers          []OIDCProvider           `json:"providers"`
}

// ManagementOIDCAuthConfig replaces the NKE_OIDC_AUTH_* environment variables
// one for one.
type ManagementOIDCAuthConfig struct {
	Enabled           bool                  `json:"enabled"`
	IssuerURL         string                `json:"issuerURL"`
	DiscoveryURL      string                `json:"discoveryURL"`
	CABundleSecretRef OIDCCABundleSecretRef `json:"caBundleSecretRef"`
}

// OIDCCABundleSecretRef points at the Secret containing a PEM CA bundle for the
// shared provider's issuer.
type OIDCCABundleSecretRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

// Empty reports whether no Secret reference was configured.
func (r OIDCCABundleSecretRef) Empty() bool {
	return r.Namespace == "" && r.Name == "" && r.Key == ""
}

// OIDCProvider is one fleet-wide provider. Unlike ManagementOIDCAuthConfig, its
// CA bundle is inline PEM rather than a Secret reference: a CA bundle is
// public certificate material with nothing to protect by indirecting through
// a Secret, and asking an operator to manage a Secret per provider buys
// nothing.
type OIDCProvider struct {
	Name           string            `json:"name"`
	IssuerURL      string            `json:"issuerURL"`
	Audiences      []string          `json:"audiences"`
	UsernameClaim  string            `json:"usernameClaim"`
	UsernamePrefix string            `json:"usernamePrefix"`
	GroupsClaim    string            `json:"groupsClaim,omitempty"`
	GroupsPrefix   string            `json:"groupsPrefix,omitempty"`
	RequiredClaims map[string]string `json:"requiredClaims,omitempty"`
	CABundle       string            `json:"caBundle"`
}

// MaxOIDCProviders is the fleet-wide provider cap: AuthenticationConfiguration
// supports at most 64 JWT authenticators in total, and a handful of those stay
// reserved for platform-managed providers.
const (
	MaxOIDCProviders = 5
)

// TemporalConfig groups the Temporal* prefix in config.Config.
type TemporalConfig struct {
	HostPort  string            `json:"hostPort"`
	Namespace string            `json:"namespace"`
	TaskQueue string            `json:"taskQueue"`
	TLS       TemporalTLSConfig `json:"tls"`
}

// TemporalTLSConfig contains optional file paths for Temporal client mTLS,
// mirroring config.TemporalTLSConfig.
type TemporalTLSConfig struct {
	CACert     string `json:"caCert"`
	ClientCert string `json:"clientCert"`
	ClientKey  string `json:"clientKey"`
	ServerName string `json:"serverName"`
}

// SpireConfig groups the Spire*/Spiffe* prefixes in config.Config. The SPIRE
// server address is not here: the chart's default sources it from the
// downward API (status.hostIP), which a ConfigMap cannot express.
type SpireConfig struct {
	SpiffeEnabled             bool            `json:"spiffeEnabled"`
	SpiffeEndpointSocket      string          `json:"spiffeEndpointSocket"`
	SpiffeJWTExchangeEndpoint string          `json:"spiffeJWTExchangeEndpoint"`
	ServerNamespace           string          `json:"serverNamespace"`
	ClassName                 string          `json:"className"`
	TPMPubhashesConfigMapName string          `json:"tpmPubhashesConfigMapName"`
	ServerPort                string          `json:"serverPort"`
	JoinTokenTTL              metav1.Duration `json:"joinTokenTTL"`
}

// Infrastructure provider names accepted in Config.SupportedInfraProviders,
// matching config.InfraProviderNico and config.InfraProviderInfra.
const (
	InfraProviderNico  = "nico"
	InfraProviderInfra = "infra"
)

// SpireAttestationMethod values accepted by NodeAgentConfig.SpireAttestationMethod,
// matching config.SpireAttestationMethod.
const (
	SpireAttestationMethodTPM       = "tpm"
	SpireAttestationMethodJoinToken = "joinToken"
	SpireAttestationMethodDSXIMDS   = "dsxImds"
)

// TeleportConfig mirrors config.TeleportConfig's chart-set fields. The three
// DefaultClusterEnabled/DefaultCPPoolEnabled/DefaultWorkerPoolEnabled fields
// are not included: the chart has never set them, so they stay on their Go
// default in internal/config until something needs to set them.
type TeleportConfig struct {
	Enabled             bool   `json:"enabled"`
	IdentityFile        string `json:"identityFile"`
	ProxyAddress        string `json:"proxyAddress"`
	Environment         string `json:"environment"`
	ManagedByLabelValue string `json:"managedByLabelValue"`
	TokenNamePrefix     string `json:"tokenNamePrefix"`
}

// Load reads and validates the configuration at path. A missing file is not
// reached in practice: Kubernetes will not start the controller's container
// until the ConfigMap a volume mount references exists. A malformed or
// invalid one is a startup failure, never a silent fallback to defaults, the
// same fail-closed rule auth.Load already follows for AUTH_CONFIG_PATH.
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("controller config path is empty")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open controller config %q: %w", path, err)
	}

	if err := requireSingleDocument(raw, path); err != nil {
		return nil, err
	}

	// Decoded with sigs.k8s.io/json, NOT sigs.k8s.io/yaml.UnmarshalStrict, which
	// ends in encoding/json and matches keys case-insensitively. There a
	// document carrying both issuerURL and issuerurl would not be a duplicate:
	// the lowercase spelling wins silently.
	jsonBytes, err := yaml.YAMLToJSONStrict(raw)
	if err != nil {
		return nil, fmt.Errorf("parse controller config %q: %w", path, err)
	}

	var cfg Config
	strictErrs, err := kjson.UnmarshalStrict(jsonBytes, &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse controller config %q: %w", path, err)
	}
	if len(strictErrs) > 0 {
		return nil, fmt.Errorf("parse controller config %q: %w", path, errors.Join(strictErrs...))
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid controller config %q: %w", path, err)
	}
	return &cfg, nil
}

// requireSingleDocument refuses a file that is empty or holds more than one
// YAML document. sigs.k8s.io/yaml has no streaming decoder and silently honors
// only the first, so an appended stanza would have no error and no effect.
func requireSingleDocument(raw []byte, path string) error {
	dec := yamlv3.NewDecoder(bytes.NewReader(raw))

	var first yamlv3.Node
	if err := dec.Decode(&first); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("controller config %q is empty", path)
		}
		return fmt.Errorf("parse controller config %q: %w", path, err)
	}

	var extra yamlv3.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("parse controller config %q: %w", path, err)
		}
		return fmt.Errorf("controller config %q contains more than one YAML document", path)
	}
	return nil
}

// Validate checks constraints Load cannot leave for callers to discover one at
// a time. A failure here is a startup failure; nothing here mutates cfg.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.MetricsListenAddress) == "" {
		return errors.New("metricsListenAddress must not be empty")
	}

	if err := c.OIDC.validate(); err != nil {
		return fmt.Errorf("oidc: %w", err)
	}

	if c.Spire.JoinTokenTTL.Duration <= 0 {
		return errors.New("spire.joinTokenTTL must be greater than 0")
	}

	if err := c.NodeAgent.validate(c.Spire.SpiffeEnabled); err != nil {
		return fmt.Errorf("nodeAgent: %w", err)
	}

	if err := c.Teleport.validate(); err != nil {
		return fmt.Errorf("teleport: %w", err)
	}

	if err := validateSupportedInfraProviders(c.SupportedInfraProviders); err != nil {
		return fmt.Errorf("supportedInfraProviders: %w", err)
	}

	return nil
}

// validateSupportedInfraProviders mirrors config's normalization of the same
// list, except that an omitted list is an error rather than ["nico"], and a
// padded name is refused rather than trimmed.
func validateSupportedInfraProviders(providers []string) error {
	if len(providers) == 0 {
		return errors.New("must contain at least one provider")
	}
	seen := make(map[string]int, len(providers))
	for i, provider := range providers {
		if strings.TrimSpace(provider) == "" {
			return fmt.Errorf("[%d] must not be empty", i)
		}
		if err := requireUnpadded(fmt.Sprintf("[%d]", i), provider); err != nil {
			return err
		}
		if provider != InfraProviderNico && provider != InfraProviderInfra {
			return fmt.Errorf("[%d] %q is not a supported provider, which are %q and %q", i, provider, InfraProviderNico, InfraProviderInfra)
		}
		if prev, dup := seen[provider]; dup {
			return fmt.Errorf("[%d] %q duplicates [%d]", i, provider, prev)
		}
		seen[provider] = i
	}
	return nil
}

// validate mirrors the node-agent rules in config.Config.Validate. joinToken,
// dsxImds, and TLS/JWT all read spire.spiffeEnabled, which lives outside this
// struct, so it is passed in.
func (n *NodeAgentConfig) validate(spiffeEnabled bool) error {
	switch n.SpireAttestationMethod {
	case SpireAttestationMethodTPM:
	case SpireAttestationMethodJoinToken:
		if !spiffeEnabled {
			return fmt.Errorf("spire.spiffeEnabled must be true when spireAttestationMethod is %q", n.SpireAttestationMethod)
		}
	case SpireAttestationMethodDSXIMDS:
		if !spiffeEnabled {
			return fmt.Errorf("spire.spiffeEnabled must be true when spireAttestationMethod is %q", n.SpireAttestationMethod)
		}
		if err := n.DSXImds.validate(); err != nil {
			return fmt.Errorf("dsxImds: %w", err)
		}
	default:
		return fmt.Errorf("spireAttestationMethod must be one of %q, %q, or %q, got %q",
			SpireAttestationMethodTPM, SpireAttestationMethodJoinToken, SpireAttestationMethodDSXIMDS, n.SpireAttestationMethod)
	}

	if !n.TLSJWT.Enabled {
		return nil
	}
	if !spiffeEnabled && !n.ExternalMTLSEnabled {
		return errors.New("spire.spiffeEnabled or externalMTLSEnabled must be true when tlsJWT.enabled is true")
	}
	if err := n.TLSJWT.validate(); err != nil {
		return fmt.Errorf("tlsJWT: %w", err)
	}
	return nil
}

// validate mirrors the dsxImds rules in config.Config.Validate. That trims
// operatorTenant and endpoint in place; this rejects padding instead, so
// Validate stays read-only and never changes what the file says.
func (d *DSXImdsConfig) validate() error {
	if strings.TrimSpace(d.OperatorTenant) == "" {
		return errors.New("operatorTenant is required")
	}
	if err := requireUnpadded("operatorTenant", d.OperatorTenant); err != nil {
		return err
	}
	if err := spiffeid.ValidatePathSegment(d.OperatorTenant); err != nil {
		return fmt.Errorf("operatorTenant must be a valid SPIFFE path segment: %w", err)
	}

	if strings.TrimSpace(d.Endpoint) == "" {
		return errors.New("endpoint is required")
	}
	if err := requireUnpadded("endpoint", d.Endpoint); err != nil {
		return err
	}
	endpoint, parseErr := url.ParseRequestURI(d.Endpoint)
	if parseErr != nil || endpoint.Hostname() == "" ||
		(!strings.EqualFold(endpoint.Scheme, "http") && !strings.EqualFold(endpoint.Scheme, "https")) {
		return fmt.Errorf("endpoint must be an absolute HTTP(S) URL with a host, got %q", d.Endpoint)
	}

	// Empty keeps the checksum the node image supplies.
	if d.PluginChecksum == "" {
		return nil
	}
	checksum, decodeErr := hex.DecodeString(d.PluginChecksum)
	if decodeErr != nil || len(checksum) != sha256.Size {
		return errors.New("pluginChecksum must be empty or a 64-character SHA-256 hex digest")
	}
	return nil
}

// validate mirrors the TLS/JWT rules in config.Config.Validate.
func (t *TLSJWTConfig) validate() error {
	switch {
	case strings.TrimSpace(t.Issuer) == "":
		return errors.New("issuer is required when enabled is true")
	case strings.TrimSpace(t.JWKSURI) == "":
		return errors.New("jwksURI is required when enabled is true")
	case len(t.Algorithms) == 0:
		return errors.New("algorithms is required when enabled is true")
	case strings.TrimSpace(t.SubjectTrustDomain) == "":
		return errors.New("subjectTrustDomain is required when enabled is true")
	case strings.TrimSpace(t.DSXIdentityExchangeURL) == "":
		return errors.New("dsxIdentityExchangeURL is required when enabled is true")
	}
	if err := requireDNSLabel("subjectTenant", t.SubjectTenant); err != nil {
		return err
	}
	return requireDNSLabel("subjectService", t.SubjectService)
}

// requireDNSLabel checks one segment of the canonical local SPIFFE ID that DSX
// Identity exchanges. config.Config.Validate trims it in place; this refuses
// padding instead, so Validate stays read-only.
func requireDNSLabel(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required when enabled is true", field)
	}
	if err := requireUnpadded(field, value); err != nil {
		return err
	}
	if len(validation.IsDNS1123Label(value)) != 0 {
		return fmt.Errorf("%s %q must be a lowercase DNS label", field, value)
	}
	return nil
}

// requireUnpadded refuses leading or trailing whitespace rather than trimming
// it, which would silently change what the operator wrote.
func requireUnpadded(field, value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s %q has leading or trailing whitespace", field, value)
	}
	return nil
}

// validate checks the shared provider and the fleet-wide list together, since
// the cross-provider constraints (uniqueness, the cap) span both.
func (o *OIDCConfig) validate() error {
	if o.ManagementOIDCAuth.Enabled && strings.TrimSpace(o.ManagementOIDCAuth.IssuerURL) == "" {
		return errors.New("managementOIDCAuth.issuerURL is required when managementOIDCAuth.enabled is true")
	}

	if len(o.Providers) > MaxOIDCProviders {
		return fmt.Errorf("providers has %d entries, more than the %d-entry cap", len(o.Providers), MaxOIDCProviders)
	}

	seenNames := make(map[string]int, len(o.Providers))
	seenIssuers := make(map[string]int, len(o.Providers))
	for i, p := range o.Providers {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("providers[%d]: name is required and must not be empty", i)
		}
		if prev, dup := seenNames[name]; dup {
			return fmt.Errorf("providers[%d]: duplicate name %q, already used by providers[%d]", i, name, prev)
		}
		seenNames[name] = i

		issuerURL := strings.TrimSpace(p.IssuerURL)
		if issuerURL == "" {
			return fmt.Errorf("providers[%d] (%s): issuerURL is required and must not be empty", i, name)
		}
		if prev, dup := seenIssuers[issuerURL]; dup {
			return fmt.Errorf("providers[%d] (%s): duplicate issuerURL %q, already used by providers[%d]", i, name, issuerURL, prev)
		}
		seenIssuers[issuerURL] = i

		if strings.TrimSpace(p.UsernameClaim) == "" {
			return fmt.Errorf("providers[%d] (%s): usernameClaim is required and must not be empty", i, name)
		}
		if len(p.Audiences) == 0 {
			return fmt.Errorf("providers[%d] (%s): audiences is required and must not be empty", i, name)
		}
		for j, aud := range p.Audiences {
			if strings.TrimSpace(aud) == "" {
				return fmt.Errorf("providers[%d] (%s): audiences[%d] must not be empty", i, name, j)
			}
		}
		if strings.TrimSpace(p.CABundle) == "" {
			return fmt.Errorf("providers[%d] (%s): caBundle is required and must not be empty", i, name)
		}
	}
	return nil
}

// validate mirrors config.TeleportConfig.Validate.
func (t *TeleportConfig) validate() error {
	if !t.Enabled {
		return nil
	}
	if t.IdentityFile == "" {
		return errors.New("identityFile is required when enabled is true")
	}
	if t.ProxyAddress == "" {
		return errors.New("proxyAddress is required when enabled is true")
	}
	if t.Environment != "prod" && t.Environment != "non-prod" {
		return fmt.Errorf("environment must be \"prod\" or \"non-prod\", got %q", t.Environment)
	}
	return nil
}
