// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// validYAML is a minimal well-formed site spec used as the happy-path baseline.
const validYAML = `
clusterName: nvke-site
cluster:
  nodes:
    - name: node1
      ip: 10.0.0.1
    - name: node2
      ip: 10.0.0.2
    - name: node3
      ip: 10.0.0.3
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  cniVersion: "1.4.0"
  controlPlaneVIP: "10.0.0.100"
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
  kubeletConfig:
    systemReservedMemory: "16Gi"
`

func TestLoadValidSiteConfig(t *testing.T) {
	path := writeTemp(t, validYAML)

	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error for valid site spec: %v", err)
	}

	if inv.ClusterName != "nvke-site" {
		t.Errorf("ClusterName = %q, want %q", inv.ClusterName, "nvke-site")
	}
	if len(inv.Cluster.Nodes) != 3 {
		t.Fatalf("Nodes count = %d, want 3", len(inv.Cluster.Nodes))
	}
	if inv.Cluster.Nodes[0].IP != "10.0.0.1" {
		t.Errorf("Nodes[0].IP = %q, want %q", inv.Cluster.Nodes[0].IP, "10.0.0.1")
	}
	// NodeAgentPort is optional and should be defaulted.
	if inv.Cluster.NodeAgentPort != DefaultNodeAgentPort {
		t.Errorf("NodeAgentPort = %q, want default %q", inv.Cluster.NodeAgentPort, DefaultNodeAgentPort)
	}
}

func TestLoadSingleNode(t *testing.T) {
	const singleNode = `
clusterName: solo
cluster:
  nodes:
    - name: node1
      ip: 10.0.0.1
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
`
	inv, err := Load(writeTemp(t, singleNode))
	if err != nil {
		t.Fatalf("Load returned error for single-node siteSpec:       %v", err)
	}
	if len(inv.Cluster.Nodes) != 1 {
		t.Errorf("Nodes count = %d, want 1", len(inv.Cluster.Nodes))
	}
}

func TestSetDefaults(t *testing.T) {
	inv := &SiteSpec{}
	inv.SetDefaults()
	if inv.Cluster.NodeAgentPort != DefaultNodeAgentPort {
		t.Errorf("SetDefaults NodeAgentPort = %q, want %q", inv.Cluster.NodeAgentPort, DefaultNodeAgentPort)
	}
	if inv.Cluster.CniName != DefaultCNIName {
		t.Errorf("SetDefaults CniName = %q, want %q", inv.Cluster.CniName, DefaultCNIName)
	}
}

// The literal is spelled out because comparing DefaultNodeAgentPort with itself
// catches a missing value, never a wrong one. The other side is
// agent/cmd/agent/main.go's DefaultNodeServerListenPort, which cli cannot import.
func TestDefaultNodeAgentPortMatchesTheAgent(t *testing.T) {
	const agentDefaultListenPort = "15005"
	if DefaultNodeAgentPort != agentDefaultListenPort {
		t.Errorf("DefaultNodeAgentPort = %q, want %q, the port nvke-agent listens on "+
			"without --secure-server-port", DefaultNodeAgentPort, agentDefaultListenPort)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	// A typo in a field name must fail loudly, not be silently ignored.
	const typo = `
clusterName: nvke-site
cluster:
  nodes:
    - name: node1
      ip: 10.0.0.1
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
  cniVersionn: "1.4.0"
`
	if _, err := Load(writeTemp(t, typo)); err == nil {
		t.Fatal("Load accepted a site spec with an unknown field; want error")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("Load accepted a missing file; want error")
	}
}

func TestLoadEmptyFile(t *testing.T) {
	if _, err := Load(writeTemp(t, "")); err == nil {
		t.Fatal("Load accepted an empty file; want error")
	}
}

// TestValidateInvalidInputs drives the validator over a range of bad fields.
// Each case asserts that the specific offending field is named in the error so
// the operator gets an actionable message.
func TestValidateInvalidInputs(t *testing.T) {
	base := func() *SiteSpec {
		return &SiteSpec{
			ClusterName: "nvke-site",
			Cluster: Cluster{
				Nodes:         []Node{{Name: "node1", IP: "10.0.0.1"}},
				K8sVersion:    "1.32.0",
				CniName:       "ovn-kubernetes",
				PodCidr:       "10.100.0.0/16",
				ServiceCidr:   "10.200.0.0/16",
				NodeAgentPort: DefaultNodeAgentPort,
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*SiteSpec)
		wantSub string
	}{
		{"missing clusterName", func(i *SiteSpec) { i.ClusterName = "" }, pathClusterName},
		{"bad clusterName", func(i *SiteSpec) { i.ClusterName = "Nvke_Site" }, pathClusterName},
		{"missing nodes", func(i *SiteSpec) { i.Cluster.Nodes = nil }, pathNodes},
		{"even node count", func(i *SiteSpec) {
			i.Cluster.Nodes = append(i.Cluster.Nodes, Node{Name: "node2", IP: "10.0.0.2"})
		}, "cluster.nodes must contain an odd number"},
		{"missing node ip", func(i *SiteSpec) { i.Cluster.Nodes[0].IP = "" }, "cluster.nodes[0].ip"},
		{"bad node ip", func(i *SiteSpec) { i.Cluster.Nodes[0].IP = "999.0.0.1" }, "cluster.nodes[0].ip"},
		{"missing node name", func(i *SiteSpec) { i.Cluster.Nodes[0].Name = "" }, "cluster.nodes[0].name"},
		{"duplicate node name", func(i *SiteSpec) {
			i.Cluster.Nodes = append(i.Cluster.Nodes, Node{Name: "node1", IP: "10.0.0.2"})
		}, "name \"node1\" is used"},
		{"duplicate node ip", func(i *SiteSpec) {
			i.Cluster.Nodes = append(i.Cluster.Nodes, Node{Name: "node2", IP: "10.0.0.1"})
		}, "ip \"10.0.0.1\" is used"},
		{"missing k8sVersion", func(i *SiteSpec) { i.Cluster.K8sVersion = "" }, pathK8sVersion},
		{"bad k8sVersion", func(i *SiteSpec) { i.Cluster.K8sVersion = "v1.32" }, pathK8sVersion},
		{"missing cni", func(i *SiteSpec) { i.Cluster.CniName = "" }, pathCniName},
		{"legacy calico cni", func(i *SiteSpec) { i.Cluster.CniName = "calico" }, pathCniName},
		{"unsupported cni", func(i *SiteSpec) { i.Cluster.CniName = "flannel" }, pathCniName},
		{"missing podCidr", func(i *SiteSpec) { i.Cluster.PodCidr = "" }, pathPodCidr},
		{"bad podCidr", func(i *SiteSpec) { i.Cluster.PodCidr = "10.100.0.0" }, pathPodCidr},
		{"bad podCidr in list", func(i *SiteSpec) { i.Cluster.PodCidr = "10.100.0.0/16,nope" }, pathPodCidr},
		{"missing serviceCidr", func(i *SiteSpec) { i.Cluster.ServiceCidr = "" }, pathServiceCidr},
		{"podCidr overlaps serviceCidr", func(i *SiteSpec) { i.Cluster.ServiceCidr = "10.100.128.0/17" }, "overlaps cluster.serviceCidr"},
		{"podCidr overlaps a serviceCidr whose prefix length has a leading zero", func(i *SiteSpec) {
			i.Cluster.ServiceCidr = "10.100.128.0/017"
		}, "overlaps cluster.serviceCidr"},
		{"podCidr list entry overlaps serviceCidr", func(i *SiteSpec) {
			i.Cluster.PodCidr = "10.100.0.0/16,10.200.0.0/24"
		}, "\"10.200.0.0/24\" overlaps cluster.serviceCidr"},
		{"vip not an IP", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "nope" }, pathControlPlaneVIP},
		{"vip is IPv6", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "fd00::1" }, pathControlPlaneVIP},
		{"vip inside podCidr", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "10.100.0.5" }, pathControlPlaneVIP},
		{"vip inside serviceCidr", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "10.200.0.5" }, pathControlPlaneVIP},
		{"vip inside second podCidr", func(i *SiteSpec) {
			i.Cluster.PodCidr = "10.100.0.0/16,10.40.0.0/16"
			i.Cluster.ControlPlaneVIP = "10.40.0.5"
		}, pathControlPlaneVIP},
		// IPv4-mapped IPv6 addresses remain invalid even when they name a node IP.
		{"vip mapped IPv4 equals a node IP", func(i *SiteSpec) {
			i.Cluster.ControlPlaneVIP = "::ffff:10.0.0.1"
		}, pathControlPlaneVIP},
		// A mapped address with no colliding node is still not IPv4 and must be
		// rejected on that ground.
		{"vip mapped IPv4 not colliding", func(i *SiteSpec) {
			i.Cluster.ControlPlaneVIP = "::ffff:172.16.0.5"
		}, pathControlPlaneVIP},
		// The unabbreviated spelling of the same mapped address must be rejected too.
		{"vip mapped IPv4 unabbreviated", func(i *SiteSpec) {
			i.Cluster.ControlPlaneVIP = "0:0:0:0:0:ffff:10.0.0.1"
		}, pathControlPlaneVIP},
		// IPv4 forms that clear the overlap checks but cannot serve an endpoint.
		{"vip unspecified", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "0.0.0.0" }, pathControlPlaneVIP},
		{"vip loopback", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "127.0.0.1" }, pathControlPlaneVIP},
		{"vip broadcast", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "255.255.255.255" }, pathControlPlaneVIP},
		{"vip multicast", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "224.0.0.1" }, pathControlPlaneVIP},
		{"vip link-local", func(i *SiteSpec) { i.Cluster.ControlPlaneVIP = "169.254.1.1" }, pathControlPlaneVIP},
		{"bad nodeAgentPort", func(i *SiteSpec) { i.Cluster.NodeAgentPort = "70000" }, pathNodeAgentPort},
		{"oidc providers before 1.34", func(i *SiteSpec) {
			i.Cluster.K8sVersion = "1.33.4"
			i.Cluster.OIDCProviders = []OIDCProvider{validOIDCProvider()}
		}, pathOIDCProviders},
		{"oidc provider missing caBundle", func(i *SiteSpec) {
			i.Cluster.K8sVersion = "1.34.0"
			p := validOIDCProvider()
			p.CABundle = ""
			i.Cluster.OIDCProviders = []OIDCProvider{p}
		}, "cluster.oidcProviders[0].caBundle"},
		{"oidc provider http issuer", func(i *SiteSpec) {
			i.Cluster.K8sVersion = "1.34.0"
			p := validOIDCProvider()
			p.IssuerURL = "http://login.example.com"
			i.Cluster.OIDCProviders = []OIDCProvider{p}
		}, "cluster.oidcProviders[0].issuerURL"},
		{"oidc provider caBundle is not a certificate", func(i *SiteSpec) {
			i.Cluster.K8sVersion = "1.34.0"
			p := validOIDCProvider()
			p.CABundle = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
			i.Cluster.OIDCProviders = []OIDCProvider{p}
		}, "cluster.oidcProviders[0].caBundle"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := base()
			tc.mutate(inv)
			err := inv.Validate()
			if err == nil {
				t.Fatalf("Validate accepted invalid input; want error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func validOIDCProvider() OIDCProvider {
	usernamePrefix := "corp:"
	return OIDCProvider{
		Name:           "corp",
		IssuerURL:      "https://login.example.com",
		Audiences:      []string{"nvke"},
		UsernameClaim:  "email",
		UsernamePrefix: &usernamePrefix,
		CABundle: `-----BEGIN CERTIFICATE-----
MIIDCTCCAfGgAwIBAgIUfGXirJ1/KK2sS+M1ktcL2jKHJOcwDQYJKoZIhvcNAQEL
BQAwFDESMBAGA1UEAwwJb2lkYy10ZXN0MB4XDTI2MTAxMzE5NDA0NloXDTM2MTAx
MDE5NDA0NlowFDESMBAGA1UEAwwJb2lkYy10ZXN0MIIBIjANBgkqhkiG9w0BAQEF
AAOCAQ8AMIIBCgKCAQEAsqiBv8u0Am0ovLJ9XUkB2tfz2aLvw7V/Iur00XOzPKhp
Hx4MgMQPSgRL/GmrvqwvjFC7ts+1ngTdMPnFo7iWlVKaiRQ+jgOaWc326HsanFsx
duFJQ3VQYwAcw6vCaIACN7nMQuqmC3VtX4odVLMCrxsJXsHqhTriI0E8So4GzAv1
Od3uA9QX1hAtevNTe7bpXyDca+p9xLr7JsY94LvvzjuXesAYmK4QcnggPEMR0hPv
mn/M5YxzKxVjf2HwyddIyCdX10h9i7isTTxkPa/hJ/pbVQ8IB69URbUO/cySbl6A
OpTz1gfxJEqepGt7SaY6SKY852khpE+oY6WDLJJMsQIDAQABo1MwUTAdBgNVHQ4E
FgQUOQ4fTXchQ4yvOvssmBJ45QJuRgIwHwYDVR0jBBgwFoAUOQ4fTXchQ4yvOvss
mBJ45QJuRgIwDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAMXM0
aTT9nSY4lMAn07bSyMg6JF7MCz3MHqPCj9IrzkXk0eH5S9rrDOqwFJCa74pTzLsD
3gC8AmcaOmTRuBeEmu+CoO73n3fveR3moh1dupT61PRohfpdM47g+d5F1l0UdsWg
OXjHThCq0+PDR/w6/aYkW2nF1eyIYVS3FQlZUl+DvQDJsJqyNePtKCjMr+D3yywl
liNqG/bR3JXrwDCBLwbWzVRE1acA6frIzr8mR8skKm2DPmcsxUX5eO64XsNIYpxc
co5hU70BGb6ZUvwExLA5obJZPx3gFDtKVqJodwOFna2Df1Ai2Tuhztv+yVbz6M1s
f0pVwhs7o5oqIpjTuQ==
-----END CERTIFICATE-----
`,
	}
}

func TestValidate_OIDCProviders(t *testing.T) {
	absent := func() *SiteSpec {
		inv := &SiteSpec{
			ClusterName: "nvke-site",
			Cluster: Cluster{
				Nodes:         []Node{{Name: "node1", IP: "10.0.0.1"}},
				K8sVersion:    "1.32.0",
				CniName:       "ovn-kubernetes",
				PodCidr:       "10.100.0.0/16",
				ServiceCidr:   "10.200.0.0/16",
				NodeAgentPort: DefaultNodeAgentPort,
			},
		}
		return inv
	}
	if err := absent().Validate(); err != nil {
		t.Fatalf("Validate rejected a 1.32 site spec with no oidc providers: %v", err)
	}

	withProviders := absent()
	withProviders.Cluster.K8sVersion = "1.34.0"
	withProviders.Cluster.OIDCProviders = []OIDCProvider{validOIDCProvider()}
	if err := withProviders.Validate(); err != nil {
		t.Fatalf("Validate rejected oidc providers on 1.34.0: %v", err)
	}
}

// TestLoadOIDCUsernamePrefix verifies prefixes must be explicit and reserved namespaces remain protected.
func TestLoadOIDCUsernamePrefix(t *testing.T) {
	tests := []struct {
		name           string
		usernamePrefix string
		groupsPrefix   string
		omitPrefix     bool
		nullPrefix     bool
		wantErr        string
	}{
		{name: "explicit empty prefix"},
		{name: "omitted prefix", omitPrefix: true, wantErr: "usernamePrefix is required"},
		{name: "null prefix", nullPrefix: true, wantErr: "usernamePrefix is required"},
		{name: "nonempty prefix", usernamePrefix: "corp:"},
		{name: "reserved username prefix", usernamePrefix: "system:", wantErr: "reserved"},
		{name: "reserved group prefix", groupsPrefix: "system:", wantErr: "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := validOIDCProvider()
			provider.UsernamePrefix = &tt.usernamePrefix
			provider.GroupsPrefix = tt.groupsPrefix
			input := minimalValid()
			input.Cluster.K8sVersion = "1.34.0"
			input.Cluster.OIDCProviders = []OIDCProvider{provider}
			body, err := yaml.Marshal(input)
			require.NoError(t, err)
			text := string(body)
			if tt.omitPrefix {
				require.Contains(t, text, `usernamePrefix: ""`)
				text = strings.Replace(text, `usernamePrefix: ""`, "", 1)
			}
			if tt.nullPrefix {
				require.Contains(t, text, `usernamePrefix: ""`)
				text = strings.Replace(text, `usernamePrefix: ""`, "usernamePrefix: null", 1)
			}
			inv, err := Load(writeTemp(t, text))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, "cluster.oidcProviders[0]")
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, inv.Cluster.OIDCProviders, 1)
			require.NotNil(t, inv.Cluster.OIDCProviders[0].UsernamePrefix)
			assert.Equal(t, tt.usernamePrefix, *inv.Cluster.OIDCProviders[0].UsernamePrefix)
		})
	}
}

// TestLoadOIDCDiscoveryURL verifies accepted overrides survive loading and invalid URLs fail validation.
func TestLoadOIDCDiscoveryURL(t *testing.T) {
	const discoveryURL = "https://identity.example.com/.well-known/openid-configuration"
	tests := []struct {
		name         string
		discoveryURL string
		wantErr      string
	}{
		{name: "omitted"},
		{name: "separate discovery host", discoveryURL: discoveryURL},
		{name: "same issuer host with full discovery path", discoveryURL: "https://login.example.com/.well-known/openid-configuration"},
		{name: "minimum port", discoveryURL: "https://identity.example.com:1/discovery"},
		{name: "maximum port", discoveryURL: "https://identity.example.com:65535/discovery"},
		{name: "IPv6 with port", discoveryURL: "https://[2001:db8::1]:443/discovery"},
		{name: "zero port", discoveryURL: "https://identity.example.com:0/discovery", wantErr: "must be between 1 and 65535"},
		{name: "port too large", discoveryURL: "https://identity.example.com:65536/discovery", wantErr: "must be between 1 and 65535"},
		{name: "port overflows integer", discoveryURL: "https://identity.example.com:999999999999999999999999999999/discovery", wantErr: "must be between 1 and 65535"},
		{name: "http", discoveryURL: "http://identity.example.com/discovery", wantErr: "must be an absolute https URL"},
		{name: "relative URL", discoveryURL: "/.well-known/openid-configuration", wantErr: "must be an absolute https URL"},
		{name: "missing hostname", discoveryURL: "https://:443/discovery", wantErr: "must be an absolute https URL"},
		{name: "malformed URL", discoveryURL: "https://identity.example.com/%zz", wantErr: "is not a URL"},
		{name: "credentials", discoveryURL: "https://example-user:example-password@identity.example.com/discovery", wantErr: "must not contain a username or password"},
		{name: "query", discoveryURL: discoveryURL + "?tenant=a", wantErr: "must not contain a query or fragment"},
		{name: "fragment", discoveryURL: discoveryURL + "#fragment", wantErr: "must not contain a query or fragment"},
		{name: "issuer URL", discoveryURL: "https://login.example.com", wantErr: "must differ from issuerURL"},
		{name: "issuer URL with trailing slash", discoveryURL: "https://login.example.com/", wantErr: "must differ from issuerURL"},
		{name: "leading whitespace", discoveryURL: " " + discoveryURL, wantErr: "leading or trailing whitespace"},
		{name: "trailing whitespace", discoveryURL: discoveryURL + " ", wantErr: "leading or trailing whitespace"},
		{name: "whitespace only", discoveryURL: " ", wantErr: "leading or trailing whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := validOIDCProvider()
			provider.DiscoveryURL = tt.discoveryURL
			input := minimalValid()
			input.Cluster.K8sVersion = "1.34.0"
			input.Cluster.OIDCProviders = []OIDCProvider{provider}
			body, err := yaml.Marshal(input)
			require.NoError(t, err)
			inv, err := Load(writeTemp(t, string(body)))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, "cluster.oidcProviders[0].discoveryURL")
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, inv.Cluster.OIDCProviders, 1)
			assert.Equal(t, provider.IssuerURL, inv.Cluster.OIDCProviders[0].IssuerURL)
			assert.Equal(t, tt.discoveryURL, inv.Cluster.OIDCProviders[0].DiscoveryURL)
		})
	}
}

// TestValidateOIDCDuplicateDiscoveryURLs verifies only nonempty discovery overrides must be unique.
func TestValidateOIDCDuplicateDiscoveryURLs(t *testing.T) {
	const discoveryURL = "https://identity.example.com/.well-known/openid-configuration"
	tests := []struct {
		name      string
		firstURL  string
		secondURL string
		wantErr   bool
	}{
		{name: "duplicate override", firstURL: discoveryURL, secondURL: discoveryURL, wantErr: true},
		{name: "distinct overrides", firstURL: discoveryURL, secondURL: "https://other.example.com/discovery"},
		{name: "both omitted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := validOIDCProvider()
			provider.DiscoveryURL = tt.firstURL
			other := provider
			other.Name = "other"
			other.IssuerURL = "https://other.example.com"
			other.DiscoveryURL = tt.secondURL
			inv := minimalValid()
			inv.Cluster.K8sVersion = "1.34.0"
			inv.Cluster.OIDCProviders = []OIDCProvider{provider, other}
			err := inv.Validate()
			if tt.wantErr {
				require.ErrorContains(t, err, "cluster.oidcProviders[1].discoveryURL")
				assert.ErrorContains(t, err, "already used by cluster.oidcProviders[0]")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateControlPlaneVIP_Accepts(t *testing.T) {
	for name, vip := range map[string]string{
		"empty is valid":       "",
		"valid IPv4 VIP":       "10.0.0.100",
		"first node IP":        "10.0.0.1",
		"another node IP":      "10.0.0.2",
		"public address":       "198.51.100.10",
		"shared address space": "100.64.0.10",
	} {
		t.Run(name, func(t *testing.T) {
			inv := minimalValid()
			inv.Cluster.ControlPlaneVIP = vip
			if vip == "" {
				inv.Cluster.Nodes = inv.Cluster.Nodes[:1]
			}
			if err := inv.Validate(); err != nil {
				t.Errorf("Validate rejected VIP %q: %v", vip, err)
			}
		})
	}
}

func TestValidateControlPlaneVIP_NodeCountRequirement(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []Node
		vip     string
		wantErr bool
	}{
		{
			name:  "one node without a VIP",
			nodes: []Node{{Name: "node1", IP: "10.0.0.1"}},
		},
		{
			name: "several nodes without a VIP",
			nodes: []Node{
				{Name: "node1", IP: "10.0.0.1"},
				{Name: "node2", IP: "10.0.0.2"},
				{Name: "node3", IP: "10.0.0.3"},
			},
			wantErr: true,
		},
		{
			name: "several nodes with a VIP",
			nodes: []Node{
				{Name: "node1", IP: "10.0.0.1"},
				{Name: "node2", IP: "10.0.0.2"},
				{Name: "node3", IP: "10.0.0.3"},
			},
			vip: "10.0.0.100",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := minimalValid()
			inv.Cluster.Nodes = tc.nodes
			inv.Cluster.ControlPlaneVIP = tc.vip
			err := inv.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("Validate accepted a multi-node control plane without a VIP")
				}
				if !strings.Contains(err.Error(), pathControlPlaneVIP) || !strings.Contains(err.Error(), "more than one control-plane node") {
					t.Fatalf("error %q does not explain the %s node-count requirement", err, pathControlPlaneVIP)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate rejected valid input: %v", err)
			}
		})
	}
}

// TestLoadControlPlaneVIPFromEnv checks the environment override.
func TestLoadControlPlaneVIPFromEnv(t *testing.T) {
	t.Setenv("CLUSTER_CONTROL_PLANE_VIP", "10.0.0.100")

	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.Cluster.ControlPlaneVIP != "10.0.0.100" {
		t.Errorf("ControlPlaneVIP = %q, want env value %q", inv.Cluster.ControlPlaneVIP, "10.0.0.100")
	}
}

// TestValidateAggregatesErrors confirms that multiple problems are reported
// together rather than one at a time.
func TestValidateAggregatesErrors(t *testing.T) {
	inv := &SiteSpec{} // everything missing
	err := inv.Validate()
	if err == nil {
		t.Fatal("Validate accepted an empty site spec; want error")
	}
	for _, want := range []string{pathClusterName, pathNodes, pathK8sVersion} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error missing %q; got:\n%s", want, err.Error())
		}
	}
}

func TestValidValidatesClean(t *testing.T) {
	inv := &SiteSpec{
		ClusterName: "nvke-site",
		Cluster: Cluster{
			ControlPlaneVIP: "10.0.0.100",
			Nodes: []Node{
				{Name: "node1", IP: "10.0.0.1"},
				{Name: "node2", IP: "10.0.0.2"},
				{Name: "node3", IP: "10.0.0.3"},
			},
			K8sVersion:    "1.32.0",
			CniName:       "ovn-kubernetes",
			PodCidr:       "10.100.0.0/16,10.101.0.0/16",
			ServiceCidr:   "10.200.0.0/16",
			NodeAgentPort: "15005",
		},
	}
	if err := inv.Validate(); err != nil {
		t.Errorf("Validate rejected a valid site spec: %v", err)
	}
}

// noNodesYAML is a site spec with no nodes, used to exercise the environment
// fallback for the flat node list.
const noNodesYAML = `
clusterName: nvke-site
cluster:
  k8sVersion: "1.32.0"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
`

func TestLoadEnvOverridesFile(t *testing.T) {
	// Each bound environment variable must win over the file value.
	t.Setenv("CLUSTER_NAME", "from-env")
	t.Setenv("CLUSTER_K8S_VERSION", "1.33.1")

	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.ClusterName != "from-env" {
		t.Errorf("ClusterName = %q, want env override %q", inv.ClusterName, "from-env")
	}
	if inv.Cluster.K8sVersion != "1.33.1" {
		t.Errorf("K8sVersion = %q, want env override %q", inv.Cluster.K8sVersion, "1.33.1")
	}
	if inv.Cluster.Nodes[0].IP != "10.0.0.1" {
		t.Errorf("Nodes[0].IP = %q, want file value %q", inv.Cluster.Nodes[0].IP, "10.0.0.1")
	}
}

func TestLoadEnvFillsField(t *testing.T) {
	// validYAML sets no nodeAgentPort; the env var supplies it.
	t.Setenv("CLUSTER_NODE_AGENT_PORT", "15005")

	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.Cluster.NodeAgentPort != "15005" {
		t.Errorf("NodeAgentPort = %q, want env value %q", inv.Cluster.NodeAgentPort, "15005")
	}
}

func TestLoadTrustDomainFromEnvironment(t *testing.T) {
	t.Setenv("NVKECTL_SPIFFE_TRUST_DOMAIN", "site.example.com")

	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.Spire.TrustDomain != "site.example.com" {
		t.Errorf("Spire.TrustDomain = %q, want environment value %q", inv.Spire.TrustDomain, "site.example.com")
	}
}

func TestLoadNodesFromEnv(t *testing.T) {
	t.Setenv("NVKECTL_CLUSTER_NODES", "node1:10.0.0.1,node2:10.0.0.2,node3:10.0.0.3")
	t.Setenv("CLUSTER_CONTROL_PLANE_VIP", "10.0.0.100")

	inv, err := Load(writeTemp(t, noNodesYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(inv.Cluster.Nodes) != 3 {
		t.Fatalf("Nodes count = %d, want 3", len(inv.Cluster.Nodes))
	}
	if inv.Cluster.Nodes[0].Name != "node1" || inv.Cluster.Nodes[0].IP != "10.0.0.1" {
		t.Errorf("Nodes[0] = %+v, want {node1 10.0.0.1}", inv.Cluster.Nodes[0])
	}
}

func TestLoadNodesEnvMalformedFails(t *testing.T) {
	// A malformed node entry must fail loudly, not be silently discarded.
	t.Setenv("NVKECTL_CLUSTER_NODES", "node2:10.0.0.2,bogus")

	if _, err := Load(writeTemp(t, noNodesYAML)); err == nil {
		t.Fatal("Load accepted a malformed NVKECTL_CLUSTER_NODES entry; want error")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp siteSpec:       %v", err)
	}
	return path
}

// TestTPMEKThumbprints covers how the SPIRE allowlist is read off the
// siteSpec:       in node order, and skipping nodes that declare none.
func TestTPMEKThumbprints(t *testing.T) {
	const (
		initThumb = "1111111111111111111111111111111111111111111111111111111111111111"
		joinThumb = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	cluster := Cluster{
		Nodes: []Node{
			{Name: "node1", IP: "10.0.0.1", TPMEKThumbprint: initThumb},
			{Name: "node2", IP: "10.0.0.2"},
			{Name: "node3", IP: "10.0.0.3", TPMEKThumbprint: joinThumb},
		},
	}

	got := cluster.TPMEKThumbprints()
	if len(got) != 2 || got[0] != initThumb || got[1] != joinThumb {
		t.Errorf("TPMEKThumbprints() = %v, want node1's then node3's", got)
	}
	if len((Cluster{}).TPMEKThumbprints()) != 0 {
		t.Error("a cluster with no thumbprints should yield none, so no TPM attestor is configured")
	}
}

// TestValidate_TPMEKThumbprint rejects a malformed thumbprint at load time. It
// would otherwise be seeded into the allowlist, match nothing, and surface as
// that node silently failing attestation.
func TestValidate_TPMEKThumbprint(t *testing.T) {
	for name, thumbprint := range map[string]string{
		"too short":   "abc123",
		"uppercase":   "AAAA111111111111111111111111111111111111111111111111111111111111",
		"non-hex":     "zzzz111111111111111111111111111111111111111111111111111111111111",
		"with colons": "aa:bb:11111111111111111111111111111111111111111111111111111111",
	} {
		t.Run(name, func(t *testing.T) {
			v := &validator{}
			v.checkNode("cluster.nodes[0]", Node{
				Name: "node1", IP: "10.0.0.1", TPMEKThumbprint: thumbprint,
			}, map[string]int{}, map[string]int{})
			if v.err() == nil {
				t.Errorf("thumbprint %q should be rejected", thumbprint)
			}
		})
	}
}

// spireCAFiles writes a readable certificate and key and returns their paths,
// alongside a path that does not exist.
func spireCAFiles(t *testing.T) (cert, key, missing string) {
	t.Helper()
	dir := t.TempDir()
	cert = filepath.Join(dir, "ca.crt")
	key = filepath.Join(dir, "ca.key")
	for _, f := range []string{cert, key} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cert, key, filepath.Join(dir, "nope")
}

// minimalValid returns a site spec that passes Validate, so a test can vary
// one field without restating the whole structure.
func minimalValid() *SiteSpec {
	return &SiteSpec{
		ClusterName: "nvke-site",
		Cluster: Cluster{
			ControlPlaneVIP: "10.0.0.100",
			Nodes: []Node{
				{Name: "node1", IP: "10.0.0.1"},
				{Name: "node2", IP: "10.0.0.2"},
				{Name: "node3", IP: "10.0.0.3"},
			},
			K8sVersion:    "1.32.0",
			CniName:       "ovn-kubernetes",
			PodCidr:       "10.100.0.0/16",
			ServiceCidr:   "10.200.0.0/16",
			NodeAgentPort: "15005",
		},
	}
}

func TestValidate_SpireTrustDomain(t *testing.T) {
	for name, tc := range map[string]struct {
		domain  string
		wantErr bool
	}{
		"unset uses bootstrap default": {"", false},
		"dns name":                     {"site.example.com", false},
		"single label":                 {"site", false},
		"scheme":                       {"spiffe://site.example.com", true},
		"port":                         {"site.example.com:8081", true},
		"uppercase":                    {"Site.example.com", true},
		"empty label":                  {"site..example.com", true},
	} {
		t.Run(name, func(t *testing.T) {
			v := &validator{}
			v.checkSpire(Spire{TrustDomain: tc.domain})
			if gotErr := v.err() != nil; gotErr != tc.wantErr {
				t.Errorf("checkSpire(%q) error = %v, want error: %v", tc.domain, v.err(), tc.wantErr)
			}
		})
	}
}

// TestValidateReferences covers the CA from the environment: both or
// neither, and both readable.
func TestValidateReferences(t *testing.T) {
	cert, key, missing := spireCAFiles(t)
	for name, tc := range map[string]struct {
		spire     Spire
		wantErr   bool
		wantField string
	}{
		"no CA":               {Spire{}, false, ""},
		"both readable":       {Spire{UpstreamCA: true, CACertFile: cert, CAKeyFile: key}, false, ""},
		"cert without key":    {Spire{UpstreamCA: true, CACertFile: cert}, true, EnvSpireCAKeyFile},
		"key without cert":    {Spire{UpstreamCA: true, CAKeyFile: key}, true, EnvSpireCACertFile},
		"cert unreadable":     {Spire{UpstreamCA: true, CACertFile: missing, CAKeyFile: key}, true, EnvSpireCACertFile},
		"key unreadable":      {Spire{UpstreamCA: true, CACertFile: cert, CAKeyFile: missing}, true, EnvSpireCAKeyFile},
		"upstreamCA, no CA":   {Spire{UpstreamCA: true}, true, EnvSpireCACertFile},
		"CA, no upstreamCA":   {Spire{CACertFile: cert, CAKeyFile: key}, true, pathSpireUpstreamCA},
		"key, no upstreamCA":  {Spire{CAKeyFile: key}, true, pathSpireUpstreamCA},
		"cert is a directory": {Spire{UpstreamCA: true, CACertFile: filepath.Dir(cert), CAKeyFile: key}, true, EnvSpireCACertFile + " " + strconv.Quote(filepath.Dir(cert)) + " is not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			inv := minimalValid()
			inv.Spire = tc.spire
			err := inv.ValidateReferences()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("ValidateReferences(%+v) error = %v, want error: %v", tc.spire, err, tc.wantErr)
			}
			if tc.wantField != "" && !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("ValidateReferences() error = %q, want it to name %q", err, tc.wantField)
			}
		})
	}
}

// TestLoad_ReadsCAFromEnv: Load takes the CA paths from the environment, checks
// them, and reports them alongside data problems.
func TestLoad_ReadsCAFromEnv(t *testing.T) {
	cert, key, missing := spireCAFiles(t)
	t.Setenv(EnvSpireCACertFile, cert)
	t.Setenv(EnvSpireCAKeyFile, key)
	withCA := validYAML + "\nspire:\n  upstreamCA: true\n"
	inv, err := Load(writeTemp(t, withCA))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.Spire.CACertFile != cert || inv.Spire.CAKeyFile != key {
		t.Errorf("CA not read from the environment: %+v", inv.Spire)
	}

	t.Setenv(EnvSpireCACertFile, missing)
	t.Setenv(EnvSpireCAKeyFile, missing)
	body := strings.Replace(withCA, `podCidr: "10.100.0.0/16"`, `podCidr: "NOT-A-CIDR"`, 1)
	_, err = Load(writeTemp(t, body))
	for _, want := range []string{pathPodCidr, EnvSpireCACertFile} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %v, want it to name %q", err, want)
		}
	}
}

// TestLoadDescription_SkipsCA: LoadDescription needs neither CA variable and
// opens no file, and still reports that the site uses a CA.
func TestLoadDescription_SkipsCA(t *testing.T) {
	t.Setenv(EnvSpireCACertFile, "")
	t.Setenv(EnvSpireCAKeyFile, "")
	withCA := validYAML + "\nspire:\n  upstreamCA: true\n"
	path := writeTemp(t, withCA)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), EnvSpireCACertFile) {
		t.Errorf("Load without the CA variables: %v", err)
	}
	inv, err := LoadDescription(path)
	if err != nil {
		t.Fatalf("LoadDescription: %v", err)
	}
	if !inv.Spire.UpstreamCA || inv.Spire.CACertFile != "" || inv.Spire.CAKeyFile != "" {
		t.Errorf("spire: %+v", inv.Spire)
	}
	_, _, missing := spireCAFiles(t)
	t.Setenv(EnvSpireCACertFile, missing)
	t.Setenv(EnvSpireCAKeyFile, missing)
	if _, err := LoadDescription(path); err != nil {
		t.Errorf("LoadDescription opened the CA files: %v", err)
	}
}

// TestLoad_RefusesRemovedCAEnv: a CA variable main read is refused, alone or
// beside its replacement, rather than leaving SPIRE self-signed.
func TestLoad_RefusesRemovedCAEnv(t *testing.T) {
	cert, key, _ := spireCAFiles(t)
	withCA := validYAML + "\nspire:\n  upstreamCA: true\n"
	for _, pair := range removedSpireCAEnv {
		for name, body := range map[string]string{"alone": validYAML, "beside the new names": withCA} {
			t.Run(pair[0]+" "+name, func(t *testing.T) {
				t.Setenv(pair[0], cert)
				if name != "alone" {
					t.Setenv(EnvSpireCACertFile, cert)
					t.Setenv(EnvSpireCAKeyFile, key)
				}
				_, err := Load(writeTemp(t, body))
				if err == nil || !strings.Contains(err.Error(), pair[0]+" is no longer accepted: unset it; set "+pair[1]) {
					t.Errorf("Load() error = %v, want %s refused", err, pair[0])
				}
			})
		}
	}
}

// TestLoad_RejectsCAInTheFile: a CA in the file is rejected, naming the variables.
func TestLoad_RejectsCAInTheFile(t *testing.T) {
	_, err := Load(writeTemp(t, validYAML+"\nspire:\n  caCertFile: /x\n  caKeyFile: /y\n"))
	for _, want := range []string{EnvSpireCACertFile, EnvSpireCAKeyFile, "no longer accepted"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %v, want it to name %q", err, want)
		}
	}
}

func TestLoadDefaultsEtcdEncryptionOn(t *testing.T) {
	// validYAML says nothing about encryption; the loaded spec must resolve
	// that to on, so every reader downstream sees a decided value.
	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.Cluster.EtcdEncryption == nil || !*inv.Cluster.EtcdEncryption {
		t.Errorf("EtcdEncryption = %v, want &true after Load", inv.Cluster.EtcdEncryption)
	}
	if !inv.Cluster.EtcdEncryptionEnabled() {
		t.Error("EtcdEncryptionEnabled() = false for an unset field, want true")
	}
}

func TestLoadEnvDisablesEtcdEncryption(t *testing.T) {
	t.Setenv("CLUSTER_ETCD_ENCRYPTION", "false")

	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.Cluster.EtcdEncryptionEnabled() {
		t.Error("CLUSTER_ETCD_ENCRYPTION=false did not disable encryption")
	}
}

func TestEtcdEncryptionEnabled(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name  string
		field *bool
		want  bool
	}{
		{"unset", nil, true},
		{"true", &on, true},
		{"false", &off, false},
	} {
		if got := (Cluster{EtcdEncryption: tc.field}).EtcdEncryptionEnabled(); got != tc.want {
			t.Errorf("%s: EtcdEncryptionEnabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLoadRejectsEmptyEtcdEncryption: the decoder would read "" as false,
// which for this field is a permanent, silent opt-out. It is refused instead.
func TestLoadRejectsEmptyEtcdEncryption(t *testing.T) {
	for _, value := range []string{`""`, `''`} {
		spec := strings.Replace(validYAML, "cluster:\n", "cluster:\n  etcdEncryption: "+value+"\n", 1)
		_, err := Load(writeTemp(t, spec))
		if err == nil || !strings.Contains(err.Error(), pathEtcdEncryption) || !strings.Contains(err.Error(), "not a boolean") {
			t.Errorf("etcdEncryption: %s: err = %v, want one naming %s as not a boolean", value, err, pathEtcdEncryption)
		}
	}
}

// TestControlPlaneSchedulable pins the rule: no worker nodes in the site spec
// means a schedulable control plane. Update it when the spec can declare them.
func TestControlPlaneSchedulable(t *testing.T) {
	inv, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !inv.Cluster.ControlPlaneSchedulable() {
		t.Error("ControlPlaneSchedulable() = false for a site spec with no worker nodes, want true")
	}
}

const nicoYAML = `
clusterName: nvke-site
infra:
  provider: nico
  nico:
    orgID: example-org
    remote:
      endpoint: https://nico.example
      siteID: 00000000-0000-0000-0000-000000000001
      vpcID: 00000000-0000-0000-0000-000000000002
    controlPlane:
      count: 3
      instanceTypePreference:
        - 00000000-0000-0000-0000-000000000003
      vpcPrefixIDs:
        - 00000000-0000-0000-0000-000000000004
      sshKeyGroupIDs:
        - 00000000-0000-0000-0000-000000000005
      release: example-os-profile
      configProfile: example-config-profile
    releases: releases
cluster:
  controlPlaneVIP: "10.0.0.100"
  k8sVersion: "1.36.3"
  cniName: ovn-kubernetes
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
spire:
  externalAddress: 192.0.2.10
`

// writeNicoTemp writes a site spec and the files it names beside it: a releases
// directory holding the example-os-profile ClusterRelease and its MachineImage.
// It also writes a NICo token file, points NVKECTL_NICO_CREDENTIALS at it and
// sets the node login, as an operator running the bootstrap would.
func writeNicoTemp(t *testing.T, content string) string {
	t.Helper()
	path := writeTemp(t, content)
	dir := filepath.Dir(path)
	writeNicoFiles(t, dir, "nico.token", "releases")
	t.Setenv(NicoCredentialsEnv, filepath.Join(dir, "nico.token"))
	t.Setenv(stackconfig.EnvNodeUser, "example-user")
	t.Setenv(stackconfig.EnvNodePassword, "example-password")
	return path
}

// writeNicoFiles writes the files a nico site spec names into dir.
func writeNicoFiles(t *testing.T, dir, token, releases string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, token), []byte("example-token\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", token, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, releases), 0o755); err != nil {
		t.Fatal(err)
	}
	const docs = "apiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: example-image\n" +
		"---\napiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-os-profile\n" +
		"spec:\n  machineImageRef:\n    name: example-image\n"
	if err := os.WriteFile(filepath.Join(dir, releases, "release.yaml"), []byte(docs), 0o600); err != nil {
		t.Fatal(err)
	}
}

// makeNico turns a valid provisioned spec into a valid nico one.
func makeNico(inv *SiteSpec) {
	inv.Infra.Provider = ProviderNico
	inv.Cluster.Nodes = nil
	inv.Infra.Nico = validNico()
	inv.Spire.ExternalAddress = "192.0.2.10"
}

func TestLoadInfrastructureFromEnv(t *testing.T) {
	t.Setenv("NVKECTL_INFRA_PROVIDER", "nico")
	// The file lists nodes; the variable asks for nico, so the spec is
	// contradictory and must fail rather than quietly pick one.
	_, err := Load(writeTemp(t, validYAML))
	if err == nil || !strings.Contains(err.Error(), "cluster.nodes must be empty") {
		t.Fatalf("Load error = %v, want one about cluster.nodes", err)
	}
}

func TestValidate_Infrastructure(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SiteSpec)
		wantErr string // empty means the spec must validate
	}{
		{
			name:   "provisioned list is valid",
			mutate: func(*SiteSpec) {},
		},
		{
			name:    "unknown provider",
			mutate:  func(inv *SiteSpec) { inv.Infra.Provider = "aws" },
			wantErr: `infra.provider "aws" is not supported`,
		},
		{
			name:    "nico block with the provisioned provider",
			mutate:  func(inv *SiteSpec) { inv.Infra.Nico = &NicoInfra{} },
			wantErr: "infra.nico is only valid with infra.provider",
		},
		{
			name:    "nico without releases",
			mutate:  func(inv *SiteSpec) { makeNico(inv); inv.Infra.Nico.Releases = "" },
			wantErr: "infra.nico.releases is required",
		},
		{
			name:    "nico without a remote",
			mutate:  func(inv *SiteSpec) { makeNico(inv); inv.Infra.Nico.Remote = nil },
			wantErr: "infra.nico.remote is required",
		},
		{
			name:    "remote endpoint is not a URL",
			mutate:  func(inv *SiteSpec) { makeNico(inv); inv.Infra.Nico.Remote.Endpoint = "nico.example" },
			wantErr: "infra.nico.remote.endpoint",
		},
		{
			name:    "remote missing its endpoint",
			mutate:  func(inv *SiteSpec) { makeNico(inv); inv.Infra.Nico.Remote.Endpoint = "" },
			wantErr: "infra.nico.remote.endpoint is required",
		},
		{
			name: "nico without a nico block",
			mutate: func(inv *SiteSpec) {
				inv.Infra.Provider = ProviderNico
				inv.Cluster.Nodes = nil
			},
			wantErr: "infra.nico is required",
		},
		{
			name: "nico with a node list",
			mutate: func(inv *SiteSpec) {
				inv.Infra.Provider = ProviderNico
				inv.Infra.Nico = validNico()
			},
			wantErr: "cluster.nodes must be empty",
		},
		{
			name:   "nico is valid",
			mutate: makeNico,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := minimalValid()
			tt.mutate(inv)
			err := inv.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_NicoInfra(t *testing.T) {
	id := openapi_types.UUID{9}
	tests := []struct {
		name    string
		mutate  func(*NicoInfra)
		wantErr string
	}{
		{"zero nodes", func(n *NicoInfra) { n.ControlPlane.Count = 0 }, "controlPlane.count must be at least 1"},
		{"even nodes", func(n *NicoInfra) { n.ControlPlane.Count = 2 }, "controlPlane.count must be an odd number"},
		{"missing org", func(n *NicoInfra) { n.OrgID = "" }, "infra.nico.orgID is required"},
		{"missing site", func(n *NicoInfra) { n.Remote.SiteID = openapi_types.UUID{} }, "remote.siteID is required"},
		{"missing vpc", func(n *NicoInfra) { n.Remote.VpcID = openapi_types.UUID{} }, "remote.vpcID is required"},
		{"no instance types", func(n *NicoInfra) { n.ControlPlane.InstanceTypePreference = nil }, "instanceTypePreference must list"},
		{
			"nil instance type",
			func(n *NicoInfra) { n.ControlPlane.InstanceTypePreference = []openapi_types.UUID{{}} },
			"instanceTypePreference[0]",
		},
		{"no ssh key groups", func(n *NicoInfra) { n.ControlPlane.SSHKeyGroupIDs = nil }, "sshKeyGroupIDs must list"},
		{"no networking", func(n *NicoInfra) { n.ControlPlane.VpcPrefixIDs = nil }, "one of vpcPrefixIDs"},
		{
			"both networkings",
			func(n *NicoInfra) { n.ControlPlane.SubnetIDs = []openapi_types.UUID{id} },
			"only one of vpcPrefixIDs and subnetIDs",
		},
		{"missing release", func(n *NicoInfra) { n.ControlPlane.Release = "" }, "release is required"},
		{"API name with a slash", func(n *NicoInfra) { n.Remote.APIName = "forge/v2" }, "remote.apiName \"forge/v2\" must be a lowercase name"},
		{"API name in capitals", func(n *NicoInfra) { n.Remote.APIName = "Forge" }, "remote.apiName \"Forge\" must be a lowercase name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := minimalValid()
			makeNico(inv)
			tt.mutate(inv.Infra.Nico)
			err := inv.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidate_NicoConfigProfileIsOptional: the bootstrap controller serves no
// config profiles, so a spec that names none is complete.
func TestValidate_NicoConfigProfileIsOptional(t *testing.T) {
	inv := minimalValid()
	makeNico(inv)
	inv.Infra.Nico.ControlPlane.ConfigProfile = ""
	if err := inv.Validate(); err != nil {
		t.Fatalf("Validate rejected a nico spec with no configProfile: %v", err)
	}
}

// uid returns the UUID 00000000-0000-0000-0000-00000000000n.
func uid(n byte) openapi_types.UUID { return openapi_types.UUID{15: n} }

// validNico is built with the API's own struct for the control-plane block, as
// the site spec's is.
func validNico() *NicoInfra {
	return &NicoInfra{
		OrgID: "example-org",
		Remote: &NicoRemote{
			Endpoint: "https://nico.example",
			SiteID:   uid(1),
			VpcID:    uid(2),
		},
		ControlPlane: NicoControlPlane{
			Count: 3,
			NicoControlPlaneProviderCreate: api.NicoControlPlaneProviderCreate{
				InstanceTypePreference: []openapi_types.UUID{uid(3)},
				VpcPrefixIDs:           []openapi_types.UUID{uid(4)},
				SSHKeyGroupIDs:         []openapi_types.UUID{uid(5)},
				Release:                "example-os-profile",
				ConfigProfile:          "example-config-profile",
			},
		},
		Releases: "releases",
	}
}

// TestLoad_NicoBlocksAreTheAPIsStructs: the file's infra.nico is decoded with the
// generated API struct for the control plane flattened into controlPlane, UUIDs
// and all, and a field it does not declare is refused like any other unknown
// field.
func TestLoad_NicoBlocksAreTheAPIsStructs(t *testing.T) {
	inv, err := Load(writeNicoTemp(t, nicoYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := validNico()
	// Compared as the JSON the API would carry: an empty list and an absent one
	// are the same request.
	gotJSON, _ := json.Marshal(inv.Infra.Nico)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("infra.nico = %s, want %s", gotJSON, wantJSON)
	}

	_, err = Load(writeNicoTemp(t, strings.Replace(nicoYAML, "      release: example-os-profile", "      relaese: typo", 1)))
	if err == nil || !strings.Contains(err.Error(), "relaese") {
		t.Errorf("Load of a misspelled field = %v, want an error naming it", err)
	}

	_, err = Load(writeNicoTemp(t, strings.Replace(nicoYAML, "siteID: 00000000-0000-0000-0000-000000000001", "siteID: not-a-uuid", 1)))
	if err == nil {
		t.Error("Load accepted a siteID that is not a UUID")
	}
}

// TestValidateBootstrapReferences_FilesMustExist: the files the bootstrap reads
// are checked by the bootstrap's own check, not at load, so a command that never
// runs the bootstrap does not need them.
func TestValidateBootstrapReferences_FilesMustExist(t *testing.T) {
	path := writeTemp(t, nicoYAML)
	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load must not need the bootstrap-only files: %v", err)
	}

	t.Setenv(NicoCredentialsEnv, "")
	t.Setenv(stackconfig.EnvNodeUser, "")
	t.Setenv(stackconfig.EnvNodePassword, "")
	err = inv.ValidateBootstrapReferences()
	for _, want := range []string{NicoCredentialsEnv + " must name", stackconfig.EnvNodeUser + " is not set", stackconfig.EnvNodePassword + " is not set", "infra.nico.releases"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateBootstrapReferences error = %v, want it to report %q", err, want)
		}
	}

	t.Setenv(NicoCredentialsEnv, filepath.Join(t.TempDir(), "absent"))
	if err := inv.ValidateBootstrapReferences(); err == nil || !strings.Contains(err.Error(), NicoCredentialsEnv+": ") {
		t.Errorf("ValidateBootstrapReferences with a missing credentials file = %v", err)
	}
}

// TestLoad_RejectsTheNicoTenantBlock: the node login is a credential, so it is
// not in the site spec; a spec that still sets it is told where it went.
func TestLoad_RejectsTheNicoTenantBlock(t *testing.T) {
	path := writeNicoTemp(t, strings.Replace(nicoYAML, "    releases: releases", "    tenant:\n      user: u\n    releases: releases", 1))
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "infra.nico.tenant is no longer accepted") || !strings.Contains(err.Error(), stackconfig.EnvNodeUser) {
		t.Errorf("Load = %v, want infra.nico.tenant refused, naming %s", err, stackconfig.EnvNodeUser)
	}
}

// TestValidateBootstrapReferences_HTTPEndpointNeedsOptIn: the bootstrap sends
// the NICo token to infra.nico.remote.endpoint, so an http one is refused on the
// host that holds the token unless the operator opts in. Load still accepts it:
// the same file is valid for commands that never send the token.
func TestValidateBootstrapReferences_HTTPEndpointNeedsOptIn(t *testing.T) {
	path := writeNicoTemp(t, strings.Replace(nicoYAML, "endpoint: https://nico.example", "endpoint: http://nico.example:18080", 1))
	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load must accept an http endpoint: %v", err)
	}
	t.Setenv(NicoCredentialsEnv, filepath.Join(filepath.Dir(path), "nico.token"))

	t.Setenv(NicoInsecureEndpointEnv, "")
	err = inv.ValidateBootstrapReferences()
	for _, want := range []string{"infra.nico.remote.endpoint", "http://nico.example:18080", "cleartext", NicoInsecureEndpointEnv + "=1"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateBootstrapReferences error = %v, want it to contain %q", err, want)
		}
	}

	t.Setenv(NicoInsecureEndpointEnv, "1")
	if err := inv.ValidateBootstrapReferences(); err != nil && strings.Contains(err.Error(), "cleartext") {
		t.Errorf("ValidateBootstrapReferences with the opt-in = %v, want no endpoint error", err)
	}

	// https needs no opt-in.
	httpsInv, err := Load(writeNicoTemp(t, nicoYAML))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(NicoInsecureEndpointEnv, "")
	if err := httpsInv.ValidateBootstrapReferences(); err != nil && strings.Contains(err.Error(), "cleartext") {
		t.Errorf("an https endpoint was refused as cleartext: %v", err)
	}
}

// TestReadNicoToken: the credentials file's format is told from its content. A
// bare token is the bearer token; the client-credentials form is recognised and
// refused until CAPNICo can take it; anything else is not a token.
func TestReadNicoToken(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		want    string
		wantErr string
	}{
		{"a bare token", "example-token", "example-token", ""},
		{"a token with a trailing newline", "example-token\n", "example-token", ""},
		{"a JWT", "aaa.bbb.ccc=", "aaa.bbb.ccc=", ""},
		{"empty", "  \n", "", "is empty"},
		{"two values", "one two\n", "", "one value with no whitespace"},
		{"client credentials as YAML", "tokenURL: https://idp.example/token\nclientID: id\nclientSecret: secret\n", "", "client-credentials form is not supported"},
		{"client credentials as JSON", `{"tokenURL": "https://idp.example/token", "clientID": "id", "clientSecret": "s"}`, "", "client-credentials form is not supported"},
		{"client credentials as KEY=VALUE", "clientID=id\nclientSecret=secret\n", "", "client-credentials form is not supported"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadNicoToken(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ReadNicoToken error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ReadNicoToken = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := ReadNicoToken(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("ReadNicoToken of a missing file succeeded")
	}
}

// TestNicoCredentials_OldNamesStillWork: NVKECTL_NICO_CREDENTIALS is read under
// its NKECTL_ name too, as every NVKECTL_ variable is for one release. The
// cleartext opt-in is not: turning the https check off answers only to its
// documented name.
func TestNicoCredentials_OldNamesStillWork(t *testing.T) {
	t.Setenv(NicoCredentialsEnv, "")
	t.Setenv("NKECTL_NICO_CREDENTIALS", "/srv/nico.token")
	if got := NicoCredentialsPath(); got != "/srv/nico.token" {
		t.Errorf("NicoCredentialsPath() = %q; want the NKECTL_ name read", got)
	}

	path := writeNicoTemp(t, strings.Replace(nicoYAML, "endpoint: https://nico.example", "endpoint: http://nico.example:18080", 1))
	inv, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(NicoCredentialsEnv, filepath.Join(filepath.Dir(path), "nico.token"))
	t.Setenv(NicoInsecureEndpointEnv, "")
	t.Setenv("NKECTL_NICO_ALLOW_INSECURE_ENDPOINT", "1")
	if err := inv.ValidateBootstrapReferences(); err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("ValidateBootstrapReferences = %v; want the http endpoint refused despite NKECTL_NICO_ALLOW_INSECURE_ENDPOINT", err)
	}
}

// TestNicoCredentials_ATokenInTheVariableIsNotPrinted: an operator who puts the
// token itself in NVKECTL_NICO_CREDENTIALS, not the path of a file holding it,
// gets an error that does not echo the token.
func TestNicoCredentials_ATokenInTheVariableIsNotPrinted(t *testing.T) {
	const token = "eyJhbGciOiJSUzI1NiJ9.sentinel-nico-token.c2lnbmF0dXJl"
	if _, err := ReadNicoToken(token); err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "not the token itself") {
		t.Errorf("ReadNicoToken(token) error = %v; want one that says what is wrong without the token", err)
	}

	inv, err := Load(writeNicoTemp(t, nicoYAML))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(NicoCredentialsEnv, token)
	if err := inv.ValidateBootstrapReferences(); err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("ValidateBootstrapReferences error = %v; want an error without the token", err)
	}
}

// TestValidateBootstrapReferences_NothingToCheckWithProvisioned: a provisioned
// spec names none of those files.
func TestValidateBootstrapReferences_NothingToCheckWithProvisioned(t *testing.T) {
	inv := minimalValid()
	if err := inv.ValidateBootstrapReferences(); err != nil {
		t.Fatalf("ValidateBootstrapReferences on a provisioned spec: %v", err)
	}
}

func TestLoadInfra(t *testing.T) {
	tests := []struct {
		name            string
		yaml            string
		wantProvider    Provider
		wantNico        bool
		wantCPNodeCount int
	}{
		{"unset defaults to provisioned and counts the listed nodes", validYAML, ProviderProvisioned, false, 3},
		{"nico counts controlPlane.count", nicoYAML, ProviderNico, true, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := Load(writeNicoTemp(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if inv.Infra.Provider != tt.wantProvider {
				t.Errorf("Infra.Provider = %q, want %q", inv.Infra.Provider, tt.wantProvider)
			}
			if inv.IsNico() != tt.wantNico {
				t.Errorf("IsNico() = %t, want %t", inv.IsNico(), tt.wantNico)
			}
			if got := inv.ControlPlaneCount(); got != tt.wantCPNodeCount {
				t.Errorf("ControlPlaneCount() = %d, want %d", got, tt.wantCPNodeCount)
			}
		})
	}
}

func TestValidate_SpireExternalAddress(t *testing.T) {
	tests := []struct {
		name    string
		nico    bool
		address string
		wantErr string // empty means the spec must validate
	}{
		{"nico with an IPv4 address", true, "192.0.2.10", ""},
		{"nico with an IPv6 address", true, "2001:db8::10", ""},
		{"nico with a DNS name", true, "bootstrap.example.com", ""},
		{"nico without an address (required only with mTLS, checked by bootstrap)", true, "", ""},
		{"nico with loopback", true, "127.0.0.1", "routable unicast"},
		{"nico with the unspecified address", true, "0.0.0.0", "routable unicast"},
		{"nico with localhost", true, "localhost", "names the loopback interface"},
		{"nico with a localhost subdomain", true, "spire.localhost", "names the loopback interface"},
		{"nico with a zoned IPv6 address", true, "2001:db8::1%eth0", "without a zone"},
		{"nico with a zone carrying a helm value", true, "2001:db8::1%x,spiffe.enabled=false", "without a zone"},
		{"nico with an upper-case name", true, "Bootstrap.Example.com", "IP address or a lowercase DNS name"},
		{"nico with a name carrying a port", true, "bootstrap.example.com:30081", "IP address or a lowercase DNS name"},
		{"provisioned without an address", false, "", ""},
		{"provisioned with an address nothing would read", false, "192.0.2.10", "only valid with infra.provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := minimalValid()
			if tt.nico {
				makeNico(inv)
			}
			inv.Spire.ExternalAddress = tt.address

			err := inv.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestNicoExampleLoads: cli/examples/sitespec-nico.yaml is a file an operator
// will copy, so it has to load as written once the files it names exist.
func TestNicoExampleLoads(t *testing.T) {
	example, err := os.ReadFile("../../../examples/sitespec-nico.yaml")
	if err != nil {
		t.Fatalf("read cli/examples/sitespec-nico.yaml: %v", err)
	}
	path := writeTemp(t, string(example))
	writeNicoFiles(t, filepath.Dir(path), "nico.token", "bootstrap-releases")
	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !inv.IsNico() || inv.ControlPlaneCount() != 3 || inv.Infra.Nico == nil {
		t.Errorf("example loaded as %+v", inv)
	}
}

// TestCheckBootstrapMode: the SPIRE address CAPNICo nodes read from userdata is
// required with mTLS and unused without it.
func TestCheckBootstrapMode(t *testing.T) {
	tests := []struct {
		name        string
		nico        bool
		address     string
		disableMTLS bool
		wantErr     bool
	}{
		{"nico, mTLS, no address", true, "", false, true},
		{"nico, mTLS, address", true, "192.0.2.10", false, false},
		{"nico, mTLS disabled, no address", true, "", true, false},
		{"provisioned, mTLS, no address", false, "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := minimalValid()
			if tt.nico {
				makeNico(inv)
			}
			inv.Spire.ExternalAddress = tt.address

			err := inv.CheckBootstrapMode(tt.disableMTLS)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "spire.externalAddress is required") {
					t.Fatalf("CheckBootstrapMode error = %v, want spire.externalAddress is required", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckBootstrapMode: %v", err)
			}
		})
	}
}

// TestValidate_NicoControlPlaneVIP: nico's controlPlane.count counts toward the
// multi-node VIP requirement, as the length of cluster.nodes does.
func TestValidate_NicoControlPlaneVIP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		count   int
		vip     string
		wantErr bool
	}{
		{"three nodes without a VIP", 3, "", true},
		{"three nodes with a VIP", 3, "10.0.0.100", false},
		{"one node without a VIP", 1, "", true},
		{"one node with a VIP", 1, "10.0.0.100", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := minimalValid()
			makeNico(inv)
			inv.Infra.Nico.ControlPlane.Count = tc.count
			inv.Cluster.ControlPlaneVIP = tc.vip
			err := inv.Validate()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), pathControlPlaneVIP+" is required") {
					t.Fatalf("Validate error = %v, want %s is required", err, pathControlPlaneVIP)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestLoad_OldNicoKeysAreRefused: nico mode was never released, so its first
// layout was replaced rather than aliased. A spec still in it fails to load,
// naming the key, instead of having it silently ignored.
func TestLoad_OldNicoKeysAreRefused(t *testing.T) {
	for _, tt := range []struct {
		name string
		yaml string
		want string
	}{
		{"infrastructure", strings.Replace(nicoYAML, "infra:\n  provider: nico\n", "infrastructure: nico\ninfra:\n  provider: nico\n", 1), "infrastructure"},
		{"bootstrapNico", nicoYAML + "bootstrapNico:\n  endpoint: https://nico.example\n", "bootstrapnico"},
		{"bootstrapTenant", nicoYAML + "bootstrapTenant:\n  user: u\n", "bootstraptenant"},
		{"bootstrapReleases", nicoYAML + "bootstrapReleases: releases\n", "bootstrapreleases"},
		{"bootstrapPullSecret", nicoYAML + "bootstrapPullSecret:\n  server: ghcr.io\n", "bootstrappullsecret"},
		{"cluster.nico", strings.Replace(nicoYAML, "cluster:\n", "cluster:\n  nico:\n    nodeCount: 3\n", 1), "nico"},
		{"a managed block", strings.Replace(nicoYAML, "    releases: releases\n", "    releases: releases\n    managed:\n      siteName: x\n", 1), "managed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeNicoTemp(t, tt.yaml))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.want) {
				t.Fatalf("Load error = %v, want %q refused as unknown", err, tt.want)
			}
		})
	}
}

func TestLoad_BootstrapReleases(t *testing.T) {
	for name, tt := range map[string]struct {
		docs    string
		wantErr string
	}{
		"complete": {docs: "apiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n---\napiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-os-profile\nspec:\n  machineImageRef:\n    name: img\n"},
		"no ClusterRelease for the release": {
			docs:    "apiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: other\nspec:\n  machineImageRef:\n    name: img\n---\napiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n",
			wantErr: `no ClusterRelease named "example-os-profile"`,
		},
		"no MachineImage for the release": {
			docs:    "apiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-os-profile\nspec:\n  machineImageRef:\n    name: img\n",
			wantErr: `not the MachineImage "img"`,
		},
		"a NodePoolRelease alone is not enough": {
			docs:    "apiVersion: internal.nvke.nvidia.com/v1\nkind: NodePoolRelease\nmetadata:\n  name: example-os-profile\n",
			wantErr: "no ClusterRelease",
		},
		"ClusterRelease without an apiVersion": {
			docs:    "kind: ClusterRelease\nmetadata:\n  name: example-os-profile\nspec:\n  machineImageRef:\n    name: img\n---\napiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n",
			wantErr: `holds a ClusterRelease with apiVersion "", want "internal.nvke.nvidia.com/v1"`,
		},
		"MachineImage with another group": {
			docs:    "apiVersion: example.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n",
			wantErr: `holds a MachineImage with apiVersion "example.com/v1"`,
		},
		"not YAML": {
			docs:    "apiVersion: internal.nvke.nvidia.com/v1\nkind: [unclosed\n",
			wantErr: "is not valid YAML",
		},
		"not a manifest": {
			docs:    "- kind: ClusterRelease\n",
			wantErr: "is not a Kubernetes manifest",
		},
		"ClusterRelease with a spec of the wrong shape": {
			docs:    "apiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-os-profile\nspec: nope\n",
			wantErr: "holds an invalid ClusterRelease",
		},
		"MachineImage with a spec of the wrong shape": {
			docs:    "apiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\nspec: nope\n",
			wantErr: "holds an invalid MachineImage",
		},
		"a request document beside the manifests": {
			docs: "apiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n---\napiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-os-profile\nspec:\n  machineImageRef:\n    name: img\n" +
				"---\nmetadata:\n  name: example-os-profile\nspec:\n  machineImageRef:\n    name: img\n",
			wantErr: "holds a document without apiVersion and kind",
		},
		"a document with a kind and no apiVersion": {
			docs: "apiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n---\napiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-os-profile\nspec:\n  machineImageRef:\n    name: img\n" +
				"---\nkind: NodePoolRelease\nmetadata:\n  name: example-os-profile\n",
			wantErr: "holds a document without apiVersion and kind",
		},
		"empty documents are skipped": {docs: "# releases\n---\napiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n---\n---\napiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-os-profile\nspec:\n  machineImageRef:\n    name: img\n---\n# trailing\n"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeNicoTemp(t, nicoYAML)
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "releases", "release.yaml"), []byte(tt.docs), 0o600); err != nil {
				t.Fatal(err)
			}
			inv, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			err = inv.ValidateBootstrapReferences()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateBootstrapReferences: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateBootstrapReferences error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}

	// kubectl apply -f <dir> reads only the exact extensions .json, .yaml and
	// .yml, so a ClusterRelease in release.YAML is never applied.
	t.Run("an upper-case extension is not counted", func(t *testing.T) {
		path := writeNicoTemp(t, nicoYAML)
		dir := filepath.Join(filepath.Dir(path), "releases")
		if err := os.Rename(filepath.Join(dir, "release.yaml"), filepath.Join(dir, "release.YAML")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "image.yaml"),
			[]byte("apiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		inv, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := inv.ValidateBootstrapReferences(); err == nil || !strings.Contains(err.Error(), `no ClusterRelease named "example-os-profile"`) {
			t.Fatalf("ValidateBootstrapReferences error = %v, want the ClusterRelease in release.YAML not counted", err)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		path := writeNicoTemp(t, nicoYAML)
		if err := os.RemoveAll(filepath.Join(filepath.Dir(path), "releases")); err != nil {
			t.Fatal(err)
		}
		inv, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := inv.ValidateBootstrapReferences(); err == nil || !strings.Contains(err.Error(), "infra.nico.releases") {
			t.Fatalf("ValidateBootstrapReferences error = %v, want infra.nico.releases reported", err)
		}
	})
}

// baseSpec is the generated three-node site spec: what an operator holds
// after `nvkectl generate sitespec`.
func baseSpec(t *testing.T) *SiteSpec {
	t.Helper()
	spec, err := Build("my-tier0-site", threeNodes(), []string{"cluster.controlPlaneVIP=10.0.0.5"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return spec
}

func TestEffectiveTrustDomainAndEndpoint(t *testing.T) {
	spec := baseSpec(t)
	spec.Spire.TrustDomain = ""
	if got := spec.EffectiveTrustDomain("nvkectl-bootstrap"); got != "nvkectl-bootstrap.nke.local" {
		t.Errorf("default trust domain: %s", got)
	}
	if domain, defaulted := spec.TrustDomain(); domain != "nvkectl-bootstrap.nke.local" || !defaulted {
		t.Errorf("TrustDomain default: %s %v", domain, defaulted)
	}
	spec.Spire.TrustDomain = "site.example.com"
	if got := spec.EffectiveTrustDomain("ignored"); got != "site.example.com" {
		t.Errorf("explicit trust domain: %s", got)
	}
	if domain, defaulted := spec.TrustDomain(); domain != "site.example.com" || defaulted {
		t.Errorf("TrustDomain explicit: %s %v", domain, defaulted)
	}
	spec.Cluster.ControlPlaneVIP = "10.0.0.5"
	if ep, err := spec.ControllerEndpoint(); err != nil || ep != "10.0.0.5" {
		t.Errorf("VIP endpoint: %q %v", ep, err)
	}
	spec.Cluster.ControlPlaneVIP = ""
	if _, err := spec.ControllerEndpoint(); err == nil {
		t.Error("several nodes without a VIP accepted")
	}
	spec.Cluster.Nodes = spec.Cluster.Nodes[:1]
	if ep, err := spec.ControllerEndpoint(); err != nil || ep != spec.Cluster.Nodes[0].IP {
		t.Errorf("single node endpoint: %q %v", ep, err)
	}
}

// A removed field is refused with what replaced it.
func TestLoad_RejectsRemovedFields(t *testing.T) {
	cases := map[string]struct{ add, want string }{
		"registry":   {"registry:\n  bootstrapRepo: x\n", "nerdctl login"},
		"caCertFile": {"spire:\n  caCertFile: ca.crt\n", EnvSpireCACertFile},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sitespec.yaml")
			if err := os.WriteFile(path, []byte(string(templateYAML)+"\n"+tc.add), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "no longer accepted") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a removed-field error naming %s, got %v", tc.want, err)
			}
		})
	}
}

// The template renders as a site spec the loader accepts, with nothing but
// the bootstrap's sections in it.
func TestTemplate_IsABootstrapSpec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	body := strings.Replace(string(templateYAML), "  # controlPlaneVIP: \"10.0.0.100\"", "  controlPlaneVIP: \"10.0.0.100\"", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Render(); err != nil {
		t.Errorf("Render of a loaded spec: %v", err)
	}
}

// TestSettings_CoversEveryVariableTheBootstrapReads: bootstrap site warns about
// an old NKECTL_ name only for the variables in Settings, so every NVKECTL_
// variable this package reads is there, except the cleartext opt-in, which has
// no old name.
func TestSettings_CoversEveryVariableTheBootstrapReads(t *testing.T) {
	listed := map[string]bool{}
	for _, name := range Settings {
		listed[name] = true
	}
	read := []string{NicoCredentialsEnv, stackconfig.EnvNodeUser, stackconfig.EnvNodePassword}
	for env := range envBindings() {
		if strings.HasPrefix(env, "NVKECTL_") {
			read = append(read, env)
		}
	}
	for _, name := range read {
		if !listed[name] {
			t.Errorf("%s is read under its old NKECTL_ name but is not in Settings, so no deprecation warning names it", name)
		}
	}
	if listed[NicoInsecureEndpointEnv] {
		t.Errorf("%s is in Settings, but it answers only to its own name", NicoInsecureEndpointEnv)
	}
}

// TestNicoRemote_EffectiveAPIName: the API name defaults to forge.
func TestNicoRemote_EffectiveAPIName(t *testing.T) {
	for _, tt := range []struct {
		remote *NicoRemote
		want   string
	}{
		{nil, DefaultNicoAPIName},
		{&NicoRemote{}, DefaultNicoAPIName},
		{&NicoRemote{APIName: "carbide"}, "carbide"},
	} {
		if got := tt.remote.EffectiveAPIName(); got != tt.want {
			t.Errorf("EffectiveAPIName(%+v) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}

// TestNodeLoginFromEnv: the bootstrap reads the login the stack install reads,
// by the same rules, so a value one accepts the other does not refuse.
func TestNodeLoginFromEnv(t *testing.T) {
	t.Setenv(stackconfig.EnvNodeUser, " nke\n")
	t.Setenv(stackconfig.EnvNodePassword, "pw")
	if user, password, err := NodeLoginFromEnv(); err != nil || user != "nke" || password != "pw" {
		t.Errorf("NodeLoginFromEnv = %q, %q, %v; want the trimmed login", user, password, err)
	}
	t.Setenv(stackconfig.EnvNodeUser, "  ")
	t.Setenv(stackconfig.EnvNodePassword, "")
	_, _, err := NodeLoginFromEnv()
	for _, want := range []string{stackconfig.EnvNodeUser + " is not set", stackconfig.EnvNodePassword + " is not set"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("unset login: error %v lacks %q", err, want)
		}
	}
	for _, tt := range []struct{ name, value string }{
		{stackconfig.EnvNodeUser, `a"b`},
		{stackconfig.EnvNodePassword, `p\w`},
		{stackconfig.EnvNodePassword, "p\x01w"},
	} {
		t.Run(tt.name+" "+tt.value, func(t *testing.T) {
			t.Setenv(stackconfig.EnvNicoToken, "token")
			t.Setenv(stackconfig.EnvDatabasePassword, "db")
			t.Setenv(stackconfig.EnvNodeUser, "nke")
			t.Setenv(stackconfig.EnvNodePassword, "pw")
			t.Setenv(tt.name, tt.value)
			_, _, err := NodeLoginFromEnv()
			if err == nil || !strings.Contains(err.Error(), tt.name+": the value holds a character") {
				t.Errorf("NodeLoginFromEnv error = %v; want %s refused as the stack install refuses it", err, tt.name)
			}
			if _, stackErr := stackconfig.CredentialsFromEnv(); stackErr == nil {
				t.Errorf("stackconfig.CredentialsFromEnv accepted what NodeLoginFromEnv refuses")
			}
		})
	}
}

// TestControllerEndpoint_NicoNeedsTheVIP: one nico node without a VIP fails
// validation, and the controller endpoint says so for nico instead of claiming
// the spec has several nodes.
func TestControllerEndpoint_NicoNeedsTheVIP(t *testing.T) {
	spec := baseSpec(t)
	makeNico(spec)
	spec.Infra.Nico.ControlPlane.Count = 1
	spec.Cluster.ControlPlaneVIP = ""
	if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "cluster.controlPlaneVIP is required") {
		t.Fatalf("Validate = %v, want cluster.controlPlaneVIP is required for one nico node", err)
	}
	if _, err := spec.ControllerEndpoint(); err == nil || !strings.Contains(err.Error(), "cluster.controlPlaneVIP") || strings.Contains(err.Error(), "several nodes") {
		t.Errorf("ControllerEndpoint error = %v, want one naming cluster.controlPlaneVIP for nico", err)
	}
	spec.Cluster.ControlPlaneVIP = "10.0.0.5"
	if ep, err := spec.ControllerEndpoint(); err != nil || ep != "10.0.0.5" {
		t.Errorf("endpoint with a VIP: %q %v", ep, err)
	}
}

func TestLoadControlPlaneVIPOverride(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allow     bool
		vip       string
		wantError string
	}{
		{name: "default refuses missing VIP", wantError: "controlPlaneVIP"},
		{name: "explicit false refuses missing VIP", wantError: "controlPlaneVIP"},
		{name: "override permits missing VIP", allow: true},
		{name: "override validates malformed VIP", allow: true, vip: "invalid", wantError: "not a valid IP"},
		{name: "override validates CIDR overlap", allow: true, vip: "10.100.0.10", wantError: "inside"},
		{name: "override validates loopback VIP", allow: true, vip: "127.0.0.1", wantError: "routable unicast"},
		{name: "override validates IPv6 VIP", allow: true, vip: "2001:db8::1", wantError: "IPv4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(validYAML, `controlPlaneVIP: "10.0.0.100"`, `controlPlaneVIP: "`+tc.vip+`"`, 1)
			path := filepath.Join(t.TempDir(), "sitespec.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			spec, err := Load(path, WithAllowControlPlaneWithoutVIP(tc.allow))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("Load error = %v, want %q", err, tc.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if spec.Cluster.ControlPlaneVIP != "" {
				t.Fatalf("override filled in a VIP: %q", spec.Cluster.ControlPlaneVIP)
			}
			if tc.allow && tc.vip == "" {
				if _, err := Load(path); err == nil {
					t.Fatal("an override load changed later default validation")
				}
			}
		})
	}
}

func TestLoadNicoControlPlaneVIPOverride(t *testing.T) {
	body := strings.Replace(nicoYAML, "  controlPlaneVIP: \"10.0.0.100\"\n", "", 1)
	path := writeNicoTemp(t, body)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "controlPlaneVIP") {
		t.Fatalf("default NICo validation = %v, want a VIP requirement", err)
	}
	// A nico site always needs its VIP, so the dev override does not lift it.
	if _, err := Load(path, WithAllowControlPlaneWithoutVIP(true)); err == nil || !strings.Contains(err.Error(), "controlPlaneVIP") {
		t.Fatalf("NICo validation with the override = %v, want a VIP requirement", err)
	}
}
