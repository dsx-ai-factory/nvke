// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controllerconfig is the file mounted from the nvke-controller-config
// ConfigMap, read once at controller startup in place of the environment
// variables config.Config parses today.
package controllerconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

// Config is the top-level shape of nvke-controller-config's config.yaml key. It
// mirrors every field of internal/config.Config the chart sets, except the ones
// that keep the environment-variable path: secretKeyRef-backed credentials,
// POD_NAMESPACE and SPIRE_SERVER_ADDRESS (both can come from the downward API),
// and AUTH_CONFIG_PATH. The package tests hold the full classification and fail
// when config.Config gains a field it does not cover.
type Config struct {
	AgentReadiness                           AgentReadinessConfig       `json:"agentReadiness"`
	FieldManager                             FieldManagerConfig         `json:"fieldManager"`
	ManagedByLabelValue                      string                     `json:"managedByLabelValue"`
	ManagementNamespace                      string                     `json:"managementNamespace"`
	RBACConfigMapName                        string                     `json:"rbacConfigMapName"`
	MetricsListenAddress                     string                     `json:"metricsListenAddress"`
	SupportedInfraProviders                  []string                   `json:"supportedInfraProviders"`
	NodeMetadataAllowlistedLabelKeys         []string                   `json:"nodeMetadataAllowlistedLabelKeys"`
	Tracing                                  TracingConfig              `json:"tracing"`
	CertRenewal                              CertRenewalConfig          `json:"certRenewal"`
	CredentialMonitoring                     CredentialMonitoringConfig `json:"credentialMonitoring"`
	KubeadmImageRepository                   string                     `json:"kubeadmImageRepository"`
	CNIImageRegistry                         string                     `json:"cniImageRegistry"`
	InstallCNIAfterInit                      bool                       `json:"installCNIAfterInit"`
	AllowLegacyCalicoClusterCreation         bool                       `json:"allowLegacyCalicoClusterCreation"`
	NicoControllersEnabled                   bool                       `json:"nicoControllersEnabled"`
	ProfilesDir                              string                     `json:"profilesDir"`
	RunbooksDir                              string                     `json:"runbooksDir"`
	TenantAccessEnabled                      bool                       `json:"tenantAccessEnabled"`
	NodeAgent                                NodeAgentConfig            `json:"nodeAgent"`
	OIDC                                     OIDCConfig                 `json:"oidc"`
	Temporal                                 TemporalConfig             `json:"temporal"`
	Spire                                    SpireConfig                `json:"spire"`
	Teleport                                 TeleportConfig             `json:"teleport"`
	MachineMetadataAllowlistedLabelKeys      []string                   `json:"machineMetadataAllowlistedLabelKeys"`
	MachineMetadataAllowlistedAnnotationKeys []string                   `json:"machineMetadataAllowlistedAnnotationKeys"`
	AllowControlPlaneWithoutVIP              bool                       `json:"allowControlPlaneWithoutVIP"`
	APIAuth                                  APIAuthConfig              `json:"apiAuth"`
}

// FieldManagerConfig groups the Server-Side-Apply field manager names, mirroring
// the FieldManager* prefix in config.Config.
type FieldManagerConfig struct {
	Name                   string `json:"name"`
	RebootMachineName      string `json:"rebootMachineName"`
	MachineHealthIssueName string `json:"machineHealthIssueName"`
}

// TracingConfig configures OpenTelemetry span export, mirroring
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

// CredentialMonitoringConfig groups the credential inspection settings in config.Config.
type CredentialMonitoringConfig struct {
	Enabled            bool            `json:"enabled"`
	InspectionInterval metav1.Duration `json:"inspectionInterval"`
	ObservationMaxAge  metav1.Duration `json:"observationMaxAge"`
	WarningDays        int             `json:"warningDays"`
}

// NodeAgentConfig mirrors config.NodeAgentConfig, plus
// config.Config.NodeAgentExternalMTLSEnabled, which shares its NODE_AGENT_
// prefix. Port is left out: the chart has never set it.
type NodeAgentConfig struct {
	SpireAttestationMethod string        `json:"spireAttestationMethod"`
	DSXImds                DSXImdsConfig `json:"dsxImds"`
	TLSJWT                 TLSJWTConfig  `json:"tlsJWT"`
	ExternalMTLSEnabled    bool          `json:"externalMTLSEnabled"`
	UpdateMethod           string        `json:"updateMethod"`
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
	DiscoveryURL   string            `json:"discoveryURL,omitempty"`
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
// matching config.InfraProviderNico, config.InfraProviderInfra and
// config.InfraProviderLaunchLayer.
const (
	InfraProviderNico        = "nico"
	InfraProviderInfra       = "infra"
	InfraProviderLaunchLayer = "launchLayer"
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
	Enabled                      bool     `json:"enabled"`
	IdentityFile                 string   `json:"identityFile"`
	ProxyAddress                 string   `json:"proxyAddress"`
	Environment                  string   `json:"environment"`
	ManagedByLabelValue          string   `json:"managedByLabelValue"`
	AcceptedManagedByLabelValues []string `json:"acceptedManagedByLabelValues"`
	TokenNamePrefix              string   `json:"tokenNamePrefix"`

	ResourceGroupLabelValue string `json:"resourceGroupLabelValue"`
}

// APIAuthConfig is the NVKE REST API's authentication configuration: the same
// audiences and issuers auth/authn reads from the file at AUTH_CONFIG_PATH,
// plus the switch that turns it on. It has no apiVersion or kind, which
// describe a file; this section is part of one.
type APIAuthConfig struct {
	Enabled   bool           `json:"enabled"`
	Audiences []string       `json:"audiences"`
	Issuers   []authn.Issuer `json:"issuers"`

	// Permissive evaluates and logs every authentication decision without
	// acting on it. It applies to whichever source configures authentication,
	// so it may be true while Enabled is false and AUTH_CONFIG_PATH is set.
	Permissive bool `json:"permissive"`
}

// AuthConfig returns the section as the configuration authn.Setup takes, or nil
// when it is switched off.
func (a APIAuthConfig) AuthConfig() *authn.Config {
	if !a.Enabled {
		return nil
	}
	return &authn.Config{Audiences: slices.Clone(a.Audiences), Issuers: slices.Clone(a.Issuers)}
}

// validate applies auth/authn's rules when the section is on. Off, it must
// be empty: a filled-in section left switched off would read as "configured"
// to anyone looking at the file and authenticate nobody.
func (a APIAuthConfig) validate() error {
	if !a.Enabled {
		if len(a.Audiences) > 0 || len(a.Issuers) > 0 {
			return errors.New("audiences or issuers are set but enabled is false; set enabled to true to authenticate API requests, or empty both")
		}
		return nil
	}
	return a.AuthConfig().ValidateStartup()
}

// Load reads and validates the configuration at path. A missing file is not
// reached in practice: Kubernetes will not start the controller's container
// until the ConfigMap a volume mount references exists. A malformed or
// invalid one is a startup failure, never a silent fallback to defaults, the
// same fail-closed rule authn.Load already follows for AUTH_CONFIG_PATH.
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
	if err := refuseMovedKeys(jsonBytes); err != nil {
		return nil, fmt.Errorf("invalid controller config %q: %w", path, err)
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

	// Required of the file rather than of a Config value, like authn.Load's
	// header check. An absent apiAuth decodes as the zero value, which is
	// switched off, so a ConfigMap that predates the section would start the
	// controller without authentication. Turning it off must be said outright.
	if err := requireAPIAuth(jsonBytes); err != nil {
		return nil, fmt.Errorf("invalid controller config %q: %w", path, err)
	}
	return &cfg, nil
}

// requireAPIAuth refuses a document whose top level has no apiAuth key or has
// apiAuth: null, which decode exactly as "enabled: false" does. jsonBytes is the
// document Load has already decoded strictly, so it is a JSON object here.
func requireAPIAuth(jsonBytes []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(jsonBytes, &top); err != nil {
		return fmt.Errorf("apiAuth: %w", err)
	}
	if raw, ok := top["apiAuth"]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("apiAuth is required; set apiAuth.enabled to false to run without API authentication. " +
			"A config.yaml without it comes from a chart older than this controller: deploy the chart and image together")
	}
	return nil
}

// movedPermissiveMode is the top-level key apiAuth.permissive replaced. Strict
// decoding would refuse it as an unknown field without saying where it went.
const (
	movedPermissiveMode = "permissiveMode"
)

// refuseMovedKeys names the new home of a moved key the document still sets.
func refuseMovedKeys(jsonBytes []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(jsonBytes, &top); err != nil {
		return fmt.Errorf("controller config: %w", err)
	}
	if _, ok := top[movedPermissiveMode]; ok {
		return fmt.Errorf("%s moved to apiAuth.permissive; set it there. "+
			"A config.yaml with %s comes from a chart older than this controller: deploy the chart and image together", movedPermissiveMode, movedPermissiveMode)
	}
	return nil
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

	if err := c.APIAuth.validate(); err != nil {
		return fmt.Errorf("apiAuth: %w", err)
	}

	if c.ManagementNamespace != "" {
		if errs := validation.IsDNS1123Label(c.ManagementNamespace); len(errs) > 0 {
			return fmt.Errorf("managementNamespace %q is not a valid namespace name: %s", c.ManagementNamespace, strings.Join(errs, "; "))
		}
	}

	if c.RBACConfigMapName != "" {
		if errs := validation.IsDNS1123Subdomain(c.RBACConfigMapName); len(errs) > 0 {
			return fmt.Errorf("rbacConfigMapName %q is not a valid ConfigMap name: %s", c.RBACConfigMapName, strings.Join(errs, "; "))
		}
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

	if err := validateNodeMetadataAllowlistedLabelKeys(c.NodeMetadataAllowlistedLabelKeys); err != nil {
		return fmt.Errorf("nodeMetadataAllowlistedLabelKeys: %w", err)
	}

	if err := validateMetadataAllowlistedKeys(c.MachineMetadataAllowlistedLabelKeys); err != nil {
		return fmt.Errorf("machineMetadataAllowlistedLabelKeys: %w", err)
	}
	if err := validateMetadataAllowlistedKeys(c.MachineMetadataAllowlistedAnnotationKeys); err != nil {
		return fmt.Errorf("machineMetadataAllowlistedAnnotationKeys: %w", err)
	}

	if err := c.AgentReadiness.validate(); err != nil {
		return fmt.Errorf("agentReadiness: %w", err)
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
		if provider != InfraProviderNico && provider != InfraProviderInfra && provider != InfraProviderLaunchLayer {
			return fmt.Errorf("[%d] %q is not a supported provider, which are %q, %q and %q", i, provider, InfraProviderNico, InfraProviderInfra, InfraProviderLaunchLayer)
		}
		if prev, dup := seen[provider]; dup {
			return fmt.Errorf("[%d] %q duplicates [%d]", i, provider, prev)
		}
		seen[provider] = i
	}
	return nil
}

const (
	// NodeMetadataControlPlaneRoleKey is written by core bootstrap components when a
	// node joins as a control plane. Callers cannot set it as a label or a taint, and
	// nodeMetadataAllowlistedLabelKeys cannot list it.
	NodeMetadataControlPlaneRoleKey = "node-role.kubernetes.io/control-plane"
)

// NodeMetadataReservedInternalDomains and their subdomains are reserved for internal components. Callers cannot
// set label or taint keys under them, and nodeMetadataAllowlistedLabelKeys cannot list them.
func NodeMetadataReservedInternalDomains() []string {
	return []string{"nke.nvidia.com", "nke.io", "nvke.nvidia.com"}
}

// validateNodeMetadataAllowlistedLabelKeys refuses an entry the server would never honor: the
// control-plane role key, or a key under NodeMetadataReservedInternalDomains. An empty
// list is valid and allows no keys under the reserved Kubernetes domains.
func validateNodeMetadataAllowlistedLabelKeys(keys []string) error {
	if err := validateMetadataAllowlistedKeys(keys); err != nil {
		return err
	}
	for i, key := range keys {
		if key == NodeMetadataControlPlaneRoleKey {
			return fmt.Errorf("[%d] %q is managed by core bootstrap components and cannot be allowlisted", i, key)
		}
		if domain, isReserved := reservedDomain(key, NodeMetadataReservedInternalDomains()); isReserved {
			return fmt.Errorf("[%d] %q is under reserved domain %q and cannot be allowlisted", i, key, domain)
		}
	}
	return nil
}

// validateMetadataAllowlistedKeys validates exact metadata keys. It does not
// accept domain wildcards; labels and annotations share the same key syntax.
func validateMetadataAllowlistedKeys(keys []string) error {
	seen := make(map[string]int, len(keys))
	for i, key := range keys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("[%d] must not be empty", i)
		}
		if err := requireUnpadded(fmt.Sprintf("[%d]", i), key); err != nil {
			return err
		}
		if errs := content.IsLabelKey(key); len(errs) > 0 {
			return fmt.Errorf("[%d] %q is not a valid label key: %s", i, key, strings.Join(errs, "; "))
		}
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("[%d] %q duplicates [%d]", i, key, prev)
		}
		seen[key] = i
	}
	return nil
}

// reservedDomain returns the entry of domains that key's prefix equals or is a subdomain of. A bare key has no prefix.
func reservedDomain(key string, domains []string) (string, bool) {
	prefix, _, prefixed := strings.Cut(key, "/")
	if !prefixed {
		return "", false
	}
	for _, domain := range domains {
		if prefix == domain || strings.HasSuffix(prefix, "."+domain) {
			return domain, true
		}
	}
	return "", false
}

// validate mirrors the node-agent rules in config.Config.Validate. joinToken,
// dsxImds, and TLS/JWT all read spire.spiffeEnabled, which lives outside this
// struct, so it is passed in.
func (n *NodeAgentConfig) validate(spiffeEnabled bool) error {
	if n.UpdateMethod != "" && n.UpdateMethod != "apt" && n.UpdateMethod != "bundle" {
		return fmt.Errorf("updateMethod must be apt or bundle")
	}
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
	seenDiscoveryURLs := make(map[string]int, len(o.Providers))
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
		if o.ManagementOIDCAuth.Enabled && issuerURL == strings.TrimSpace(o.ManagementOIDCAuth.IssuerURL) {
			return fmt.Errorf("providers[%d] (%s): duplicate issuerURL %q, already used by managementOIDCAuth", i, name, issuerURL)
		}
		seenIssuers[issuerURL] = i

		if p.DiscoveryURL != "" {
			if err := validateDiscoveryURL(issuerURL, p.DiscoveryURL); err != nil {
				return fmt.Errorf("providers[%d] (%s): %w", i, name, err)
			}
			if prev, dup := seenDiscoveryURLs[p.DiscoveryURL]; dup {
				return fmt.Errorf("providers[%d] (%s): duplicate discoveryURL %q, already used by providers[%d]", i, name, p.DiscoveryURL, prev)
			}
			if o.ManagementOIDCAuth.Enabled && p.DiscoveryURL == strings.TrimSpace(o.ManagementOIDCAuth.DiscoveryURL) {
				return fmt.Errorf("providers[%d] (%s): duplicate discoveryURL %q, already used by managementOIDCAuth", i, name, p.DiscoveryURL)
			}
			seenDiscoveryURLs[p.DiscoveryURL] = i
		}

		if strings.TrimSpace(p.UsernameClaim) == "" {
			return fmt.Errorf("providers[%d] (%s): usernameClaim is required and must not be empty", i, name)
		}
		if err := validateIdentityPrefixes(p); err != nil {
			return fmt.Errorf("providers[%d] (%s): %w", i, name, err)
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

// validateDiscoveryURL applies kube-apiserver's checks on a JWT issuer's
// discoveryURL, so a bad value stops the controller at startup rather than
// every control-plane node of each new cluster at bootstrap.
func validateDiscoveryURL(issuerURL, discoveryURL string) error {
	if err := requireUnpadded("discoveryURL", discoveryURL); err != nil {
		return err
	}
	u, err := url.Parse(discoveryURL)
	if err != nil {
		return fmt.Errorf("discoveryURL %q is not a URL: %w", discoveryURL, err)
	}
	switch {
	case u.Scheme != "https" || u.Hostname() == "":
		return fmt.Errorf("discoveryURL %q must be an absolute https URL", discoveryURL)
	case u.User != nil:
		return fmt.Errorf("discoveryURL %q must not contain a username or password", discoveryURL)
	case u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("discoveryURL %q must not contain a query or fragment", discoveryURL)
	case strings.TrimRight(discoveryURL, "/") == strings.TrimRight(issuerURL, "/"):
		return fmt.Errorf("discoveryURL %q must differ from issuerURL; leave it unset to use the issuer's own discovery document", discoveryURL)
	}
	return nil
}

// ReservedIdentityPrefixes returns the username and group namespaces that
// Kubernetes, kubeadm, NVKE and NVIDIA own. kube-apiserver rejects any identity
// a fleet-wide provider maps into one, and Validate refuses a provider whose
// own prefix already lies inside one, since every login through it would fail.
func ReservedIdentityPrefixes() []string {
	return []string{"system:", "kubeadm:", "nke:", "nvke:", "nvidia:"}
}

// validateIdentityPrefixes refuses a usernamePrefix or groupsPrefix inside a
// reserved namespace. A prefix that only begins one, such as "sys", is left to
// kube-apiserver, which checks the mapped value.
func validateIdentityPrefixes(p OIDCProvider) error {
	for _, reserved := range ReservedIdentityPrefixes() {
		if strings.HasPrefix(p.UsernamePrefix, reserved) {
			return fmt.Errorf("usernamePrefix %q is inside the reserved %q namespace", p.UsernamePrefix, reserved)
		}
		if strings.HasPrefix(p.GroupsPrefix, reserved) {
			return fmt.Errorf("groupsPrefix %q is inside the reserved %q namespace", p.GroupsPrefix, reserved)
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
