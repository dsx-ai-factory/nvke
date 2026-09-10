// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

const (
	// ForgeEnvironmentProduction is the standard production Forge environment.
	ForgeEnvironmentProduction = "production"
	// ForgeEnvironmentTest selects the local test mock server.
	// Skips NGC auth (uses a self-signed JWT) and does not require FWE_API_KEY.
	ForgeEnvironmentTest = "test"

	// InfraProviderNico is the API-facing name for the Nico provider block.
	InfraProviderNico = "nico"
	// InfraProviderInfra is the API-facing name for the generic CAPI provider block.
	InfraProviderInfra = "infra"
)

type Config struct {
	// LogLevel is the level of logs to output (debug|info|warn|error)
	LogLevel string `env:"LOG_LEVEL" default:"info"`

	// DBConnectionString is the Postgres connection string for the database.
	// In production the password component is overridden by CnpgSuperuserPassword
	// (sourced from the cnpg-db-passwords Secret) so the compiled-in dev default
	// never reaches a real cluster.
	DBConnectionString string `env:"DB_CONNECTION_STRING" default:"postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"`

	// CnpgSuperuserPassword is the CNPG PostgreSQL superuser password, sourced from
	// the cnpg-db-passwords Secret. When set it replaces the password in
	// DBConnectionString at startup.
	CnpgSuperuserPassword string `env:"CNPG_SUPERUSER_PASSWORD"`

	// CnpgCloudPassword is the cloud-workflow database app-user password.
	CnpgCloudPassword string `env:"CNPG_CLOUD_PASSWORD"`

	// CnpgTemporalPassword is the temporal database app-user password.
	CnpgTemporalPassword string `env:"CNPG_TEMPORAL_PASSWORD"`

	// DBQueryLogLevel is the level of logs to output for all database queries (debug|info)
	DBQueryLogLevel string `env:"DB_QUERY_LOG_LEVEL" default:"debug"`

	// DBMaxOpenConnections is the maximum number of open connections to the database
	DBMaxOpenConnections int `env:"DB_MAX_OPEN_CONNECTIONS" default:"32"`

	// ServerListenAddress is the address and port to bind for the HTTP server
	ServerListenAddress string `env:"SERVER_LISTEN_ADDRESS" default:":8080"`

	// MetricsListenAddress is the address and port for the Prometheus metrics
	// server. Defaults to 127.0.0.1:9090 (localhost-only) so the endpoint is
	// not reachable from outside the pod without an explicit override.
	// Set to 0.0.0.0:9090 (paired with a NetworkPolicy) to allow scraping.
	MetricsListenAddress string `env:"METRICS_LISTEN_ADDRESS" default:"127.0.0.1:9090"`

	// TemporalHostPort is the host and port of the Temporal server to use for CWE workflows
	TemporalHostPort string `env:"TEMPORAL_HOST_PORT" default:"localhost:7233"`

	// TemporalNamespace is the Temporal namespace to use for CWE workflows
	TemporalNamespace string `env:"TEMPORAL_NAMESPACE" default:"cloud-workflow-engine"`

	// TemporalTaskQueue is the Temporal task queue to use for CWE workflows
	TemporalTaskQueue string `env:"TEMPORAL_TASK_QUEUE" default:"cloud-workflow-engine-tasks"`

	// TemporalUIURL is the base URL of the Temporal Web UI (e.g. http://temporal-ui.example.com).
	// When set, the node history UI renders workflow titles as links into the Temporal UI.
	TemporalUIURL string `env:"TEMPORAL_UI_URL" default:""`

	// TemporalTLS configures optional mTLS for CWE's Temporal client.
	TemporalTLS TemporalTLSConfig `envPrefix:"TEMPORAL_MTLS_"`

	// OutboxPollingInterval is the interval to wait between polling the workflow outbox for new entries
	OutboxPollingInterval time.Duration `env:"OUTBOX_POLLING_INTERVAL" default:"5s"`

	// OutboxJitterFactor is the multiplier for how much to randomly vary the outbox polling interval
	OutboxJitterFactor float64 `env:"OUTBOX_JITTER_FACTOR" default:"0.2"`

	// OutboxBatchSize is the maximum number of workflow outbox entries to process in a single batch
	OutboxBatchSize int `env:"OUTBOX_BATCH_SIZE" default:"10"`

	// NodeHealthCheckDisabled is a flag to disable node health checks
	// This can act as an emergency switch to disable node health checks
	NodeHealthCheckDisabled bool `env:"NODE_HEALTH_CHECK_DISABLED" default:"false"`

	// NodeHealthCheckTimeout is the timeout for a cluster's node health check reconcile
	NodeHealthCheckTimeout time.Duration `env:"NODE_HEALTH_CHECK_TIMEOUT" default:"2m"`

	// ForgeEnvironment selects which Forge environment to use (production|test).
	// Defaults to production.
	ForgeEnvironment string `env:"FORGE_ENVIRONMENT" default:"production"`

	// ForgeURL is the Forge API base URL.
	ForgeURL string `env:"FORGE_URL" default:"https://api.ngc.nvidia.com"`

	// FWEAPIKey is the NGC API key to pass to FWE, for use when calling the Forge API.
	FWEAPIKey string `env:"FWE_API_KEY"`

	// NicoControllersEnabled enables controllers that watch NICo infrastructure
	// resources. Disable it when CWE manages only provisioned infrastructure.
	NicoControllersEnabled bool `env:"NICO_CONTROLLERS_ENABLED" default:"true"`

	// KubeVipFMDSEndpoint is the full Forge Metadata Service URL used to resolve
	// the per-node DPU ASN on native-networking sites. When empty, CAPI and
	// subnet-based NICo pools use the static Forge peer ASN.
	KubeVipFMDSEndpoint string `env:"KUBE_VIP_FMDS_ENDPOINT"`

	// ProfilesDir is the directory from which to read osProfiles and configProfiles
	ProfilesDir string `env:"PROFILES_DIR" default:"./profiles"`

	// SupportedInfraProviders is the list of provider payload keys this CWE
	// deployment advertises for cluster specs.
	SupportedInfraProviders []string `env:"SUPPORTED_INFRA_PROVIDERS" envSeparator:"," default:"nico"`

	// RunbooksDir is the optional directory containing config-profile hook scripts.
	// When omitted, hook execution looks under ProfilesDir/runbooks before falling
	// back to controller-owned embedded runbooks.
	RunbooksDir string `env:"RUNBOOKS_DIR"`

	// NodeRebootInitialTimeout is the initial timeout for the node reboot workflow.
	NodeRebootInitialTimeout time.Duration `env:"NODE_REBOOT_INITIAL_TIMEOUT" default:"30m"`

	// NodeRebootTimeoutIncrement is the increment for the node reboot workflow timeout by each attempt.
	NodeRebootTimeoutIncrement time.Duration `env:"NODE_REBOOT_TIMEOUT_INCREMENT" default:"15m"`

	// NodeRebootMaxTimeout is the maximum timeout for the node reboot workflow.
	NodeRebootMaxTimeout time.Duration `env:"NODE_REBOOT_MAX_TIMEOUT" default:"1h"`

	// NodeRebootPollingInterval is the polling interval for the node reboot workflow.
	NodeRebootPollingInterval time.Duration `env:"NODE_REBOOT_POLLING_INTERVAL" default:"1m"`

	// NodeProvisioningTimeout is the maximum time a worker node may remain provisioning
	// before the provisioning monitor gives up and recycles it. A value of 0 disables
	// automatic timeout-based provisioning remediation.
	NodeProvisioningTimeout time.Duration `env:"NODE_PROVISIONING_TIMEOUT" default:"2h"`

	// NodeSchedulingRetryInterval is the retry interval for node scheduling when
	// external capacity or provider readiness is temporarily unavailable.
	NodeSchedulingRetryInterval time.Duration `env:"NODE_SCHEDULING_RETRY_INTERVAL" default:"2m"`

	// CertRenewalControllerEnabled enables CWE-owned periodic Kubernetes control-plane certificate renewal.
	CertRenewalControllerEnabled bool `env:"CERT_RENEWAL_CONTROLLER_ENABLED" default:"true"`

	// CertRenewalInterval is the interval between certificate renewal checks for each cluster.
	CertRenewalInterval time.Duration `env:"CERT_RENEWAL_INTERVAL" default:"12h"`

	// CertRenewalThresholdDays is passed to the certificate-renewal runbook.
	CertRenewalThresholdDays int `env:"CERT_RENEWAL_THRESHOLD_DAYS" default:"30"`

	// TenantSitesCacheTTL is the time-to-live for site cache entries
	TenantSitesCacheTTL time.Duration `env:"TENANT_SITES_CACHE_TTL" default:"24h"`

	// NKESiteControllerName is the name of the site controller
	NKESiteControllerName string `env:"NKE_SITE_CONTROLLER_NAME"`

	// SiteInstancesCacheTTL is the time-to-live for instance cache entries
	SiteInstancesCacheTTL time.Duration `env:"SITE_INSTANCES_CACHE_TTL" default:"30s"`

	// SiteInstanceTypesCacheTTL is the time-to-live for instance type cache entries
	SiteInstanceTypesCacheTTL time.Duration `env:"SITE_INSTANCE_TYPES_CACHE_TTL" default:"5m"`

	// SiteSubnetsCacheTTL is the time-to-live for subnet cache entries
	SiteSubnetsCacheTTL time.Duration `env:"SITE_SUBNETS_CACHE_TTL" default:"30s"`

	// SiteVpcPrefixesCacheTTL is the time-to-live for VPC prefix cache entries
	SiteVpcPrefixesCacheTTL time.Duration `env:"SITE_VPC_PREFIXES_CACHE_TTL" default:"30s"`

	// SiteVPCsCacheTTL is the time-to-live for VPC cache entries
	SiteVPCsCacheTTL time.Duration `env:"SITE_VPCS_CACHE_TTL" default:"5m"`

	// SiteSSHKeyGroupsCacheTTL is the time-to-live for SSH key group cache entries
	SiteSSHKeyGroupsCacheTTL time.Duration `env:"SITE_SSH_KEY_GROUPS_CACHE_TTL" default:"5m"`

	// SiteIPBlocksCacheTTL is the time-to-live for IP block cache entries
	SiteIPBlocksCacheTTL time.Duration `env:"SITE_IP_BLOCKS_CACHE_TTL" default:"5m"`

	// SiteInfiniBandPartitionsCacheTTL is the time-to-live for InfiniBand partition cache entries
	SiteInfiniBandPartitionsCacheTTL time.Duration `env:"SITE_INFINIBAND_PARTITIONS_CACHE_TTL" default:"5m"`

	// SiteNVLinkLogicalPartitionsCacheTTL is the time-to-live for NVLink logical partition cache entries
	SiteNVLinkLogicalPartitionsCacheTTL time.Duration `env:"SITE_NVLINK_LOGICAL_PARTITIONS_CACHE_TTL" default:"30s"`

	// SpiffeEnabled is a feature flag to enable SPIFFE support.
	SpiffeEnabled bool `env:"SPIFFE_ENABLED" default:"false"`

	// SpireAgentStartupTimeout is the timeout for the SPIRE agent startup.
	SpireAgentStartupTimeout time.Duration `env:"SPIRE_AGENT_STARTUP_TIMEOUT" default:"30s"`

	// SpireAgentStartupRetryInterval is the interval for retrying the SPIRE agent startup.
	SpireAgentStartupRetryInterval time.Duration `env:"SPIRE_AGENT_STARTUP_RETRY_INTERVAL" default:"1s"`

	// SpiffeEndpointSocket is the socket path for the SPIFFE endpoint. Note this is the same env var used by SPIRE agent by default.
	SpiffeEndpointSocket string `env:"SPIFFE_ENDPOINT_SOCKET" default:"unix:///tmp/spire-agent/agent.sock"`

	// SpiffeJWTExchangeEndpoint, when set, is the RFC 8693 token-exchange endpoint
	// the exec credential plugin trades its JWT-SVID at before presenting the
	// result to a tenant API server. It is written into the kubeconfigs the
	// tenant-access controller publishes, so it takes effect on the next reconcile.
	//
	// Leave it empty to present the JWT-SVID from the local SPIRE server directly,
	// which is the behavior every site has today. Set it only where the tenant
	// clusters trust the exchange's issuer rather than that SPIRE server's, or
	// CWE's calls to them will start failing authentication.
	SpiffeJWTExchangeEndpoint string `env:"SPIFFE_JWT_EXCHANGE_ENDPOINT"`

	// SpireServerNamespace is the namespace of the SPIRE server.
	SpireServerNamespace string `env:"SPIRE_SERVER_NAMESPACE" default:"spire-server"`

	// SpireClassName is the SPIRE className used in ClusterStaticEntry CRD entries.
	SpireClassName string `env:"SPIRE_CLASS_NAME" default:"spire-mgmt-spire"`

	// SpireTPMPubhashesConfigMapName is the name of the SPIRE configmap to store TPM pubhashes.
	SpireTPMPubhashesConfigMapName string `env:"SPIRE_TPM_PUBHASHES_CONFIG_MAP_NAME" default:"tpm-pubhashes"`

	// SpireServerAddress is the address of the SPIRE server for agents to connect to.
	// This needs to be an address that is reachable from the node which at the time of provisioning is not running in k8s e.g. kube-vip or control plane node IP.
	SpireServerAddress string `env:"SPIRE_SERVER_ADDRESS" default:"localhost"`

	// SpireServerPort is the port of the SPIRE server for agents to connect to.
	// This needs to be a port that is exposed externally to the node which at the time of provisioning is not running in k8s e.g. a nodePort value.
	SpireServerPort string `env:"SPIRE_SERVER_PORT" default:"9091"`

	// SpireServerAPIAddress is the SPIRE server API address used for server-side operations such as issuing join tokens.
	// This should be reachable from CWE. By default we assume SPIRE server is running in-cluster in the spire-server namespace.
	SpireServerAPIAddress string `env:"SPIRE_SERVER_API_ADDRESS" default:"spire-server.spire-server.svc.cluster.local:8081"`

	// SpireJoinTokenTTL is the TTL used when creating SPIRE join tokens via the server API.
	SpireJoinTokenTTL time.Duration `env:"SPIRE_JOIN_TOKEN_TTL" default:"1h"`

	// NodeAgentPort is the "new" port that nke node-agent listens on.
	// Note: This will only be used if SpiffeEnabled is true. Eventually this will be the only port used.
	NodeAgentPort string `env:"NODE_AGENT_PORT" default:"15005"`

	// DgxcTenantUser is the DGXC tenant SSH username injected into OS profile userData templates.
	DgxcTenantUser string `env:"DGXC_TENANT_USER"`

	// DgxcTenantPassword is the DGXC tenant SSH plaintext password. CWE hashes it at provision time
	// before embedding in cloud-init user-data, so the hash varies per node (unique random salt).
	DgxcTenantPassword string `env:"DGXC_TENANT_PASSWORD"`

	Teleport TeleportConfig `envPrefix:"TELEPORT_"`

	// When true, all mutating requests (POST, PUT, PATCH, DELETE) must include the X-Managed-By header.
	ManagedByK8sPlatform bool `env:"MANAGED_BY_K8S_PLATFORM" default:"false"`

	// LocalDevForceDockerMachineBootstrap forces every node through the
	// DockerMachine bootstrap/preflight + container-mode kubeadm contract
	// (the /run/cluster-api/bootstrap-success.complete sentinel) regardless of
	// its CAPI infra machine template.
	//
	// LOCAL-DEV ONLY: the local NICo container flow provisions nodes as bare Docker
	// containers with no real cloud-init, so the normal cloud-init-based
	// bootstrap never signals completion. This flag must NEVER be set in a real
	// environment; doing so would misroute production nodes to the DinD contract.
	LocalDevForceDockerMachineBootstrap bool `env:"LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP" default:"false"`

	// ProvisionControlPlaneWithControlPlaneController routes control-plane node
	// provisioning through the control-plane controller, which creates no-nodepool
	// control-plane nodes. When enabled, the node set controller stops creating
	// pending nodes for control-plane node pools so the two controllers never both
	// create control-plane nodes. When false, control-plane nodes continue to be
	// created by the node set controller from their node pool.
	ProvisionControlPlaneWithControlPlaneController bool `env:"PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER" default:"false"`

	// InstallCNIAfterInit installs CNI after kubeadm init, from a Debian package
	// rather than a bundle server. It needs the charts package on the node, which
	// Forge user-data installs as of #398.
	//
	// Still off by default: no real site has run this path yet. Turn it on
	// deliberately, so a regression has one obvious cause.
	InstallCNIAfterInit bool `env:"INSTALL_CNI_AFTER_INIT" default:"false"`

	// PodNamespace is the namespace the CWE worker runs in.
	PodNamespace string `env:"POD_NAMESPACE" default:"cloud-workflow-engine"`

	// KubeadmImageRepository is the OCI registry kubeadm uses for control-plane
	// component images. Set KUBEADM_IMAGE_REPOSITORY to redirect to a local mirror
	// for air-gap environments.
	KubeadmImageRepository string `env:"KUBEADM_IMAGE_REPOSITORY" default:"registry.k8s.io"`

	// CNIImageRegistry is the OCI registry prefix for CNI component images.
	// Set CNI_IMAGE_REGISTRY to redirect CNI image pulls to a local mirror.
	CNIImageRegistry string `env:"CNI_IMAGE_REGISTRY" default:""`

	// NkeOIDCAuth is the site-wide configuration for OIDC authentication from
	// NKE management identities to tenant cluster kube-apiservers.
	NkeOIDCAuth NkeOIDCAuth `envPrefix:"NKE_OIDC_AUTH_"`

	// TenantAccessEnabled is the site Helm/env flag for the Nico consume
	// fallback. CAPI clients always read the published nke-system Secret.
	// The TenantAccess controller is constructed whenever a Kubernetes client
	// exists and always publishes nke-system/<name>-kubeconfig; this flag does
	// not gate that writer.
	TenantAccessEnabled bool `env:"TENANT_ACCESS_ENABLED" default:"false"`

	// AuthConfigPath is the mounted YAML file holding the API authentication
	// configuration. Unset disables authentication. A set-but-unreadable path is
	// a startup failure, never "disabled"; see auth.Load.
	//
	// Validate() does not check it: unset is the valid default.
	AuthConfigPath string `env:"AUTH_CONFIG_PATH"`

	// PermissiveMode evaluates and logs authentication and admission decisions
	// without enforcing them, so no request is rejected. No effect when
	// AuthConfigPath is unset.
	PermissiveMode bool `env:"PERMISSIVE_MODE" default:"false"`

	// Tracing configures OpenTelemetry distributed tracing. When Enabled and
	// Endpoint are set, CWE exports OTLP gRPC spans. Endpoint and related OTEL_*
	// vars may be injected by the OpenTelemetry Operator (inject-sdk + sidecar);
	// do not hardcode collector URLs in application code.
	Tracing TracingConfig `envPrefix:"OTEL_"`
}

// TracingConfig configures OpenTelemetry tracing export.
// Env vars use standard OTEL_* names (also commonly injected by the
// OpenTelemetry Operator): OTEL_TRACING_ENABLED, OTEL_EXPORTER_OTLP_ENDPOINT,
// OTEL_EXPORTER_OTLP_INSECURE, OTEL_SERVICE_NAME.
type TracingConfig struct {
	// Enabled gates TracerProvider initialization and transport instrumentation.
	Enabled bool `env:"TRACING_ENABLED" default:"false"`
	// Endpoint is the OTLP gRPC collector host:port (scheme optional). With
	// operator sidecar injection this is typically localhost:4317.
	Endpoint string `env:"EXPORTER_OTLP_ENDPOINT" default:""`
	// Insecure uses plaintext OTLP (required for localhost sidecar / in-cluster collectors).
	Insecure bool `env:"EXPORTER_OTLP_INSECURE" default:"true"`
	// ServiceName is the resource attribute service.name.
	ServiceName string `env:"SERVICE_NAME" default:"cloud-workflow-engine"`
}

// TemporalTLSConfig contains optional file paths for Temporal client mTLS.
type TemporalTLSConfig struct {
	CaCert     string `env:"CA_CERT"`
	ClientKey  string `env:"CLIENT_KEY"`
	ClientCert string `env:"CLIENT_CERT"`
	ServerName string `env:"SERVER_NAME"`
}

type NkeOIDCAuth struct {
	// Enabled controls whether new clusters receive an apiserver
	// AuthenticationConfiguration for NKE OIDC identities.
	Enabled bool `env:"ENABLED" default:"false"`
	// AutoMigrationEnabled is reserved for future migration of existing clusters.
	AutoMigrationEnabled bool `env:"AUTO_MIGRATION_ENABLED" default:"false"`
	// IssuerURL is the OIDC issuer trusted by tenant kube-apiservers.
	IssuerURL string `env:"ISSUER_URL" default:""`
	// DiscoveryURL optionally overrides the issuer discovery endpoint.
	DiscoveryURL string `env:"DISCOVERY_URL" default:""`
	// CABundleSecret points at the Secret containing a PEM CA bundle for the issuer.
	CABundleSecret OIDCCABundleSecretRef `envPrefix:"CA_BUNDLE_SECRET_"`
}

type OIDCCABundleSecretRef struct {
	Namespace string `env:"NAMESPACE" default:""`
	Name      string `env:"NAME" default:""`
	Key       string `env:"KEY" default:""`
}

func (r OIDCCABundleSecretRef) Empty() bool {
	return r.Namespace == "" && r.Name == "" && r.Key == ""
}

// TeleportConfig contains configuration for the Teleport TPM join token integration.
type TeleportConfig struct {
	Enabled      bool   `env:"ENABLED" default:"false"`
	IdentityFile string `env:"IDENTITY_FILE"`
	ProxyAddress string `env:"PROXY_ADDRESS"`
	Environment  string `env:"ENVIRONMENT" default:"non-prod"`

	DefaultClusterEnabled    bool `env:"DEFAULT_CLUSTER_ENABLED" default:"false"`
	DefaultCPPoolEnabled     bool `env:"DEFAULT_CP_POOL_ENABLED" default:"false"`
	DefaultWorkerPoolEnabled bool `env:"DEFAULT_WORKER_POOL_ENABLED" default:"false"`
}

// NodePoolTeleportDefault returns the teleport default for a node pool based on its role.
func (c TeleportConfig) NodePoolTeleportDefault(controlPlane bool) bool {
	if controlPlane {
		return c.DefaultCPPoolEnabled
	}
	return c.DefaultWorkerPoolEnabled
}

// EffectiveDBConnectionString returns DBConnectionString with the password
// replaced by CnpgSuperuserPassword when it is set, so production deployments
// use Vault-sourced credentials rather than the compiled-in dev default.
func (c Config) EffectiveDBConnectionString() (string, error) {
	if c.CnpgSuperuserPassword == "" {
		return c.DBConnectionString, nil
	}
	u, err := url.Parse(c.DBConnectionString)
	if err != nil {
		return "", fmt.Errorf("parse DB_CONNECTION_STRING: %w", err)
	}
	u.User = url.UserPassword(u.User.Username(), c.CnpgSuperuserPassword)
	return u.String(), nil
}

// validateDBCredentials rejects the compiled-in dev-default database
// credentials on any database that is not loopback.
//
// The guard exists so a deployment cannot reach a real database with the
// password that ships in the source. Whether that risk applies is a property of
// the database, not of anything else: `postgres:postgres@` on localhost is a
// container on the developer's own machine, and demanding an override there only
// stopped `make run` from working out of the box.
func (c *Config) validateDBCredentials() error {
	if c.CnpgSuperuserPassword != "" || !strings.Contains(c.DBConnectionString, "postgres:postgres@") {
		return nil
	}

	u, err := url.Parse(c.DBConnectionString)
	if err != nil {
		return fmt.Errorf("parse DB_CONNECTION_STRING: %w", err)
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}

	return fmt.Errorf(
		"DB_CONNECTION_STRING uses weak dev-default credentials against non-local host %q: set CNPG_SUPERUSER_PASSWORD",
		u.Hostname(),
	)
}

// isLoopbackHost reports whether host names the local machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Validate checks required field constraints.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.MetricsListenAddress) == "" {
		return fmt.Errorf("METRICS_LISTEN_ADDRESS must not be empty")
	}
	if err := c.validateDBCredentials(); err != nil {
		return err
	}
	if c.SpireServerAPIAddress == "" {
		return fmt.Errorf("SPIRE_SERVER_API_ADDRESS is required")
	}
	if c.SpireJoinTokenTTL <= 0 {
		return fmt.Errorf("SPIRE_JOIN_TOKEN_TTL must be greater than 0")
	}
	if err := c.Teleport.Validate(); err != nil {
		return err
	}
	supportedInfraProviders, err := normalizeSupportedInfraProviders(c.SupportedInfraProviders)
	if err != nil {
		return err
	}
	c.SupportedInfraProviders = supportedInfraProviders

	return nil
}

func normalizeSupportedInfraProviders(providers []string) ([]string, error) {
	if providers == nil {
		return []string{InfraProviderNico}, nil
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("SUPPORTED_INFRA_PROVIDERS must contain at least one provider")
	}

	seen := make(map[string]struct{}, len(providers))
	normalizedProviders := make([]string, 0, len(providers))
	for _, provider := range providers {
		normalized := strings.TrimSpace(provider)
		if normalized == "" {
			return nil, fmt.Errorf("SUPPORTED_INFRA_PROVIDERS must not contain empty provider names")
		}
		if normalized != InfraProviderNico && normalized != InfraProviderInfra {
			return nil, fmt.Errorf("SUPPORTED_INFRA_PROVIDERS contains unsupported provider %q", normalized)
		}
		if _, ok := seen[normalized]; ok {
			return nil, fmt.Errorf("SUPPORTED_INFRA_PROVIDERS contains duplicate provider %q", normalized)
		}
		seen[normalized] = struct{}{}
		normalizedProviders = append(normalizedProviders, normalized)
	}
	return normalizedProviders, nil
}

func (c *TeleportConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.IdentityFile == "" {
		return fmt.Errorf("TELEPORT_IDENTITY_FILE is required when TELEPORT_ENABLED=true")
	}
	if c.ProxyAddress == "" {
		return fmt.Errorf("TELEPORT_PROXY_ADDRESS is required when TELEPORT_ENABLED=true")
	}
	if c.Environment != "prod" && c.Environment != "non-prod" {
		return fmt.Errorf("TELEPORT_ENVIRONMENT must be 'prod' or 'non-prod', got: %q", c.Environment)
	}
	return nil
}

// UseControlPlaneControllerForControlPlane reports whether provisioning
// of all control-plane nodes should be routed through the control-plane
// controller path.
func (c Config) UseControlPlaneControllerForControlPlane() bool {
	return c.ProvisionControlPlaneWithControlPlaneController
}

// EffectiveRunbooksDir returns the configured profile-hook runbook directory.
func (c Config) EffectiveRunbooksDir() string {
	if c.RunbooksDir != "" {
		return c.RunbooksDir
	}
	return filepath.Join(c.ProfilesDir, "runbooks")
}

// ParseConfigFromEnv parses configuration from environment variables and validates it.
func ParseConfigFromEnv() (Config, error) {
	cfg, err := env.ParseAsWithOptions[Config](env.Options{
		DefaultValueTagName: "default",
	})
	if err != nil {
		return Config{}, fmt.Errorf("failed to parse config from environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config validation failed: %w", err)
	}
	return cfg, nil
}
