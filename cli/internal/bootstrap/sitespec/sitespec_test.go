// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
	return OIDCProvider{
		Name:           "corp",
		IssuerURL:      "https://login.example.com",
		Audiences:      []string{"nvke"},
		UsernameClaim:  "email",
		UsernamePrefix: "corp:",
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
				if !strings.Contains(err.Error(), pathControlPlaneVIP) || !strings.Contains(err.Error(), "more than one node") {
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
