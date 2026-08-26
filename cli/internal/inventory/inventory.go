// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package inventory loads and validates the declarative inventory file that
// drives "nkectl bootstrap site".
//
// The inventory file describes a set of pre-provisioned nodes and the vanilla
// Kubernetes cluster to build on them. It is the single source of truth for a
// bootstrap run: the command loads it, validates it, and fails fast with a
// clear error before dispatching any workflow or touching any node.
//
// Environment variables (see envBindings) override the matching field from the
// file, so existing tier-0 / DS9 scripts can drive the inventory via the
// environment. A set variable takes precedence over the file value.
//
// The schema deliberately mirrors the field names of the tier-0 bootstrap
// template so operators do not need to relearn them, but drops the
// NVIDIA-specific and deploy-time fields (SWE image tag, ArgoCD repos,
// embedded database credentials) so the file can be open-sourced. See
// docs/inventory-spec.md for the full field reference.
package inventory

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

const (
	// DefaultNodeAgentPort is the gRPC port nke-agent listens on when the
	// inventory does not set cluster.nodeAgentPort.
	DefaultNodeAgentPort = "50051"

	// Inventory field paths — the single source of truth for the dotted paths
	// shared by the environment bindings and the validator messages.
	pathClusterName          = "clusterName"
	pathInitNodeIP           = "cluster.initNode.ip"
	pathInitNodeName         = "cluster.initNode.name"
	pathK8sVersion           = "cluster.k8sVersion"
	pathCniName              = "cluster.cniName"
	pathCniVersion           = "cluster.cniVersion"
	pathPodCidr              = "cluster.podCidr"
	pathServiceCidr          = "cluster.serviceCidr"
	pathSystemReservedMemory = "cluster.kubeletConfig.systemReservedMemory"
	pathNodeAgentPort        = "cluster.nodeAgentPort"
	pathJoinNodes            = "cluster.joinNodes"
	pathBootstrapRepo        = "registry.bootstrapRepo"
	pathBootstrapChart       = "registry.bootstrapChart"
	pathAPIKey               = "registry.apiKey"
	pathIdentityAPIKey       = "registry.identityApiKey"
	pathSpireCACertFile      = "spire.caCertFile"
	pathSpireCAKeyFile       = "spire.caKeyFile"
)

var (
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
	return []string{"calico", "ovn-kubernetes"}
}

// Inventory is the declarative bootstrap input file.
type Inventory struct {
	// ClusterName is the name of the Kubernetes cluster to create.
	ClusterName string `yaml:"clusterName"`
	// Cluster holds the node inventory and Kubernetes settings.
	Cluster Cluster `yaml:"cluster"`
	// Registry holds the container-registry settings for the bootstrap engine
	// image. The field names are registry-agnostic so the file can be
	// open-sourced (tier-0 used NVIDIA-specific "ngcApiKey"/"sweImageTag").
	Registry Registry `yaml:"registry"`
	// Spire holds the identity-plane settings for the bootstrap.
	Spire Spire `yaml:"spire"`
}

// Spire describes the identity plane `nkectl bootstrap site` stands up.
type Spire struct {
	// CACertFile and CAKeyFile point at a CA the operator brings to the
	// bootstrap. SPIRE chains its own CA to it, so a node that was given the
	// certificate ahead of time can verify the server the first time it speaks
	// to it, instead of trusting whatever answers.
	//
	// They are paths rather than inline PEM for two reasons: a private key does
	// not belong in a file meant to be committed, and the same certificate has
	// to be delivered to each node by whatever provisions it — for DSX, the
	// ansible runbook over SSH — so it already exists as a file.
	//
	// Both or neither. Omitting them leaves SPIRE self-signed, which is fine
	// when nothing outside the cluster has to verify it, and means nodes cannot
	// be given a trust anchor in advance.
	CACertFile string `yaml:"caCertFile,omitempty"`
	CAKeyFile  string `yaml:"caKeyFile,omitempty"`
}

// Cluster describes the target Kubernetes cluster and its nodes.
type Cluster struct {
	// InitNode is the first control-plane node (kubeadm init runs here).
	InitNode Node `yaml:"initNode"`
	// JoinNodes are the remaining nodes. In M0 every node joins as a
	// control-plane node, matching tier-0's behavior.
	JoinNodes []Node `yaml:"joinNodes"`
	// K8sVersion is the Kubernetes version to install, e.g. "1.32.0".
	K8sVersion string `yaml:"k8sVersion"`
	// CniName selects the CNI plugin; see SupportedCNIs.
	CniName string `yaml:"cniName"`
	// CniVersion is an optional CNI plugin version (e.g. "3.31.4" for Calico).
	// Empty lets the node-agent use its compiled-in default.
	CniVersion string `yaml:"cniVersion"`
	// PodCidr is the pod network CIDR. Multiple CIDRs may be given as a
	// comma-separated string.
	PodCidr string `yaml:"podCidr"`
	// ServiceCidr is the service network CIDR.
	ServiceCidr string `yaml:"serviceCidr"`
	// NodeAgentPort is the gRPC port nke-agent listens on. Defaults to
	// DefaultNodeAgentPort when unset.
	NodeAgentPort string `yaml:"nodeAgentPort"`
	// KubeletConfig holds optional kubelet resource reservations.
	KubeletConfig KubeletConfig `yaml:"kubeletConfig"`
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
	// inventory rather than beside the credentials.
	TPMEKThumbprint string `yaml:"tpmEkThumbprint,omitempty"`
}

// TPMEKThumbprints returns every node's EK thumbprint, in inventory order,
// skipping nodes that have none. This is the allowlist the SPIRE server is
// installed with.
func (c Cluster) TPMEKThumbprints() []string {
	thumbprints := make([]string, 0, 1+len(c.JoinNodes))
	for _, node := range append([]Node{c.InitNode}, c.JoinNodes...) {
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

// Registry holds the settings for pulling the bootstrap engine image and chart.
// The version (image/chart tag) is baked into the nkectl build, so it is not
// part of the inventory.
type Registry struct {
	// BootstrapRepo is the OCI repository that holds the bootstrap engine image
	// and chart (e.g. "nvcr.io/org/cloud-workflow-engine").
	BootstrapRepo string `yaml:"bootstrapRepo"`
	// BootstrapChart is the bootstrap engine chart name within BootstrapRepo.
	BootstrapChart string `yaml:"bootstrapChart"`
	// APIKey is an optional registry credential. It is never defaulted: a
	// secret is only ever used when the operator supplies one, via this field,
	// a CLI flag, or an environment variable. See the SetDefaults note.
	APIKey string `yaml:"apiKey,omitempty"`

	// IdentityAPIKey is the credential for the registry account holding the
	// SPIRE chart and its server image.
	//
	// It is a second key rather than a second host because the two can be the
	// same host: the bootstrap image and the identity chart both live on
	// nvcr.io, in different accounts. A registry login covers one account, so
	// one credential cannot reach both, and keying these by hostname would not
	// tell them apart. Empty falls back to APIKey, which is right when one
	// account does hold everything.
	IdentityAPIKey string `yaml:"identityApiKey,omitempty"`
}

// IdentityCredential returns the credential for the SPIRE chart's registry,
// falling back to the bootstrap one when only a single account is in play.
func (r Registry) IdentityCredential() string {
	if r.IdentityAPIKey != "" {
		return r.IdentityAPIKey
	}
	return r.APIKey
}

// envBindings maps environment variable names to their inventory field paths.
// A set variable overrides the matching field from the file, so existing
// tier-0 / DS9 scripts can drive the inventory through the environment.
func envBindings() map[string]string {
	return map[string]string{
		"CLUSTER_NAME":                   pathClusterName,
		"INIT_NODE_IP":                   pathInitNodeIP,
		"INIT_NODE_NAME":                 pathInitNodeName,
		"CLUSTER_K8S_VERSION":            pathK8sVersion,
		"CLUSTER_CNI_NAME":               pathCniName,
		"CLUSTER_CNI_VERSION":            pathCniVersion,
		"CLUSTER_POD_CIDR":               pathPodCidr,
		"CLUSTER_SERVICE_CIDR":           pathServiceCidr,
		"CLUSTER_SYSTEM_RESERVED_MEMORY": pathSystemReservedMemory,
		"CLUSTER_NODE_AGENT_PORT":        pathNodeAgentPort,
		"REGISTRY_BOOTSTRAP_REPO":        pathBootstrapRepo,
		"REGISTRY_BOOTSTRAP_CHART":       pathBootstrapChart,
		"REGISTRY_API_KEY":               pathAPIKey,
		"IDENTITY_REGISTRY_API_KEY":      pathIdentityAPIKey,
		"SPIRE_CA_CERT_FILE":             pathSpireCACertFile,
		"SPIRE_CA_KEY_FILE":              pathSpireCAKeyFile,
	}
}

// newInventoryViper returns a Viper that reads a YAML inventory file with the
// environment-variable overrides in envBindings bound on top. Each binding is
// explicit (BindEnv); AutomaticEnv is intentionally not used, as it cannot map
// the dotted, camelCase field paths to valid environment variable names.
func newInventoryViper() *viper.Viper {
	v := viper.New()
	v.SetConfigType("yaml")
	for env, path := range envBindings() {
		_ = v.BindEnv(path, env)
	}
	return v
}

// Load reads the inventory file, overlays bound environment variables, applies
// defaults, and validates the result. It returns a fully validated Inventory
// or an aggregated error describing every problem found. A nil error means the
// inventory is safe to act on.
func Load(path string) (*Inventory, error) {
	v := newInventoryViper()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read inventory file %s: %w", path, err)
	}

	// JOIN_NODES="name:ip,name:ip" overlays join nodes when the file lists
	// none. A malformed entry fails loudly rather than being silently dropped.
	if joinEnv := os.Getenv("JOIN_NODES"); joinEnv != "" && !v.IsSet(pathJoinNodes) {
		nodes, err := parseJoinNodesEnv(joinEnv)
		if err != nil {
			return nil, err
		}
		v.Set(pathJoinNodes, nodes)
	}

	var inv Inventory
	if err := v.Unmarshal(&inv, useYAMLTags, rejectUnknownFields); err != nil {
		return nil, fmt.Errorf("inventory file %s: %w", path, err)
	}

	inv.SetDefaults()
	if err := inv.Validate(); err != nil {
		return nil, fmt.Errorf("inventory file %s: %w", path, err)
	}
	return &inv, nil
}

// useYAMLTags makes viper decode using the structs' existing yaml tags.
func useYAMLTags(dc *mapstructure.DecoderConfig) { dc.TagName = "yaml" }

// rejectUnknownFields makes decoding fail on an unknown field, so a typo in a
// field name surfaces immediately instead of being silently ignored.
func rejectUnknownFields(dc *mapstructure.DecoderConfig) { dc.ErrorUnused = true }

// parseJoinNodesEnv parses JOIN_NODES="name:ip,name:ip" into node maps,
// returning an error on any malformed entry.
func parseJoinNodesEnv(s string) ([]map[string]string, error) {
	var nodes []map[string]string
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, ip, ok := strings.Cut(entry, ":")
		name, ip = strings.TrimSpace(name), strings.TrimSpace(ip)
		if !ok || name == "" || ip == "" {
			return nil, fmt.Errorf("JOIN_NODES entry %q: expected \"name:ip\"", entry)
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
func (inv *Inventory) SetDefaults() {
	if inv.Cluster.NodeAgentPort == "" {
		inv.Cluster.NodeAgentPort = DefaultNodeAgentPort
	}
}

// Validate checks that every mandatory field is present and well-formed and
// returns a single error aggregating all problems. Validation runs before any
// workflow is dispatched, so a bad file never reaches a node.
func (inv *Inventory) Validate() error {
	v := &validator{}

	v.checkClusterName(inv.ClusterName)
	v.checkNodes(inv.Cluster)
	v.checkK8sVersion(inv.Cluster.K8sVersion)
	v.checkCNI(inv.Cluster.CniName)
	v.checkCIDR(pathPodCidr, inv.Cluster.PodCidr, true)
	v.checkCIDR(pathServiceCidr, inv.Cluster.ServiceCidr, false)
	v.checkNodeAgentPort(inv.Cluster.NodeAgentPort)
	v.checkRegistry(inv.Registry)
	v.checkSpire(inv.Spire)

	return v.err()
}

// validator accumulates field-level errors so the caller sees every problem
// in one pass rather than one-at-a-time.
type validator struct {
	errs []string
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

// checkNodes validates the init node and every join node, and rejects
// duplicate names or IPs across the whole set. Every entry is checked, so a
// malformed join node fails loudly instead of being silently discarded.
func (v *validator) checkNodes(c Cluster) {
	names := map[string]int{}
	ips := map[string]int{}

	v.checkNode("cluster.initNode", c.InitNode, names, ips)
	for i, n := range c.JoinNodes {
		v.checkNode(fmt.Sprintf("cluster.joinNodes[%d]", i), n, names, ips)
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

func (v *validator) checkNodeAgentPort(port string) {
	if port == "" {
		return // optional; SetDefaults normally fills this in
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		v.addf("cluster.nodeAgentPort %q must be a number between 1 and 65535", port)
	}
}

func (v *validator) checkRegistry(r Registry) {
	v.checkRegistryRef(pathBootstrapRepo, r.BootstrapRepo)
	v.checkRegistryRef(pathBootstrapChart, r.BootstrapChart)
}

// checkSpire validates the bootstrap CA, if one was given.
//
// Existence is checked here rather than left to the deploy because this is the
// last point at which nothing has been touched. A missing or unreadable CA
// otherwise surfaces several minutes in, as a helm failure that names a
// temporary file rather than the inventory field that was wrong.
func (v *validator) checkSpire(s Spire) {
	switch {
	case s.CACertFile == "" && s.CAKeyFile == "":
		return
	case s.CACertFile == "":
		v.addf("%s is required when %s is set", pathSpireCACertFile, pathSpireCAKeyFile)
		return
	case s.CAKeyFile == "":
		v.addf("%s is required when %s is set: SPIRE needs the key to sign with, "+
			"not just the certificate to chain to", pathSpireCAKeyFile, pathSpireCACertFile)
		return
	}

	for path, file := range map[string]string{
		pathSpireCACertFile: s.CACertFile,
		pathSpireCAKeyFile:  s.CAKeyFile,
	} {
		if _, err := os.Stat(file); err != nil {
			v.addf("%s %q cannot be read: %v", path, file, err)
		}
	}
}

// checkRegistryRef validates a required, whitespace-free registry reference.
// The API key is intentionally not checked here — it is optional and never
// defaulted.
func (v *validator) checkRegistryRef(path, value string) {
	switch {
	case value == "":
		v.addf("%s is required", path)
	case strings.ContainsAny(value, " \t"):
		v.addf("%s %q must not contain whitespace", path, value)
	}
}
