// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package siteconfig loads and validates the declarative site spec file that
// drives "nvkectl bootstrap site".
//
// The site spec file describes a set of pre-provisioned nodes and the vanilla
// Kubernetes cluster to build on them. It is the single source of truth for a
// bootstrap run: the command loads it, validates it, and fails fast with a
// clear error before dispatching any workflow or touching any node.
//
// Environment variables (see envBindings) override the matching field from the
// file, so existing tier-0 / DS9 scripts can drive the site spec via the
// environment. A set variable takes precedence over the file value.
//
// The schema deliberately mirrors the field names of the tier-0 bootstrap
// template so operators do not need to relearn them, but drops the
// NVIDIA-specific and deploy-time fields (SWE image tag, ArgoCD repos,
// embedded database credentials) so the file can be open-sourced. See
// docs/sitespec.md for the full field reference.
package sitespec

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/util/cert"

	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
)

const (
	// DefaultNodeAgentPort is the gRPC port nvke-agent listens on when the
	// site spec does not set cluster.nodeAgentPort.
	DefaultNodeAgentPort = "15005"

	// DefaultCNIName is the CNI installed when cluster.cniName is omitted.
	DefaultCNIName = "ovn-kubernetes"

	// DefaultEtcdEncryption is what cluster.etcdEncryption means when the site
	// spec leaves it unset.
	DefaultEtcdEncryption = true

	// SiteSpec field paths — the single source of truth for the dotted paths
	// shared by the environment bindings and the validator messages.
	pathClusterName          = "clusterName"
	pathInfraProvider        = "infra.provider"
	pathNodes                = "cluster.nodes"
	pathNico                 = "infra.nico"
	pathNicoRemote           = "infra.nico.remote"
	pathNicoControlPlane     = "infra.nico.controlPlane"
	pathNicoReleases         = "infra.nico.releases"
	pathK8sVersion           = "cluster.k8sVersion"
	pathCniName              = "cluster.cniName"
	pathCniVersion           = "cluster.cniVersion"
	pathControlPlaneVIP      = "cluster.controlPlaneVIP"
	pathPodCidr              = "cluster.podCidr"
	pathServiceCidr          = "cluster.serviceCidr"
	pathSystemReservedMemory = "cluster.kubeletConfig.systemReservedMemory"
	pathNodeAgentPort        = "cluster.nodeAgentPort"
	pathEtcdEncryption       = "cluster.etcdEncryption"
	pathSpireTrustDomain     = "spire.trustDomain"
	pathSpireUpstreamCA      = "spire.upstreamCA"
	pathOIDCProviders        = "cluster.oidcProviders"

	// maxOIDCProviders matches controllerconfig.MaxOIDCProviders.
	maxOIDCProviders = 5

	// oidcAuthConfigMinVersion is the first Kubernetes release that can read the
	// AuthenticationConfiguration the controller renders. Below it the controller
	// pins an empty provider list and the site API server never sees the ones
	// the site spec asked for.
	oidcAuthConfigMinVersion = "1.34.0"

	// Identity namespaces a fleet-wide OIDC provider must not use as a
	// usernamePrefix or groupsPrefix. kube-apiserver rejects system, kubeadm,
	// nke, and nvidia; nvke is reserved the same way.
	oidcPrefixSystem  = "system:"
	oidcPrefixKubeadm = "kubeadm:"
	oidcPrefixNKE     = "nke:"
	oidcPrefixNVKE    = "nvke:"
	oidcPrefixNVIDIA  = "nvidia:"

	pathSpireExternalAddress = "spire.externalAddress"

	// The SPIRE CA comes from the environment, so the site spec can be committed.
	EnvSpireCACertFile = "NVKECTL_SPIRE_CA_CERT_FILE" //nolint:gosec // G101: an env var name, not a credential
	EnvSpireCAKeyFile  = "NVKECTL_SPIRE_CA_KEY_FILE"  //nolint:gosec // G101: an env var name, not a credential
)

// removedSpireCAEnv pairs the CA variables main bound to the removed spec fields
// with their replacements. Setting one is refused, so a script that still
// exports it does not get a self-signed SPIRE.
var removedSpireCAEnv = [][2]string{
	{"SPIRE_CA_CERT_FILE", EnvSpireCACertFile},
	{"SPIRE_CA_KEY_FILE", EnvSpireCAKeyFile},
}

// removedFields maps each removed field to what replaced it.
var removedFields = map[string]string{
	"registry":          "the bootstrap takes its images from --nvke-controller-image and --provisioned-provider-image; log in to a private registry with nerdctl login",
	"spire.caCertFile":  "set " + EnvSpireCACertFile + " and spire.upstreamCA: true",
	"spire.caKeyFile":   "set " + EnvSpireCAKeyFile + " and spire.upstreamCA: true",
	"infra.nico.tenant": "set " + stackconfig.EnvNodeUser + " and " + stackconfig.EnvNodePassword,
}

var (
	oidcReservedPrefixes = []string{
		oidcPrefixSystem,
		oidcPrefixKubeadm,
		oidcPrefixNKE,
		oidcPrefixNVKE,
		oidcPrefixNVIDIA,
	}

	// k8sVersionRE matches a plain semantic version like "1.32.0". A leading
	// "v" is not accepted — the bootstrap flow passes this string straight to
	// kubeadm.
	k8sVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

	// dnsLabelRE matches an RFC 1123 DNS label, used for cluster and node names.
	dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	// tpmEKThumbprintRE matches the SHA-256 of a TPM endorsement key, as
	// printed by spire-tpm-plugin's get_tpm_pubhash: 64 lowercase hex digits.
	tpmEKThumbprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// SupportedCNIs returns the CNI plugins the bootstrap flow can install.
func SupportedCNIs() []string {
	return []string{DefaultCNIName}
}

// SiteSpec is the declarative bootstrap input file.
type SiteSpec struct {
	// ClusterName is the name of the Kubernetes cluster to create.
	ClusterName string `yaml:"clusterName"`
	// Infra selects where the cluster's machines come from and holds what that
	// provider needs. Omitting it is the provisioned provider.
	Infra Infra `yaml:"infra,omitempty"`
	// Cluster holds the node inventory and Kubernetes settings.
	Cluster Cluster `yaml:"cluster"`
	// Spire holds the identity-plane settings for the bootstrap.
	Spire Spire `yaml:"spire"`

	// dir is the directory the file was loaded from, which relative paths in
	// infra.nico.releases are resolved against. Empty for a spec that was
	// built rather than loaded.
	dir string `yaml:"-"`
}

// ResolvePath returns path made absolute: as given when absolute, else joined
// onto the directory the site spec was loaded from, else onto the working
// directory.
func (inv *SiteSpec) ResolvePath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if inv.dir != "" {
		return filepath.Join(inv.dir, path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Spire describes the identity plane `nvkectl bootstrap site` stands up.
type Spire struct {
	// TrustDomain is the SPIFFE trust domain used for all identities that the
	// bootstrap SPIRE server issues. It must also be used by the permanent
	// site SPIRE server that replaces it.
	TrustDomain string `yaml:"trustDomain,omitempty"`

	// UpstreamCA records that the site's SPIRE chains to an operator CA, so
	// every run requires the CA variables instead of falling back to a
	// self-signed root.
	UpstreamCA bool `yaml:"upstreamCA,omitempty"`

	// CACertFile and CAKeyFile are a CA the operator brings to the bootstrap,
	// from NVKECTL_SPIRE_CA_CERT_FILE and NVKECTL_SPIRE_CA_KEY_FILE. SPIRE
	// chains its own CA to it, so a node given the certificate ahead of time
	// verifies the server on first contact. Both with UpstreamCA, neither
	// without it; neither leaves SPIRE self-signed.
	CACertFile string `yaml:"-"`
	CAKeyFile  string `yaml:"-"`

	// ExternalAddress is the address nodes use to reach the bootstrap SPIRE
	// server: the host that runs the bootstrap, as seen from the network the
	// nodes are on. An IP address or a DNS name.
	//
	// Required with ProviderNico, where CWE writes the SPIRE server's
	// address into each node's userdata, so it has to be one the node can reach
	// before anything in the cluster exists. Not used with
	// ProviderProvisioned: those hosts were given the address when they
	// were provisioned.
	ExternalAddress string `yaml:"externalAddress,omitempty"`
}

// Cluster describes the target Kubernetes cluster and its nodes.
type Cluster struct {
	// Nodes are the pre-provisioned control-plane nodes. The site spec does not
	// select which node runs kubeadm init. Used only with
	// ProviderProvisioned.
	Nodes []Node `yaml:"nodes"`
	// K8sVersion is the Kubernetes version to install, e.g. "1.32.0".
	K8sVersion string `yaml:"k8sVersion"`
	// CniName selects the CNI plugin; see SupportedCNIs. Empty means DefaultCNIName.
	CniName string `yaml:"cniName"`
	// CniVersion is an optional OVN-Kubernetes version (for example "1.4.0").
	// Empty lets the node-agent use its compiled-in default.
	CniVersion string `yaml:"cniVersion"`
	// ControlPlaneVIP is an IPv4 API endpoint advertised by kube-vip over BGP.
	// It may be empty only for a single-node control plane, which then uses the
	// initializing node's IP as its fixed endpoint.
	ControlPlaneVIP string `yaml:"controlPlaneVIP,omitempty"`
	// PodCidr is the pod network CIDR. Multiple CIDRs may be given as a
	// comma-separated string.
	PodCidr string `yaml:"podCidr"`
	// ServiceCidr is the service network CIDR.
	ServiceCidr string `yaml:"serviceCidr"`
	// NodeAgentPort is the gRPC port nvke-agent listens on. Defaults to
	// DefaultNodeAgentPort when unset.
	NodeAgentPort string `yaml:"nodeAgentPort"`
	// EtcdEncryption enables encryption at rest for the Kubernetes API's etcd
	// data. A pointer so "not set" can be told from "set to false": the
	// default is on, and an explicit false is permanent — a cluster built
	// without encryption cannot be encrypted afterwards.
	EtcdEncryption *bool `yaml:"etcdEncryption,omitempty"`
	// KubeletConfig holds optional kubelet resource reservations.
	KubeletConfig KubeletConfig `yaml:"kubeletConfig"`
	// OIDC providers the bootstrap controller
	// pins onto the site cluster apiservers. Optional.
	OIDCProviders []OIDCProvider `yaml:"oidcProviders,omitempty"`
}

// OIDCProvider is one fleet-wide OIDC provider. Field names match the
// controller chart's oidc.providers entries.
type OIDCProvider struct {
	Name           string            `yaml:"name"`
	IssuerURL      string            `yaml:"issuerURL"`
	Audiences      []string          `yaml:"audiences"`
	UsernameClaim  string            `yaml:"usernameClaim"`
	UsernamePrefix *string           `yaml:"usernamePrefix"`
	GroupsClaim    string            `yaml:"groupsClaim,omitempty"`
	GroupsPrefix   string            `yaml:"groupsPrefix,omitempty"`
	RequiredClaims map[string]string `yaml:"requiredClaims,omitempty"`
	CABundle       string            `yaml:"caBundle"`
	DiscoveryURL   string            `yaml:"discoveryURL,omitempty"`
}

// EtcdEncryptionEnabled reports whether the cluster's etcd data is encrypted
// at rest. Unset means enabled; see EtcdEncryption.
func (c Cluster) EtcdEncryptionEnabled() bool {
	return c.EtcdEncryption == nil || *c.EtcdEncryption
}

// ControlPlaneSchedulable reports whether the control-plane NoSchedule taint
// is removed from every node after bootstrap. It is removed when the site spec
// declares no worker nodes.
func (c Cluster) ControlPlaneSchedulable() bool {
	// The site spec cannot declare worker nodes yet. Derive this from that
	// list once it exists.
	return true
}

// Node is a single pre-provisioned host.
type Node struct {
	Name string `yaml:"name"`
	IP   string `yaml:"ip"`

	// TPMEKThumbprint is the SHA-256 of this host's TPM endorsement key,
	// as printed by spire-tpm-plugin's get_tpm_pubhash.
	//
	// It is what the SPIRE server's allowlist is keyed on, and it identifies
	// the machine rather than anything about its software: an EK is burned
	// into the TPM and cannot be moved or changed. Reading it is an
	// out-of-band step before the bootstrap — for DSX, the ansible runbook
	// collects it from each host over SSH.
	//
	// Optional. A node without one is not admitted by TPM attestation, and a
	// bootstrap where no node has one deploys SPIRE with no TPM attestor at
	// all. It is not a secret: it is a public key hash, so it belongs in the
	// site spec rather than beside the credentials.
	TPMEKThumbprint string `yaml:"tpmEkThumbprint,omitempty"`
}

// TPMEKThumbprints returns every node's EK thumbprint, in inventory order,
// skipping nodes that have none. This is the allowlist the SPIRE server is
// installed with.
func (c Cluster) TPMEKThumbprints() []string {
	thumbprints := make([]string, 0, len(c.Nodes))
	for _, node := range c.Nodes {
		if node.TPMEKThumbprint != "" {
			thumbprints = append(thumbprints, node.TPMEKThumbprint)
		}
	}
	return thumbprints
}

// KubeletConfig holds the kubelet reservations honored in M0.
type KubeletConfig struct {
	// SystemReservedMemory is the kubelet system-reserved memory (e.g. "16Gi").
	SystemReservedMemory string `yaml:"systemReservedMemory"`
}

// envBindings maps environment variable names to their site spec field paths.
// A set variable overrides the matching field from the file, so existing
// tier-0 / DS9 scripts can drive the site spec through the environment.
func envBindings() map[string]string {
	return map[string]string{
		"CLUSTER_NAME":                   pathClusterName,
		"NVKECTL_INFRA_PROVIDER":         pathInfraProvider,
		"CLUSTER_K8S_VERSION":            pathK8sVersion,
		"CLUSTER_CNI_NAME":               pathCniName,
		"CLUSTER_CNI_VERSION":            pathCniVersion,
		"CLUSTER_CONTROL_PLANE_VIP":      pathControlPlaneVIP,
		"CLUSTER_POD_CIDR":               pathPodCidr,
		"CLUSTER_SERVICE_CIDR":           pathServiceCidr,
		"CLUSTER_SYSTEM_RESERVED_MEMORY": pathSystemReservedMemory,
		"CLUSTER_NODE_AGENT_PORT":        pathNodeAgentPort,
		"CLUSTER_ETCD_ENCRYPTION":        pathEtcdEncryption,
		"NVKECTL_SPIFFE_TRUST_DOMAIN":    pathSpireTrustDomain,
		"NVKECTL_SPIRE_EXTERNAL_ADDRESS": pathSpireExternalAddress,
	}
}

// Settings is every NVKECTL_* variable this package reads; each is also read
// under its previous NKECTL_* name for one release (see envalias). The one
// exception is NicoInsecureEndpointEnv, which turns a safety check off and so
// answers only to its own name.
var Settings = []string{
	"NVKECTL_SPIFFE_TRUST_DOMAIN",
	"NVKECTL_CLUSTER_NODES",
	"NVKECTL_INFRA_PROVIDER",
	"NVKECTL_SPIRE_EXTERNAL_ADDRESS",
	EnvSpireCACertFile,
	EnvSpireCAKeyFile,
	NicoCredentialsEnv,
	stackconfig.EnvNodeUser,
	stackconfig.EnvNodePassword,
}

// newInventoryViper returns a Viper that reads a YAML site spec file with the
// environment-variable overrides in envBindings bound on top. Each binding is
// explicit (BindEnv); AutomaticEnv is intentionally not used, as it cannot map
// the dotted, camelCase field paths to valid environment variable names.
func newInventoryViper() *viper.Viper {
	v := viper.New()
	v.SetConfigType("yaml")
	for env, path := range envBindings() {
		// A setting the rename touched is also read under its previous name,
		// for one release; the current name wins when both are set.
		if old := envalias.Old(env); old != "" {
			_ = v.BindEnv(path, env, old)
		} else {
			_ = v.BindEnv(path, env)
		}
	}
	return v
}

type loadOptions struct {
	allowControlPlaneWithoutVIP bool
}

// LoadOption configures site-spec loading.
type LoadOption func(*loadOptions)

// WithAllowControlPlaneWithoutVIP permits a multi-node site to omit its VIP during development.
// A configured VIP still receives normal address validation.
func WithAllowControlPlaneWithoutVIP(allow bool) LoadOption {
	return func(options *loadOptions) {
		options.allowControlPlaneWithoutVIP = allow
	}
}

// Load reads the site spec file, overlays bound environment variables, applies
// defaults, and validates the result. It returns a fully validated SiteSpec
// or an aggregated error describing every problem found. A nil error means the
// site spec is safe to act on.
func Load(path string, options ...LoadOption) (*SiteSpec, error) {
	return load(path, true, options...)
}

// LoadDescription reads the site spec as Load does but without the SPIRE CA:
// the CA variables are not read and the files are not opened. Spire.UpstreamCA
// still says whether the site uses one. For a caller that describes the site
// and never acts on it, such as a render that writes no credential.
func LoadDescription(path string, options ...LoadOption) (*SiteSpec, error) {
	return load(path, false, options...)
}

func load(path string, withCA bool, options ...LoadOption) (*SiteSpec, error) {
	var opts loadOptions
	for _, option := range options {
		option(&opts)
	}
	v := newInventoryViper()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read site spec file %s: %w", path, err)
	}

	// NVKECTL_CLUSTER_NODES="name:ip,name:ip" supplies the flat node list when
	// the file lists none. A malformed entry fails loudly rather than being
	// silently dropped.
	if nodesEnv := envalias.Get("NVKECTL_CLUSTER_NODES"); nodesEnv != "" && !v.IsSet(pathNodes) {
		nodes, err := parseNodesEnv(nodesEnv)
		if err != nil {
			return nil, err
		}
		v.Set(pathNodes, nodes)
	}

	// The decoder reads "" as false for a bool. Here that would disable
	// encryption silently and for the life of the cluster, and "" is what an
	// unset template variable produces. Refuse it before decoding loses it.
	if raw, ok := v.Get(pathEtcdEncryption).(string); ok && raw == "" {
		return nil, fmt.Errorf("site spec file %s: %s: %q is not a boolean (true or false)",
			path, pathEtcdEncryption, raw)
	}

	if err := rejectRemovedFields(v); err != nil {
		return nil, fmt.Errorf("site spec file %s: %w", path, err)
	}

	var inv SiteSpec
	if err := v.Unmarshal(&inv, useYAMLTags, decodeTextValues, rejectUnknownFields); err != nil {
		return nil, fmt.Errorf("site spec file %s: %w", path, err)
	}
	if abs, err := filepath.Abs(path); err == nil {
		inv.dir = filepath.Dir(abs)
	}
	if withCA {
		inv.Spire.CACertFile, inv.Spire.CAKeyFile = envalias.Get(EnvSpireCACertFile), envalias.Get(EnvSpireCAKeyFile)
	}

	inv.SetDefaults()

	// Both check sets share one validator so the caller still sees every
	// problem in one pass, as it did before the two were separated.
	checks := &validator{allowControlPlaneWithoutVIP: opts.allowControlPlaneWithoutVIP}
	inv.appendDataChecks(checks)
	if withCA {
		inv.appendReferenceChecks(checks)
	}
	if err := checks.err(); err != nil {
		return nil, fmt.Errorf("site spec file %s: %w", path, err)
	}
	return &inv, nil
}

// rejectRemovedFields names the replacement for each removed field the file sets.
func rejectRemovedFields(v *viper.Viper) error {
	var errs []string
	for field, instead := range removedFields {
		if v.InConfig(field) {
			errs = append(errs, fmt.Sprintf("%s is no longer accepted: %s", field, instead))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

// useYAMLTags makes viper decode using the structs' existing yaml tags. Embedded
// structs are squashed, as yaml.v3's ",inline" does when the spec is written
// back out, since mapstructure does not read that tag option.
func useYAMLTags(dc *mapstructure.DecoderConfig) {
	dc.TagName = "yaml"
	dc.Squash = true
}

// decodeTextValues lets a string decode into a type that parses itself, which
// is how the API's UUID fields read from the file. It adds to viper's own
// hooks rather than replacing them.
func decodeTextValues(dc *mapstructure.DecoderConfig) {
	dc.DecodeHook = mapstructure.ComposeDecodeHookFunc(dc.DecodeHook, mapstructure.TextUnmarshallerHookFunc())
}

// rejectUnknownFields makes decoding fail on an unknown field, so a typo in a
// field name surfaces immediately instead of being silently ignored.
func rejectUnknownFields(dc *mapstructure.DecoderConfig) { dc.ErrorUnused = true }

// parseNodesEnv parses NVKECTL_CLUSTER_NODES="name:ip,name:ip" into node maps,
// returning an error on any malformed entry.
func parseNodesEnv(s string) ([]map[string]string, error) {
	var nodes []map[string]string
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, ip, ok := strings.Cut(entry, ":")
		name, ip = strings.TrimSpace(name), strings.TrimSpace(ip)
		if !ok || name == "" || ip == "" {
			return nil, fmt.Errorf("NVKECTL_CLUSTER_NODES entry %q: expected \"name:ip\"", entry)
		}
		nodes = append(nodes, map[string]string{"name": name, "ip": ip})
	}
	return nodes, nil
}

// SetDefaults fills in optional fields that have a well-known default.
//
// It intentionally never fills in credentials. tier-0's SetDefaults() injected
// well-known database passwords when the database block was omitted; that
// behavior is deliberately not carried over — a missing secret stays empty so
// it can be supplied explicitly (flag or environment variable) rather than
// silently falling back to a shared default.
func (inv *SiteSpec) SetDefaults() {
	if inv.Infra.Provider == "" {
		inv.Infra.Provider = ProviderProvisioned
	}
	if inv.Cluster.NodeAgentPort == "" {
		inv.Cluster.NodeAgentPort = DefaultNodeAgentPort
	}
	if inv.Cluster.CniName == "" {
		inv.Cluster.CniName = DefaultCNIName
	}
	if inv.Cluster.EtcdEncryption == nil {
		enabled := DefaultEtcdEncryption
		inv.Cluster.EtcdEncryption = &enabled
	}
}

// Validate checks that every mandatory field is present and well-formed and
// returns a single error aggregating all problems.
//
// It reads nothing outside the SiteSpec, so it holds wherever the site spec is
// handled: on the machine that writes the file as much as on the one that acts
// on it. Checks that need the filesystem live in ValidateReferences.
func (inv *SiteSpec) Validate() error {
	v := &validator{}
	inv.appendDataChecks(v)
	return v.err()
}

// appendDataChecks runs every check that reads only the SiteSpec, adding each
// problem to v. It takes the validator rather than returning an error so a
// caller that runs more than one check set still reports one aggregated error.
func (inv *SiteSpec) appendDataChecks(v *validator) {
	v.checkClusterName(inv.ClusterName)
	v.checkInfra(inv)
	v.checkK8sVersion(inv.Cluster.K8sVersion)
	v.checkCNI(inv.Cluster.CniName)
	v.checkCIDR(pathPodCidr, inv.Cluster.PodCidr, true)
	v.checkCIDR(pathServiceCidr, inv.Cluster.ServiceCidr, false)
	v.checkCIDRsDisjoint(inv.Cluster)
	v.checkControlPlaneVIP(inv)
	v.checkNodeAgentPort(inv.Cluster.NodeAgentPort)
	v.checkSpire(inv.Spire)
	v.checkOIDCProviders(inv.Cluster)
	v.checkSpireExternalAddress(inv)
}

// ValidateReferences checks that the files the site spec names can be read,
// and returns a single error aggregating all problems.
//
// Call it from the command that is about to act on the sitespec. A caller that
// only writes the site spec out should not: the files it names are needed where
// the bootstrap runs, which is not necessarily where the file was produced.
func (inv *SiteSpec) ValidateReferences() error {
	v := &validator{}
	inv.appendReferenceChecks(v)
	return v.err()
}

// appendReferenceChecks runs every check that reads the filesystem, adding each
// problem to v. See appendDataChecks for why it takes the validator.
func (inv *SiteSpec) appendReferenceChecks(v *validator) {
	v.checkSpireCA(inv.Spire)
}

// ValidateBootstrapReferences checks what the bootstrap reads besides the site
// spec: that the infra.nico.releases manifests can be read and hold the release
// the control plane's release names, with relative paths resolved from the site
// spec's directory; that NVKECTL_NICO_CREDENTIALS names a readable NICo token
// file; and that NVKECTL_NODE_USER and NVKECTL_NODE_PASSWORD are set.
//
// Load does not call it, because Load also serves commands that never run the
// bootstrap: the controller stack install on the site node, through
// render deploy-path, has no use for these files and may not have them. Call it
// from the commands that run, or vet, the bootstrap. It reads nothing with
// the provisioned provider.
func (inv *SiteSpec) ValidateBootstrapReferences() error {
	v := &validator{}
	v.checkNicoFiles(inv)
	v.checkNicoCredentials(inv)
	return v.err()
}

// validator accumulates field-level errors so the caller sees every problem
// in one pass rather than one-at-a-time.
type validator struct {
	errs                        []string
	allowControlPlaneWithoutVIP bool
}

func (v *validator) addf(format string, args ...any) {
	v.errs = append(v.errs, fmt.Sprintf(format, args...))
}

func (v *validator) err() error {
	if len(v.errs) == 0 {
		return nil
	}
	return fmt.Errorf("validation failed:\n  - %s", strings.Join(v.errs, "\n  - "))
}

func (v *validator) checkClusterName(name string) {
	switch {
	case name == "":
		v.addf("clusterName is required")
	case !dnsLabelRE.MatchString(name):
		v.addf("clusterName %q must be a lowercase RFC 1123 label (letters, digits, hyphens)", name)
	}
}

func (v *validator) checkUUID(path string, value openapi_types.UUID) {
	if value == (openapi_types.UUID{}) {
		v.addf("%s is required", path)
	}
}

func (v *validator) checkUUIDs(path string, values []openapi_types.UUID) {
	for i, value := range values {
		if value == (openapi_types.UUID{}) {
			v.addf("%s[%d] is the nil UUID", path, i)
		}
	}
}

// checkNodes validates every node and rejects duplicate names or IPs across
// the whole set.
func (v *validator) checkNodes(c Cluster) {
	if len(c.Nodes) == 0 {
		v.addf("%s must contain at least one node", pathNodes)
		return
	}
	if len(c.Nodes)%2 == 0 {
		v.addf("%s must contain an odd number of nodes", pathNodes)
	}

	names := map[string]int{}
	ips := map[string]int{}

	for i, n := range c.Nodes {
		v.checkNode(fmt.Sprintf("%s[%d]", pathNodes, i), n, names, ips)
	}

	for name, count := range names {
		if count > 1 {
			v.addf("node name %q is used %d times; names must be unique", name, count)
		}
	}
	for ip, count := range ips {
		if count > 1 {
			v.addf("node ip %q is used %d times; IPs must be unique", ip, count)
		}
	}
}

func (v *validator) checkNode(path string, n Node, names, ips map[string]int) {
	switch {
	case n.Name == "":
		v.addf("%s.name is required", path)
	case !dnsLabelRE.MatchString(n.Name):
		v.addf("%s.name %q must be a lowercase RFC 1123 label", path, n.Name)
	default:
		names[n.Name]++
	}

	switch {
	case n.IP == "":
		v.addf("%s.ip is required", path)
	case net.ParseIP(n.IP) == nil:
		v.addf("%s.ip %q is not a valid IP address", path, n.IP)
	default:
		ips[n.IP]++
	}

	// Optional, but a malformed one is worth catching here: it would otherwise
	// be seeded into the SPIRE allowlist, match nothing, and surface much later
	// as that node failing attestation for no visible reason.
	if n.TPMEKThumbprint != "" && !tpmEKThumbprintRE.MatchString(n.TPMEKThumbprint) {
		v.addf("%s.tpmEkThumbprint %q must be 64 lowercase hex digits (the SHA-256 of the node's TPM endorsement key)",
			path, n.TPMEKThumbprint)
	}
}

// checkOIDCProviders records invalid provider settings and duplicate names or URLs.
func (v *validator) checkOIDCProviders(c Cluster) {
	if len(c.OIDCProviders) == 0 {
		return
	}
	if got, err := version.ParseGeneric(c.K8sVersion); err == nil && got.LessThan(version.MustParseGeneric(oidcAuthConfigMinVersion)) {
		v.addf("%s requires %s %s or newer, got %q", pathOIDCProviders, pathK8sVersion, oidcAuthConfigMinVersion, c.K8sVersion)
	}
	if len(c.OIDCProviders) > maxOIDCProviders {
		v.addf("%s has %d entries, more than the %d-entry cap", pathOIDCProviders, len(c.OIDCProviders), maxOIDCProviders)
	}

	seenNames := map[string]int{}
	seenIssuers := map[string]int{}
	seenDiscoveryURLs := map[string]int{}
	for i, p := range c.OIDCProviders {
		path := fmt.Sprintf("%s[%d]", pathOIDCProviders, i)
		name := strings.TrimSpace(p.Name)
		if name == "" {
			v.addf("%s.name is required", path)
		} else if prev, dup := seenNames[name]; dup {
			v.addf("%s.name %q is already used by %s[%d]", path, name, pathOIDCProviders, prev)
		} else {
			seenNames[name] = i
		}
		issuerURL := strings.TrimSpace(p.IssuerURL)
		if issuerURL == "" {
			v.addf("%s.issuerURL is required", path)
		} else {
			if u, err := url.Parse(issuerURL); err != nil || u.Scheme != "https" || u.Host == "" {
				v.addf("%s.issuerURL %q must be an https URL", path, issuerURL)
			}
			if prev, dup := seenIssuers[issuerURL]; dup {
				v.addf("%s.issuerURL %q is already used by %s[%d]", path, issuerURL, pathOIDCProviders, prev)
			} else {
				seenIssuers[issuerURL] = i
			}
		}
		if p.DiscoveryURL != "" {
			v.checkOIDCDiscoveryURL(path+".discoveryURL", issuerURL, p.DiscoveryURL)
			if prev, dup := seenDiscoveryURLs[p.DiscoveryURL]; dup {
				v.addf("%s.discoveryURL %q is already used by %s[%d]", path, p.DiscoveryURL, pathOIDCProviders, prev)
			} else {
				seenDiscoveryURLs[p.DiscoveryURL] = i
			}
		}
		if strings.TrimSpace(p.UsernameClaim) == "" {
			v.addf("%s.usernameClaim is required", path)
		}
		// Kubernetes requires prefix when claim is set; an empty string explicitly disables prefixing.
		if p.UsernamePrefix == nil {
			v.addf("%s.usernamePrefix is required; use \"\" for no prefix", path)
		} else {
			v.checkOIDCPrefix(path+".usernamePrefix", *p.UsernamePrefix)
		}
		v.checkOIDCPrefix(path+".groupsPrefix", p.GroupsPrefix)
		if len(p.Audiences) == 0 {
			v.addf("%s.audiences is required", path)
		}
		for j, aud := range p.Audiences {
			if strings.TrimSpace(aud) == "" {
				v.addf("%s.audiences[%d] must not be empty", path, j)
			}
		}
		if strings.TrimSpace(p.CABundle) == "" {
			v.addf("%s.caBundle is required", path)
		} else if _, err := cert.ParseCertsPEM([]byte(p.CABundle)); err != nil {
			v.addf("%s.caBundle must contain a PEM-encoded certificate", path)
		}
	}
}

// checkOIDCDiscoveryURL records discovery overrides that cannot be used by kube-apiserver.
func (v *validator) checkOIDCDiscoveryURL(path, issuerURL, discoveryURL string) {
	if strings.TrimSpace(discoveryURL) != discoveryURL {
		v.addf("%s must not contain leading or trailing whitespace", path)
		return
	}
	u, err := url.Parse(discoveryURL)
	if err != nil {
		v.addf("%s %q is not a URL: %v", path, discoveryURL, err)
		return
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			v.addf("%s port %q must be between 1 and 65535", path, port)
			return
		}
	}
	switch {
	case u.Scheme != "https" || u.Hostname() == "":
		v.addf("%s %q must be an absolute https URL", path, discoveryURL)
	case u.User != nil:
		v.addf("%s must not contain a username or password", path)
	case u.RawQuery != "" || u.Fragment != "":
		v.addf("%s %q must not contain a query or fragment", path, discoveryURL)
	case strings.TrimRight(discoveryURL, "/") == strings.TrimRight(issuerURL, "/"):
		v.addf("%s %q must differ from issuerURL; leave it unset to use the issuer's own discovery document", path, discoveryURL)
	}
}

func (v *validator) checkOIDCPrefix(path, prefix string) {
	for _, reserved := range oidcReservedPrefixes {
		if strings.HasPrefix(prefix, reserved) {
			v.addf("%s %q is inside the reserved %q namespace", path, prefix, reserved)
			return
		}
	}
}

func (v *validator) checkK8sVersion(ver string) {
	switch {
	case ver == "":
		v.addf("cluster.k8sVersion is required")
	case !k8sVersionRE.MatchString(ver):
		v.addf("cluster.k8sVersion %q must be a plain semantic version like \"1.32.0\" (no leading \"v\")", ver)
	}
}

func (v *validator) checkCNI(name string) {
	if name == "" {
		v.addf("cluster.cniName is required (one of: %s)", strings.Join(SupportedCNIs(), ", "))
		return
	}
	for _, s := range SupportedCNIs() {
		if name == s {
			return
		}
	}
	v.addf("cluster.cniName %q is not supported (one of: %s)", name, strings.Join(SupportedCNIs(), ", "))
}

// checkCIDR validates a required CIDR field. When allowList is true the value
// may be a comma-separated list of CIDRs and each entry is validated.
func (v *validator) checkCIDR(path, value string, allowList bool) {
	if value == "" {
		v.addf("%s is required", path)
		return
	}
	parts := []string{value}
	if allowList {
		parts = strings.Split(value, ",")
	}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			v.addf("%s contains an empty CIDR entry", path)
			continue
		}
		if _, _, err := net.ParseCIDR(p); err != nil {
			v.addf("%s %q is not a valid CIDR", path, p)
		}
	}
}

// checkControlPlaneVIP requires a stable endpoint for nico sites and for
// multi-node control planes, and validates a configured IPv4 VIP against
// cluster CIDRs.
func (v *validator) checkControlPlaneVIP(inv *SiteSpec) {
	c := inv.Cluster
	if c.ControlPlaneVIP == "" {
		if inv.IsNico() {
			v.addf("%s is required with %s: %q: the site cluster's management plane is reached there", pathControlPlaneVIP,
				pathInfraProvider, ProviderNico)
		} else if inv.ControlPlaneCount() > 1 && !v.allowControlPlaneWithoutVIP {
			v.addf("%s is required when the cluster has more than one control-plane node (%s or %s) because kube-vip provides "+
				"a stable control-plane endpoint", pathControlPlaneVIP, pathNodes, pathNicoControlPlane+".count")
		}
		return
	}

	vip, err := netip.ParseAddr(c.ControlPlaneVIP)
	if err != nil {
		v.addf("%s %q is not a valid IP address", pathControlPlaneVIP, c.ControlPlaneVIP)
		return
	}
	// Match the controller's rejection of IPv4-mapped IPv6 addresses.
	if !vip.Is4() {
		v.addf("%s %q must be an IPv4 address", pathControlPlaneVIP, c.ControlPlaneVIP)
		return
	}
	// The VIP becomes kubeadm's controlPlaneEndpoint and a /32 advertised over
	// BGP, so the other control-plane nodes have to be able to dial it: not the
	// unspecified, loopback, multicast, link-local or broadcast forms.
	if !vip.IsGlobalUnicast() {
		v.addf("%s %q must be a routable unicast address", pathControlPlaneVIP, c.ControlPlaneVIP)
		return
	}

	for _, cidr := range strings.Split(c.PodCidr, ",") {
		v.checkVIPOutsideCIDR(vip, c.ControlPlaneVIP, pathPodCidr, cidr)
	}
	v.checkVIPOutsideCIDR(vip, c.ControlPlaneVIP, pathServiceCidr, c.ServiceCidr)
}

// checkCIDRsDisjoint refuses a pod CIDR that overlaps the service CIDR.
// kubeadm accepts the pair and the cluster then fails to route. Malformed
// CIDRs are reported by checkCIDR and skipped here.
func (v *validator) checkCIDRsDisjoint(c Cluster) {
	svc, ok := parsePrefix(c.ServiceCidr)
	if !ok {
		return
	}
	for _, p := range strings.Split(c.PodCidr, ",") {
		p = strings.TrimSpace(p)
		pod, ok := parsePrefix(p)
		if !ok {
			continue
		}
		if pod.Overlaps(svc) {
			v.addf("%s %q overlaps %s %q", pathPodCidr, p, pathServiceCidr, c.ServiceCidr)
		}
	}
}

// checkVIPOutsideCIDR reports when the VIP falls inside cidr. A malformed or
// empty cidr is left to checkCIDR; nothing is reported here for it.
// parsePrefix parses a CIDR the way checkCIDR accepts it, with net.ParseCIDR,
// and returns it as a netip.Prefix. net.ParseCIDR allows a prefix length with
// a leading zero; netip.ParsePrefix does not, so parsing with netip alone would
// skip a CIDR that checkCIDR had accepted.
func parsePrefix(cidr string) (netip.Prefix, bool) {
	_, ipnet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return netip.Prefix{}, false
	}
	prefix, err := netip.ParsePrefix(ipnet.String())
	if err != nil {
		return netip.Prefix{}, false
	}
	return prefix, true
}

func (v *validator) checkVIPOutsideCIDR(vip netip.Addr, vipText, path, cidr string) {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return
	}
	prefix, ok := parsePrefix(cidr)
	if !ok {
		return
	}
	if prefix.Contains(vip) {
		v.addf("%s %q is inside %s %q", pathControlPlaneVIP, vipText, path, cidr)
	}
}

func (v *validator) checkNodeAgentPort(port string) {
	if port == "" {
		return // optional; SetDefaults normally fills this in
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		v.addf("cluster.nodeAgentPort %q must be a number between 1 and 65535", port)
	}
}

// checkSpire validates the trust domain.
func (v *validator) checkSpire(s Spire) {
	if s.TrustDomain != "" {
		v.checkTrustDomain(s.TrustDomain)
	}
}

// checkSpireExternalAddress checks the address nodes reach SPIRE on when CAPNICo
// creates them, and refuses it otherwise so a value that nothing reads is not
// mistaken for one that does. Whether it is required depends on whether mTLS is
// on, which the site spec does not know; nvkectl bootstrap site checks that.
func (v *validator) checkSpireExternalAddress(inv *SiteSpec) {
	address := inv.Spire.ExternalAddress
	if !inv.IsNico() {
		if address != "" {
			v.addf("%s is only valid with %s: %q", pathSpireExternalAddress, pathInfraProvider, ProviderNico)
		}
		return
	}
	if address == "" {
		return
	}
	if ip, err := netip.ParseAddr(address); err == nil {
		// Loopback and the unspecified address mean nothing from another machine.
		// A zone means nothing either, and its text is not validated: it reaches
		// helm --set-string, where a comma would set another value.
		if !ip.IsGlobalUnicast() || ip.Zone() != "" {
			v.addf("%s %q must be a routable unicast address without a zone", pathSpireExternalAddress, address)
		}
		return
	}
	for _, label := range strings.Split(address, ".") {
		if len(label) == 0 || len(label) > 63 || !dnsLabelRE.MatchString(label) {
			v.addf("%s %q must be an IP address or a lowercase DNS name", pathSpireExternalAddress, address)
			return
		}
	}
	// localhost names the loopback interface (RFC 6761), as 127.0.0.1 does.
	if address == "localhost" || strings.HasSuffix(address, ".localhost") {
		v.addf("%s %q names the loopback interface, which means nothing from another machine", pathSpireExternalAddress, address)
	}
}

// CheckBootstrapMode applies the rules that depend on how the bootstrap will
// run, which Validate cannot know. With CAPNICo creating the nodes and mTLS on,
// the nodes read the SPIRE server's address from their userdata, so
// spire.externalAddress is required; with disableMTLS nothing reads it.
func (inv *SiteSpec) CheckBootstrapMode(disableMTLS bool) error {
	if inv.IsNico() && !disableMTLS && inv.Spire.ExternalAddress == "" {
		return fmt.Errorf("%s is required with %s: %q unless mTLS is disabled: nodes created by CAPNICo "+
			"need an address that reaches the bootstrap host's SPIRE server", pathSpireExternalAddress, pathInfraProvider, ProviderNico)
	}
	return nil
}

// checkTrustDomain validates the DNS-name form required for a SPIFFE trust
// domain. Schemes, paths, ports, empty labels, and uppercase characters are
// rejected by the same label rules used for the other DNS-shaped fields.
func (v *validator) checkTrustDomain(domain string) {
	if len(domain) > 253 {
		v.addf("%s %q must not exceed 253 characters", pathSpireTrustDomain, domain)
		return
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || !dnsLabelRE.MatchString(label) {
			v.addf("%s %q must be a lowercase DNS name", pathSpireTrustDomain, domain)
			return
		}
	}
}

// checkSpireCA reports a half-specified or unreadable CA from the
// environment, before anything has been touched.
func (v *validator) checkSpireCA(s Spire) {
	for _, pair := range removedSpireCAEnv {
		if os.Getenv(pair[0]) != "" {
			v.addf("%s is no longer accepted: unset it; set %s and %s: true", pair[0], pair[1], pathSpireUpstreamCA)
		}
	}
	certEnv, keyEnv := EnvSpireCACertFile, EnvSpireCAKeyFile
	switch {
	case s.CACertFile == "" && s.CAKeyFile == "":
		if s.UpstreamCA {
			v.addf("%s is true, so %s and %s are required: every run of this site needs the CA it was set up with",
				pathSpireUpstreamCA, certEnv, keyEnv)
		}
		return
	case !s.UpstreamCA:
		v.addf("%s or %s is set but %s is not true: set it in the site spec so every later run requires the CA",
			certEnv, keyEnv, pathSpireUpstreamCA)
		return
	case s.CACertFile == "":
		v.addf("%s is required when %s is set", certEnv, keyEnv)
		return
	case s.CAKeyFile == "":
		v.addf("%s is required when %s is set: SPIRE needs the key to sign with", keyEnv, certEnv)
		return
	}
	for _, f := range [][2]string{{certEnv, s.CACertFile}, {keyEnv, s.CAKeyFile}} {
		file, err := os.Open(f[1])
		if err != nil {
			v.addf("%s %q cannot be read: %v", f[0], f[1], err)
			continue
		}
		info, err := file.Stat()
		_ = file.Close()
		if err == nil && !info.Mode().IsRegular() {
			v.addf("%s %q is not a regular file", f[0], f[1])
		}
	}
}

// DefaultBootstrapClusterName is the cluster name `nvkectl bootstrap site`
// gives its bootstrap cluster unless NVKECTL_BOOTSTRAP_CLUSTER_NAME says
// otherwise. The default trust domain derives from it.
const DefaultBootstrapClusterName = "nvkectl-bootstrap"

// EffectiveTrustDomain returns the SPIFFE trust domain the bootstrap used:
// spire.trustDomain when set, else <bootstrapClusterName>.nke.local, the
// default `nvkectl bootstrap site` derives from its cluster name.
func (inv *SiteSpec) EffectiveTrustDomain(bootstrapClusterName string) string {
	if inv.Spire.TrustDomain != "" {
		return inv.Spire.TrustDomain
	}
	return bootstrapClusterName + ".nke.local"
}

// TrustDomain returns spire.trustDomain, or the trust domain a bootstrap with
// the default cluster name derives, and whether that default was used. The
// bootstrap and the controller stack install share it, so a site spec that
// sets nothing still joins both to one domain.
func (inv *SiteSpec) TrustDomain() (domain string, defaulted bool) {
	if inv.Spire.TrustDomain != "" {
		return inv.Spire.TrustDomain, false
	}
	return inv.EffectiveTrustDomain(DefaultBootstrapClusterName), true
}

// ControllerEndpoint returns the address the site controller is reached at:
// the control-plane VIP, or the only node's IP when the spec has one node
// and no VIP. With nico no node has an address until NICo allocates it, so
// the VIP is the only source; the bootstrap itself does not need one for a
// single node, which is why this is checked by the stack install and not
// when the spec loads.
func (inv *SiteSpec) ControllerEndpoint() (string, error) {
	if inv.Cluster.ControlPlaneVIP != "" {
		return inv.Cluster.ControlPlaneVIP, nil
	}
	if inv.IsNico() {
		return "", errors.New("the controller stack install needs cluster.controlPlaneVIP with infra.provider nico: the " +
			"nodes have no address until NICo allocates them, so there is nothing else to reach the controller at " +
			"(the bootstrap itself does not need it with one node)")
	}
	if len(inv.Cluster.Nodes) == 1 {
		return inv.Cluster.Nodes[0].IP, nil
	}
	return "", errors.New("the site spec has several nodes and no cluster.controlPlaneVIP")
}
