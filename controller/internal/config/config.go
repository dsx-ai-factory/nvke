// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
)

const (
	// InfraProviderNico is the API-facing name for the Nico provider block.
	InfraProviderNico = "nico"
	// InfraProviderInfra is the API-facing name for the generic CAPI provider block.
	InfraProviderInfra = "infra"
	// InfraProviderLaunchLayer is the API-facing name for the LaunchLayer provider block.
	InfraProviderLaunchLayer = "launchLayer"

	SpireAttestationMethodTPM       SpireAttestationMethod = "tpm"
	SpireAttestationMethodJoinToken SpireAttestationMethod = "joinToken"
	SpireAttestationMethodDSXIMDS   SpireAttestationMethod = "dsxImds"

	NodeAgentExternalTLSCertPath = "/etc/nvke/node-agent-client-tls/tls.crt"
	NodeAgentExternalTLSKeyPath  = "/etc/nvke/node-agent-client-tls/tls.key"
	NodeAgentExternalTLSCAPath   = "/etc/nvke/node-agent-client-tls/ca.crt"
	// NodeAgentExternalTLSNVKECertPath and ...KeyPath are an optional second client
	// identity whose URI SAN is the new-form controller URN. When present, nodes whose
	// agent speaks the new form are dialed with it; otherwise the first identity is
	// used for every node.
	NodeAgentExternalTLSNVKECertPath = "/etc/nvke/node-agent-client-tls/nvke-tls.crt"
	NodeAgentExternalTLSNVKEKeyPath  = "/etc/nvke/node-agent-client-tls/nvke-tls.key"
	NodeAgentConfigPath              = "/etc/default/nke-agent"
	NodeAgentControllerCAPath        = "/etc/nke/controller-ca.crt"
)

// SpireAttestationMethod selects the SPIRE node attestation method pinned at node creation.
type SpireAttestationMethod string

// DSXImdsConfig configures the external dsx_imds SPIRE node attestor.
type DSXImdsConfig struct {
	// Endpoint is the metadata endpoint used by the external node attestor.
	Endpoint string `controllerconfig:"nodeAgent.dsxImds.endpoint"`

	// PluginChecksum overrides the checksum supplied by the DSX node image. An
	// empty value keeps the image-owned environment expansion.
	PluginChecksum string `controllerconfig:"nodeAgent.dsxImds.pluginChecksum"`

	// OperatorTenant is the tenant segment in dsx_imds-attested SPIRE agent IDs.
	// It must match the DSX Identity operator tenant.
	OperatorTenant string `controllerconfig:"nodeAgent.dsxImds.operatorTenant"`
}

// TLSJWTConfig configures TLS/JWT authentication for controller calls to node agents.
type TLSJWTConfig struct {
	// Enabled selects the TLS/JWT transport pinned to newly created nodes.
	Enabled bool `controllerconfig:"nodeAgent.tlsJWT.enabled"`

	// Issuer is the identity issuer expected in node-agent JWTs.
	Issuer string `controllerconfig:"nodeAgent.tlsJWT.issuer"`

	// JWKSURI is the HTTPS endpoint used to fetch node-agent JWT signing keys.
	JWKSURI string `controllerconfig:"nodeAgent.tlsJWT.jwksURI"`

	// Algorithms is the asymmetric JWT algorithm allow-list.
	Algorithms []string `controllerconfig:"nodeAgent.tlsJWT.algorithms"`

	// SubjectTrustDomain is the exact SPIFFE trust domain accepted in the
	// verified node-agent JWT subject.
	SubjectTrustDomain string `controllerconfig:"nodeAgent.tlsJWT.subjectTrustDomain"`

	// SubjectTenant is the tenant segment in the local SPIFFE ID exchanged by
	// DSX Identity for node-agent JWT authentication.
	SubjectTenant string `controllerconfig:"nodeAgent.tlsJWT.subjectTenant"`

	// SubjectService is the service segment in the local SPIFFE ID exchanged by
	// DSX Identity for node-agent JWT authentication.
	SubjectService string `controllerconfig:"nodeAgent.tlsJWT.subjectService"`

	// DSXIdentityExchangeURL is injected into node-agent bootstrap arguments.
	DSXIdentityExchangeURL string `controllerconfig:"nodeAgent.tlsJWT.dsxIdentityExchangeURL"`
}

// NodeAgentConfig configures connections to and SPIRE attestation for managed node agents.
type NodeAgentConfig struct {
	// Port is the secure port that the NVKE node agent listens on.
	Port string `env:"PORT" default:"15005"`

	// SpireAttestationMethod selects the SPIRE attestation method pinned to
	// newly created nodes.
	SpireAttestationMethod SpireAttestationMethod `controllerconfig:"nodeAgent.spireAttestationMethod"`

	// DSXImds contains settings specific to dsx_imds attestation.
	DSXImds DSXImdsConfig

	// TLSJWT contains TLS/JWT transport settings.
	TLSJWT TLSJWTConfig

	// UpdateMethod selects apt or bundle installation for opted-in clusters.
	UpdateMethod string `controllerconfig:"nodeAgent.updateMethod"`
}

// Config is the controller's configuration. A field with an env tag comes from
// that environment variable; a field with a controllerconfig tag comes from that
// key in the mounted nvke-controller-config file, which
// controllerconfig.Config.ApplyTo copies in at startup.
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

	// DBQueryLogValues logs each query with its values instead of a "?" for each,
	// and logs PostgreSQL error messages. A query can carry operator-supplied
	// content, and a message can repeat a rejected value, so it is off by default.
	DBQueryLogValues bool `env:"DB_QUERY_LOG_VALUES" default:"false"`

	// DBMaxOpenConnections is the maximum number of open connections to the database
	DBMaxOpenConnections int `env:"DB_MAX_OPEN_CONNECTIONS" default:"32"`

	// ServerListenAddress is the address and port to bind for the HTTP server
	ServerListenAddress string `env:"SERVER_LISTEN_ADDRESS" default:":8080"`

	// MetricsListenAddress is the address and port for the Prometheus metrics
	// server. Defaults to 127.0.0.1:9090 (localhost-only) so the endpoint is
	// not reachable from outside the pod without an explicit override.
	// Set to 0.0.0.0:9090 (paired with a NetworkPolicy) to allow scraping.
	MetricsListenAddress string `controllerconfig:"metricsListenAddress"`

	// TemporalHostPort is the host and port of the Temporal server to use for controller workflows
	TemporalHostPort string `controllerconfig:"temporal.hostPort"`

	// TemporalNamespace is the Temporal namespace to use for controller workflows
	TemporalNamespace string `controllerconfig:"temporal.namespace"`

	// TemporalTaskQueue is the Temporal task queue to use for controller workflows
	TemporalTaskQueue string `controllerconfig:"temporal.taskQueue"`

	// TemporalTLS configures optional mTLS for the controller's Temporal client.
	TemporalTLS TemporalTLSConfig

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

	// AgentReadiness controls node-agent reachability probes independently of tenant health checks.
	AgentReadiness AgentReadinessConfig

	// NicoControllersEnabled enables controllers that watch NICo infrastructure
	// resources. Disable it when the controller manages only provisioned infrastructure.
	NicoControllersEnabled bool `controllerconfig:"nicoControllersEnabled"`
	// KubeVipFMDSEndpoint is the full Forge Metadata Service URL used to resolve
	// the per-node DPU ASN on native-networking sites. When empty, CAPI and
	// subnet-based NICo pools use the static Forge peer ASN.
	KubeVipFMDSEndpoint string `env:"KUBE_VIP_FMDS_ENDPOINT"`

	// ProfilesDir is the directory from which to read configProfiles
	ProfilesDir string `controllerconfig:"profilesDir"`

	// SupportedInfraProviders is the list of provider payload keys this controller
	// deployment advertises for cluster specs.
	SupportedInfraProviders []string `controllerconfig:"supportedInfraProviders"`

	// NodeMetadataAllowlistedLabelKeys is the optional allowlist of label keys a caller
	// may set under the reserved Kubernetes domains (kubernetes.io, k8s.io). Empty, no
	// keys in those domains are allowed. It does not apply to taints.
	NodeMetadataAllowlistedLabelKeys []string `controllerconfig:"nodeMetadataAllowlistedLabelKeys"`

	// RunbooksDir is the optional directory containing config-profile hook scripts.
	// When omitted, hook execution looks under ProfilesDir/runbooks before falling
	// back to controller-owned embedded runbooks.
	RunbooksDir string `controllerconfig:"runbooksDir"`

	// NodeRebootInitialTimeout is the initial reboot verification timeout. Soft
	// verification uses this duration capped at 30 minutes, without retry increments.
	NodeRebootInitialTimeout time.Duration `env:"NODE_REBOOT_INITIAL_TIMEOUT" default:"30m"`

	// NodeRebootTimeoutIncrement is the increment for the node reboot workflow timeout by each attempt.
	NodeRebootTimeoutIncrement time.Duration `env:"NODE_REBOOT_TIMEOUT_INCREMENT" default:"15m"`

	// NodeRebootMaxTimeout is the maximum timeout for the node reboot workflow.
	NodeRebootMaxTimeout time.Duration `env:"NODE_REBOOT_MAX_TIMEOUT" default:"1h"`

	// NodeRebootPollingInterval is the polling interval for the node reboot workflow.
	NodeRebootPollingInterval time.Duration `env:"NODE_REBOOT_POLLING_INTERVAL" default:"1m"`

	// NodeRebootSoftRebootEnabled selects a CAPI node reboot's strategy: true tries the in-guest
	// soft reboot RPC first, falling back to a hard reboot on RPC error or after at most 30 minutes
	// without a verified boot change. false skips straight to the hard CAPI Machine reboot
	// annotation, for any backend where the in-guest reboot path can never produce a real,
	// detectable boot change.
	NodeRebootSoftRebootEnabled bool `env:"NODE_REBOOT_SOFT_REBOOT_ENABLED" default:"true"`

	// NodeProvisioningTimeout is the maximum time a worker node may remain provisioning
	// before the provisioning monitor gives up and recycles it. A value of 0 disables
	// automatic timeout-based provisioning remediation.
	NodeProvisioningTimeout time.Duration `env:"NODE_PROVISIONING_TIMEOUT" default:"2h"`

	// CertRenewalControllerEnabled enables controller-owned periodic Kubernetes control-plane certificate renewal.
	CertRenewalControllerEnabled bool `controllerconfig:"certRenewal.controllerEnabled"`

	// CertRenewalInterval is the interval between certificate renewal checks for each cluster.
	CertRenewalInterval time.Duration `controllerconfig:"certRenewal.interval"`

	// CertRenewalThresholdDays is passed to the certificate-renewal runbook.
	CertRenewalThresholdDays int `controllerconfig:"certRenewal.thresholdDays"`

	// CredentialMonitoringEnabled enables read-only credential inspection independently of renewal.
	CredentialMonitoringEnabled bool `controllerconfig:"credentialMonitoring.enabled"`

	// CertificateInspectionInterval is the interval between node certificate inspections.
	CertificateInspectionInterval time.Duration `controllerconfig:"credentialMonitoring.inspectionInterval"`

	// CertificateObservationMaxAge is the oldest observation that still counts as fresh.
	CertificateObservationMaxAge time.Duration `controllerconfig:"credentialMonitoring.observationMaxAge"`

	// CertificateWarningDays is how many days before expiry a certificate is reported as expiring.
	CertificateWarningDays int `controllerconfig:"credentialMonitoring.warningDays"`

	// NVKESiteControllerName is the name of the site controller
	NVKESiteControllerName string `env:"NKE_SITE_CONTROLLER_NAME"`

	// FieldManagerName is the Server-Side-Apply field manager this controller
	// uses for its own CAPI/NICo patches. Defaults to today's literal so an
	// unset value is today's behavior exactly; a site that renames it changes
	// who future patches say they own, not who owns fields already applied
	// under the old name.
	FieldManagerName string `controllerconfig:"fieldManager.name"`

	// FieldManagerRebootMachineName is the field manager scoped to the CAPI
	// Machine reboot annotation alone, kept separate from FieldManagerName so
	// removing it releases only that annotation.
	FieldManagerRebootMachineName string `controllerconfig:"fieldManager.rebootMachineName"`

	// FieldManagerMachineHealthIssueName is the field manager used when
	// patching a Machine's health-issue annotation.
	FieldManagerMachineHealthIssueName string `controllerconfig:"fieldManager.machineHealthIssueName"`

	// ManagedByLabelValue is the value written under the
	// nvke.nvidia.com/managed-by label key on objects this controller owns,
	// and the value the tenant Secret reconciler filters incoming Secret
	// events on. Both sides must agree, so this is read from one place.
	ManagedByLabelValue string `controllerconfig:"managedByLabelValue"`

	// ManagementNamespace is the shared management-plane namespace holding the
	// tenant-access Secrets, the RBAC ConfigMap and the OIDC-mode CAPI
	// objects. Empty means constants.LegacyManagementNamespace, so a config.yaml
	// written before this key existed keeps today's behavior exactly. Read it
	// through ManagementNamespaceOrDefault rather than directly: unlike the
	// label keys, the old namespace is not also read, so an empty value reaching
	// a client would address the wrong namespace rather than fall back.
	ManagementNamespace string `controllerconfig:"managementNamespace"`

	// RBACConfigMapName is the operator-managed ConfigMap in ManagementNamespace
	// holding the RBAC manifest applied into tenant clusters. Empty means
	// nvkerbac.LegacyConfigMapName, for the same reason ManagementNamespace
	// falls back. Read it through RBACConfigMapSource, which pairs it with the
	// namespace it is resolved against.
	RBACConfigMapName string `controllerconfig:"rbacConfigMapName"`

	// SpiffeEnabled is a feature flag to enable SPIFFE support.
	SpiffeEnabled bool `controllerconfig:"spire.spiffeEnabled"`

	// SpireAgentStartupTimeout is the timeout for the SPIRE agent startup.
	SpireAgentStartupTimeout time.Duration `env:"SPIRE_AGENT_STARTUP_TIMEOUT" default:"30s"`

	// SpireAgentStartupRetryInterval is the interval for retrying the SPIRE agent startup.
	SpireAgentStartupRetryInterval time.Duration `env:"SPIRE_AGENT_STARTUP_RETRY_INTERVAL" default:"1s"`

	// SpiffeEndpointSocket is the socket path for the SPIFFE endpoint.
	SpiffeEndpointSocket string `controllerconfig:"spire.spiffeEndpointSocket"`

	// SpiffeJWTExchangeEndpoint, when set, is the RFC 8693 token-exchange endpoint
	// the exec credential plugin trades its JWT-SVID at before presenting the
	// result to a tenant API server. It is written into the kubeconfigs the
	// tenant-access controller publishes, so it takes effect on the next reconcile.
	//
	// Leave it empty to present the JWT-SVID from the local SPIRE server directly,
	// which is the behavior every site has today. Set it only where the tenant
	// clusters trust the exchange's issuer rather than that SPIRE server's, or
	// the controller's calls to them will start failing authentication.
	SpiffeJWTExchangeEndpoint string `controllerconfig:"spire.spiffeJWTExchangeEndpoint"`

	// SpireServerNamespace is the namespace of the SPIRE server.
	SpireServerNamespace string `controllerconfig:"spire.serverNamespace"`

	// SpireClassName is the SPIRE className used in ClusterStaticEntry CRD entries.
	SpireClassName string `controllerconfig:"spire.className"`

	// SpireTPMPubhashesConfigMapName is the name of the SPIRE configmap to store TPM pubhashes.
	SpireTPMPubhashesConfigMapName string `controllerconfig:"spire.tpmPubhashesConfigMapName"`

	// SpireServerAddress is the address of the SPIRE server for agents to connect to.
	// This needs to be an address that is reachable from the node which at the time of provisioning is not running in k8s e.g. kube-vip or control plane node IP.
	SpireServerAddress string `env:"SPIRE_SERVER_ADDRESS" default:"localhost"`

	// SpireServerPort is the port of the SPIRE server for agents to connect to.
	// This needs to be a port that is exposed externally to the node which at the time of provisioning is not running in k8s e.g. a nodePort value.
	SpireServerPort string `controllerconfig:"spire.serverPort"`

	// SpireServerAPIAddress is the SPIRE server API address used for server-side operations such as issuing join tokens.
	// This should be reachable from the controller. By default we assume SPIRE server is running in-cluster in the spire-server namespace.
	SpireServerAPIAddress string `env:"SPIRE_SERVER_API_ADDRESS" default:"spire-server.spire-server.svc.cluster.local:8081"`

	// SpireJoinTokenTTL is the TTL used when creating SPIRE join tokens via the server API.
	SpireJoinTokenTTL time.Duration `controllerconfig:"spire.joinTokenTTL"`

	NodeAgent NodeAgentConfig `envPrefix:"NODE_AGENT_"`

	// NodeAgentExternalMTLSEnabled selects an externally issued client
	// certificate as the base transport pinned to newly created nodes instead
	// of selecting transport from each node's SPIFFE setting.
	NodeAgentExternalMTLSEnabled bool `controllerconfig:"nodeAgent.externalMTLSEnabled"`

	// DgxcTenantUser is the DGXC tenant SSH username injected into release userData templates.
	DgxcTenantUser string `env:"DGXC_TENANT_USER"`

	// DgxcTenantPassword is the DGXC tenant SSH plaintext password. The controller hashes it at provision time
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

	// InstallCNIAfterInit installs CNI after kubeadm init, from a Debian package
	// rather than a bundle server. It needs the charts package on the node, which
	// Forge user-data installs as of #398.
	//
	// Still off by default: no real site has run this path yet. Turn it on
	// deliberately, so a regression has one obvious cause.
	InstallCNIAfterInit bool `controllerconfig:"installCNIAfterInit"`

	// PodNamespace is the namespace the controller worker runs in.
	PodNamespace string `env:"POD_NAMESPACE" default:"cloud-workflow-engine"`

	// KubeadmImageRepository is the OCI registry kubeadm uses for control-plane
	// component images. Point it at a local mirror for air-gap environments.
	KubeadmImageRepository string `controllerconfig:"kubeadmImageRepository"`

	// CNIImageRegistry is the OCI registry prefix for CNI component images. Point
	// it at a local mirror to redirect CNI image pulls.
	CNIImageRegistry string `controllerconfig:"cniImageRegistry"`

	// NvkeOIDCAuth is the site-wide configuration for OIDC authentication from
	// NVKE management identities to tenant cluster kube-apiservers.
	NvkeOIDCAuth NvkeOIDCAuth `envPrefix:"NKE_OIDC_AUTH_"`

	// TenantAccessEnabled is the site flag for the Nico consume
	// fallback. CAPI clients always read the published nke-system Secret.
	// The TenantAccess controller is constructed whenever a Kubernetes client
	// exists and always publishes nke-system/<name>-kubeconfig; this flag does
	// not gate that writer.
	TenantAccessEnabled bool `controllerconfig:"tenantAccessEnabled"`

	// AuthConfigPath is the mounted YAML file holding the API authentication
	// configuration, for a ConfigMap the chart does not render; the
	// chart-rendered configuration is apiAuth in nvke-controller-config
	// instead, and setting both is a startup failure. Unset, with apiAuth not
	// enabled in nvke-controller-config, disables authentication. A
	// set-but-unreadable path is a startup failure, never "disabled"; see
	// authn.Load.
	//
	// Validate() does not check it: unset is the valid default.
	AuthConfigPath string `env:"AUTH_CONFIG_PATH"`

	// ControllerConfigPath is the nvke-controller-config file every field with
	// a controllerconfig tag comes from. Required: a path that does not load is
	// a startup failure.
	ControllerConfigPath string `env:"CONTROLLER_CONFIG_PATH,notEmpty"`

	// PermissiveMode evaluates and logs authentication and admission decisions
	// without enforcing them, so no request is rejected. No effect while
	// authentication is off: AuthConfigPath unset and apiAuth.enabled false.
	PermissiveMode bool `controllerconfig:"apiAuth.permissive"`

	// Tracing configures the export of OpenTelemetry spans to an OTLP collector.
	Tracing TracingConfig

	// Machine metadata allowlists permit exact keys under reserved domains.
	MachineMetadataAllowlistedLabelKeys      []string `controllerconfig:"machineMetadataAllowlistedLabelKeys"`
	MachineMetadataAllowlistedAnnotationKeys []string `controllerconfig:"machineMetadataAllowlistedAnnotationKeys"`

	// AllowControlPlaneWithoutVIP permits control-plane lifecycle operations without a VIP for development.
	// The API endpoint remains tied to the initializing node's IP.
	AllowControlPlaneWithoutVIP bool `controllerconfig:"allowControlPlaneWithoutVIP"`
}

// TracingConfig configures OpenTelemetry span export.
type TracingConfig struct {
	// Enabled turns on span export and database spans for queries with a trace parent.
	Enabled bool `controllerconfig:"tracing.enabled"`
	// Endpoint is the OTLP gRPC collector host:port (scheme optional) that
	// spans are exported to. When empty, the OTEL_EXPORTER_OTLP_* variables the
	// OpenTelemetry Operator injects are used; with a sidecar this is typically
	// localhost:4317.
	Endpoint string `controllerconfig:"tracing.endpoint"`
	// Insecure uses plaintext OTLP (required for localhost sidecar / in-cluster collectors).
	Insecure bool `controllerconfig:"tracing.insecure"`
	// ServiceName is the resource attribute service.name.
	ServiceName string `controllerconfig:"tracing.serviceName"`
}

// TemporalTLSConfig contains optional file paths for Temporal client mTLS.
type TemporalTLSConfig struct {
	CaCert     string `controllerconfig:"temporal.tls.caCert"`
	ClientKey  string `controllerconfig:"temporal.tls.clientKey"`
	ClientCert string `controllerconfig:"temporal.tls.clientCert"`
	ServerName string `controllerconfig:"temporal.tls.serverName"`
}

type NvkeOIDCAuth struct {
	// Enabled controls whether new clusters receive an apiserver
	// AuthenticationConfiguration for NVKE OIDC identities.
	Enabled bool `controllerconfig:"oidc.managementOIDCAuth.enabled"`
	// AutoMigrationEnabled is reserved for future migration of existing clusters.
	AutoMigrationEnabled bool `env:"AUTO_MIGRATION_ENABLED" default:"false"`
	// IssuerURL is the OIDC issuer trusted by tenant kube-apiservers.
	IssuerURL string `controllerconfig:"oidc.managementOIDCAuth.issuerURL"`
	// DiscoveryURL optionally overrides the issuer discovery endpoint.
	DiscoveryURL string `controllerconfig:"oidc.managementOIDCAuth.discoveryURL"`
	// CABundleSecret points at the Secret containing a PEM CA bundle for the issuer.
	CABundleSecret OIDCCABundleSecretRef
}

type OIDCCABundleSecretRef struct {
	Namespace string `controllerconfig:"oidc.managementOIDCAuth.caBundleSecretRef.namespace"`
	Name      string `controllerconfig:"oidc.managementOIDCAuth.caBundleSecretRef.name"`
	Key       string `controllerconfig:"oidc.managementOIDCAuth.caBundleSecretRef.key"`
}

func (r OIDCCABundleSecretRef) Empty() bool {
	return r.Namespace == "" && r.Name == "" && r.Key == ""
}

// TeleportConfig contains configuration for the Teleport TPM join token integration.
type TeleportConfig struct {
	Enabled      bool   `controllerconfig:"teleport.enabled"`
	IdentityFile string `controllerconfig:"teleport.identityFile"`
	ProxyAddress string `controllerconfig:"teleport.proxyAddress"`
	Environment  string `controllerconfig:"teleport.environment"`

	DefaultClusterEnabled    bool `env:"DEFAULT_CLUSTER_ENABLED" default:"false"`
	DefaultCPPoolEnabled     bool `env:"DEFAULT_CP_POOL_ENABLED" default:"false"`
	DefaultWorkerPoolEnabled bool `env:"DEFAULT_WORKER_POOL_ENABLED" default:"false"`

	// ManagedByLabelValue is the value of the Teleport "managed-by" label this
	// controller stamps on tokens it creates. It is the written value only;
	// ownership checks go through OwnershipManagedByValues, which also accepts
	// AcceptedManagedByLabelValues.
	ManagedByLabelValue string `controllerconfig:"teleport.managedByLabelValue"`

	// AcceptedManagedByLabelValues are additional "managed-by" values this
	// controller still owns without writing: the spellings its own earlier
	// releases stamped. Moving ManagedByLabelValue does not relabel the tokens
	// and nodes already carrying the old one -- a Teleport token's labels are
	// fixed at mint, a node's at join -- so without this the move disowns every
	// one of them, and nothing reaps what nothing owns. List the previous value
	// here for as long as any resource may still carry it.
	AcceptedManagedByLabelValues []string `controllerconfig:"teleport.acceptedManagedByLabelValues"`

	// TokenNamePrefix prefixes the deterministic Teleport TPM join token name
	// this controller mints for a node.
	TokenNamePrefix string `controllerconfig:"teleport.tokenNamePrefix"`

	// ResourceGroupLabelValue is the value of the Teleport "resource-group"
	// label stamped on tokens and node userdata. Existing sites pin "nke".
	ResourceGroupLabelValue string `controllerconfig:"teleport.resourceGroupLabelValue"`
}

// OwnershipManagedByValues returns every "managed-by" value this controller
// owns: the one it writes first, then the ones it only accepts. Duplicates and
// blanks are dropped, so a deployment that lists its current value among the
// accepted ones gets the same answer as one that does not.
func (c TeleportConfig) OwnershipManagedByValues() []string {
	values := make([]string, 0, len(c.AcceptedManagedByLabelValues)+1)
	for _, v := range append([]string{c.ManagedByLabelValue}, c.AcceptedManagedByLabelValues...) {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(values, v) {
			values = append(values, v)
		}
	}
	return values
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

// ManagementNamespaceOrDefault returns the shared management-plane namespace,
// falling back to constants.LegacyManagementNamespace when ManagementNamespace is
// unset. The chart renders the key on every install, so the fallback is for a
// config.yaml written before it existed and for a zero Config in tests; it is
// deliberately the legacy name, because a site that has not pinned a namespace
// must not be moved to a new one by a binary upgrade alone.
func (c Config) ManagementNamespaceOrDefault() string {
	if c.ManagementNamespace == "" {
		return constants.LegacyManagementNamespace
	}
	return c.ManagementNamespace
}

// RBACConfigMapSource returns the ConfigMap holding the tenant RBAC manifest:
// the management namespace and the name together, because a name resolved
// against the wrong namespace reads nothing. The name falls back to
// nvkerbac.LegacyConfigMapName when unset, for the reason
// ManagementNamespaceOrDefault gives.
func (c Config) RBACConfigMapSource() nvkerbac.Source {
	name := c.RBACConfigMapName
	if name == "" {
		name = nvkerbac.LegacyConfigMapName
	}
	return nvkerbac.Source{Namespace: c.ManagementNamespaceOrDefault(), Name: name}
}

// Validate checks the settings the environment supplies. nvke-controller-config
// validates its own when it loads.
func (c *Config) Validate() error {
	if err := c.AgentReadiness.Validate(); err != nil {
		return err
	}
	if err := c.validateDBCredentials(); err != nil {
		return err
	}
	if err := c.validateCredentialMonitoring(); err != nil {
		return err
	}
	if c.SpireServerAPIAddress == "" {
		return fmt.Errorf("SPIRE_SERVER_API_ADDRESS is required")
	}
	return nil
}

// EffectiveRunbooksDir returns the configured profile-hook runbook directory.
func (c Config) EffectiveRunbooksDir() string {
	if c.RunbooksDir != "" {
		return c.RunbooksDir
	}
	return filepath.Join(c.ProfilesDir, "runbooks")
}

// ParseConfigFromEnv parses the fields with an env tag. It does not validate:
// the caller applies nvke-controller-config first, then calls Validate.
func ParseConfigFromEnv() (Config, error) {
	cfg, err := env.ParseAsWithOptions[Config](env.Options{
		DefaultValueTagName: "default",
	})
	if err != nil {
		return Config{}, fmt.Errorf("failed to parse config from environment: %w", err)
	}
	return cfg, nil
}
