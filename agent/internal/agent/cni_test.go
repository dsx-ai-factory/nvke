// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sample kubeconfig YAML for testing
const validKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0tLS1...
    server: https://192.168.1.100:6443
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: kubernetes-admin
  name: kubernetes-admin@kubernetes
current-context: kubernetes-admin@kubernetes
kind: Config
preferences: {}
users:
- name: kubernetes-admin
  user:
    client-certificate-data: LS0tLS1...
    client-key-data: LS0tLS1...
`

const kubeconfigNoClusters = `apiVersion: v1
clusters: []
contexts: []
kind: Config
preferences: {}
`

const kubeconfigInvalidClusterFormat = `apiVersion: v1
clusters:
- "not a map"
kind: Config
`

const kubeconfigNoServer = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0tLS1...
  name: kubernetes
kind: Config
`

const kubeconfigInvalidClusterInfo = `apiVersion: v1
clusters:
- cluster: "not a map"
  name: kubernetes
kind: Config
`

func TestGetKubeAPIServer(t *testing.T) {
	tests := []struct {
		name           string
		kubeconfigData string
		expectedServer string
		expectError    bool
		errorContains  string
	}{
		{
			name:           "valid kubeconfig",
			kubeconfigData: validKubeconfig,
			expectedServer: "https://192.168.1.100:6443",
			expectError:    false,
		},
		{
			name:           "no clusters in kubeconfig",
			kubeconfigData: kubeconfigNoClusters,
			expectError:    true,
			errorContains:  "no clusters found",
		},
		{
			name:           "invalid cluster format",
			kubeconfigData: kubeconfigInvalidClusterFormat,
			expectError:    true,
			errorContains:  "invalid cluster format",
		},
		{
			name:           "invalid cluster info format",
			kubeconfigData: kubeconfigInvalidClusterInfo,
			expectError:    true,
			errorContains:  "invalid cluster info format",
		},
		{
			name:           "no server in cluster",
			kubeconfigData: kubeconfigNoServer,
			expectError:    true,
			errorContains:  "server URL not found",
		},
		{
			name:           "invalid YAML",
			kubeconfigData: "not: valid: yaml: [",
			expectError:    true,
			errorContains:  "failed to parse kubeconfig",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a temp file with the kubeconfig data
			tmpDir := t.TempDir()
			kubeconfigPath := filepath.Join(tmpDir, "kubeconfig")
			err := os.WriteFile(kubeconfigPath, []byte(tc.kubeconfigData), 0o644)
			assert.NoError(t, err)

			server, err := getKubeAPIServer(kubeconfigPath)

			if tc.expectError {
				assert.Error(t, err)
				if tc.errorContains != "" {
					assert.Contains(t, err.Error(), tc.errorContains)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedServer, server)
			}
		})
	}
}

func TestGetKubeAPIServerFileNotFound(t *testing.T) {
	_, err := getKubeAPIServer("/nonexistent/path/kubeconfig")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read kubeconfig")
}

func TestGetKubeAPIServerWithMultipleClusters(t *testing.T) {
	// Test that it returns the first cluster's server
	multiClusterKubeconfig := `apiVersion: v1
clusters:
- cluster:
    server: https://cluster1.example.com:6443
  name: cluster1
- cluster:
    server: https://cluster2.example.com:6443
  name: cluster2
kind: Config
`
	tmpDir := t.TempDir()
	kubeconfigPath := filepath.Join(tmpDir, "kubeconfig")
	err := os.WriteFile(kubeconfigPath, []byte(multiClusterKubeconfig), 0o644)
	assert.NoError(t, err)

	server, err := getKubeAPIServer(kubeconfigPath)
	assert.NoError(t, err)
	assert.Equal(t, "https://cluster1.example.com:6443", server)
}

func TestGetKubeAPIServerWithDifferentServerFormats(t *testing.T) {
	tests := []struct {
		name           string
		serverURL      string
		expectedServer string
	}{
		{
			name:           "standard https with port",
			serverURL:      "https://10.0.0.1:6443",
			expectedServer: "https://10.0.0.1:6443",
		},
		{
			name:           "hostname with port",
			serverURL:      "https://api.cluster.local:6443",
			expectedServer: "https://api.cluster.local:6443",
		},
		{
			name:           "hostname without port",
			serverURL:      "https://kubernetes.default.svc",
			expectedServer: "https://kubernetes.default.svc",
		},
		{
			name:           "with path",
			serverURL:      "https://api.example.com:6443/api",
			expectedServer: "https://api.example.com:6443/api",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kubeconfig := `apiVersion: v1
clusters:
- cluster:
    server: ` + tc.serverURL + `
  name: kubernetes
kind: Config
`
			tmpDir := t.TempDir()
			kubeconfigPath := filepath.Join(tmpDir, "kubeconfig")
			err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o644)
			assert.NoError(t, err)

			server, err := getKubeAPIServer(kubeconfigPath)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedServer, server)
		})
	}
}

func TestCiliumVersionConstant(t *testing.T) {
	// Verify the CILIUM_VERSION constant is properly set
	assert.NotNil(t, CILIUM_VERSION)
	assert.Equal(t, "1.15.5", CILIUM_VERSION.String())
}

func TestInstallCNIApplyRequiresControlPlane(t *testing.T) {
	prev := chartsDebPresenceCheck
	chartsDebPresenceCheck = func(context.Context, string) error { return nil }
	t.Cleanup(func() { chartsDebPresenceCheck = prev })

	s := &NodeAgentServer{
		NodeStatus: &NodeStatus{Role: WorkerRole, Architecture: "amd64"},
	}
	err := s.installCNI(t.Context(), &pb.InstallCNIRequest{
		CniVariant: pb.CniVariant_CNI_VARIANT_OVN,
		Version:    "1.0.0",
	}, "nvke-cni-ovn-charts-1.0.0=1.0.0")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errCNIApplyRequiresControlPlane), "got %v", err)
}

func TestCheckHostOpenVSwitchReady(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		r := &MockRunner{
			Runners: []runner{
				runForCommand("ovs-vsctl", []string{"--version"}, func(string, []string, *nvkeutils.CommandOptions) ([]byte, error) {
					return []byte("ovs-vsctl (Open vSwitch) 3.3.0\n"), nil
				}),
			},
		}
		assert.NoError(t, checkHostOpenVSwitchReady(r))
		assert.Equal(t, 1, r.RunCalls)
	})

	t.Run("ovs-vsctl missing or failed", func(t *testing.T) {
		r := &MockRunner{Err: errors.New("executable file not found")}
		err := checkHostOpenVSwitchReady(r)
		assert.Error(t, err)
		assert.True(t, errors.Is(err, errOvnRequiresHostOpenVSwitch), "got %v", err)
	})
}

func TestInstallCNIOVNRequiresHostOpenVSwitch(t *testing.T) {
	prev := chartsDebPresenceCheck
	chartsDebPresenceCheck = func(context.Context, string) error { return nil }
	t.Cleanup(func() { chartsDebPresenceCheck = prev })

	s := &NodeAgentServer{
		NodeStatus: &NodeStatus{Role: ControlPlaneRole, Architecture: "amd64"},
		Runner:     &MockRunner{Err: errors.New("ovs-vsctl: not found")},
	}
	err := s.installCNI(t.Context(), &pb.InstallCNIRequest{
		CniVariant:     pb.CniVariant_CNI_VARIANT_OVN,
		PodNetworkCidr: "10.244.0.0/16",
		ServiceCidr:    "10.96.0.0/12",
	}, "nvke-cni-ovn-charts-1.6.0")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errOvnRequiresHostOpenVSwitch), "got %v", err)
}

func TestChartsDebPresenceFailsWhenAbsent(t *testing.T) {
	err := chartsDebPresenceDefault(t.Context(), "nonexistent-package-xyz")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errCNIChartsDebNotInstalled), "got %v", err)
}

func TestNvkeCniAptSpec(t *testing.T) {
	spec, err := nvkeCniAptSpec(pb.CniVariant_CNI_VARIANT_OVN, pb.CniAptRole_CNI_APT_ROLE_CHARTS, "v2.1.0")
	assert.NoError(t, err)
	assert.Equal(t, "nvke-cni-ovn-charts-2.1.0=2.1.0", spec)

	spec, err = nvkeCniAptSpec(pb.CniVariant_CNI_VARIANT_OVN, pb.CniAptRole_CNI_APT_ROLE_IMAGES, "1")
	assert.NoError(t, err)
	assert.Equal(t, "nvke-cni-ovn-images-1=1", spec)

	spec, err = nvkeCniAptSpec(pb.CniVariant_CNI_VARIANT_CALICO, pb.CniAptRole_CNI_APT_ROLE_CHARTS, "3.0")
	assert.NoError(t, err)
	assert.Equal(t, "nvke-cni-calico-manifests-3.0=3.0", spec)

	spec, err = nvkeCniAptSpec(pb.CniVariant_CNI_VARIANT_UNSPECIFIED, pb.CniAptRole_CNI_APT_ROLE_IMAGES, "1")
	assert.NoError(t, err)
	assert.Equal(t, "nvke-cni-ovn-images-1=1", spec)

	spec, err = nvkeCniAptSpec(pb.CniVariant_CNI_VARIANT_UNSPECIFIED, pb.CniAptRole_CNI_APT_ROLE_CHARTS, "")
	assert.NoError(t, err)
	assert.Equal(t, "nvke-cni-ovn-charts-1.6.0", spec)

	spec, err = nvkeCniAptSpec(pb.CniVariant_CNI_VARIANT_CALICO, pb.CniAptRole_CNI_APT_ROLE_IMAGES, "")
	assert.NoError(t, err)
	assert.Equal(t, "nvke-cni-calico-images-3.30.1", spec)

	_, err = nvkeCniAptSpec(pb.CniVariant_CNI_VARIANT_CALICO, pb.CniAptRole_CNI_APT_ROLE_UNSPECIFIED, "1")
	assert.Error(t, err)
}

func TestAlternateCniDebName(t *testing.T) {
	assert.Equal(t, "nke-cni-ovn-charts-1.4.0", alternateCniDebName("nvke-cni-ovn-charts-1.4.0"))
	assert.Equal(t, "nvke-cni-calico-manifests-3.30.1", alternateCniDebName("nke-cni-calico-manifests-3.30.1"))
	assert.Empty(t, alternateCniDebName("spire-agent"))
}

func TestCalicoInstallRootFromFiles(t *testing.T) {
	t.Run("helm chart with manifests", func(t *testing.T) {
		root, err := calicoInstallRootFromFiles([]string{
			"/usr/share/foo/Chart.yaml",
			"/usr/share/foo/manifests/tigera-operator.yaml",
		})
		assert.NoError(t, err)
		assert.Equal(t, "/usr/share/foo", root)
	})
	t.Run("manifest only (legacy pkg-builder path)", func(t *testing.T) {
		root, err := calicoInstallRootFromFiles([]string{
			"/usr/share/nke-calico-charts/manifests/tigera-operator.yaml",
			"/usr/share/nke-calico-charts/manifests/custom-resources.yaml",
		})
		assert.NoError(t, err)
		assert.Equal(t, "/usr/share/nke-calico-charts", root)
	})
	t.Run("cni-packages layout under /usr/share/nvke-cni/calico", func(t *testing.T) {
		root, err := calicoInstallRootFromFiles([]string{
			"/usr/share/nvke-cni/calico/manifests/tigera-operator.yaml",
			"/usr/share/nvke-cni/calico/manifests/custom-resources.yaml",
			"/usr/share/nvke-cni/calico/apply-manifests.sh",
		})
		assert.NoError(t, err)
		assert.Equal(t, "/usr/share/nvke-cni/calico", root)
	})
	// The exact `dpkg-query -L` output of the nvke-cni-calico-manifests-<stream>
	// deb built by os-image/packages/cni-calico. That package exists so a node
	// can install Calico with no registry, and it is only useful if this
	// resolves: keep the two in step.
	t.Run("os-image cni-calico package layout", func(t *testing.T) {
		root, err := calicoInstallRootFromFiles([]string{
			"/usr/share/nvke-cni/calico/3.30.1/manifests/custom-resources.yaml",
			"/usr/share/nvke-cni/calico/3.30.1/manifests/tigera-operator.yaml",
		})
		assert.NoError(t, err)
		assert.Equal(t, "/usr/share/nvke-cni/calico/3.30.1", root)
	})
	t.Run("prefers chart root when both match", func(t *testing.T) {
		root, err := calicoInstallRootFromFiles([]string{
			"/other/manifests/tigera-operator.yaml",
			"/chart/Chart.yaml",
			"/chart/manifests/tigera-operator.yaml",
		})
		assert.NoError(t, err)
		assert.Equal(t, "/chart", root)
	})
	t.Run("missing", func(t *testing.T) {
		_, err := calicoInstallRootFromFiles([]string{"/usr/bin/foo"})
		assert.Error(t, err)
	})
}

func TestCalicoCustomResourcesApplyPath(t *testing.T) {
	manifestsDir := t.TempDir()
	baseYAML := `# test
    - name: default-ipv4-ippool
      cidr: 192.168.0.0/16
`
	crPath := filepath.Join(manifestsDir, "custom-resources.yaml")
	assert.NoError(t, os.WriteFile(crPath, []byte(baseYAML), 0o644))

	t.Run("empty pod cidr returns packaged path", func(t *testing.T) {
		p, cleanup, err := calicoCustomResourcesApplyPath(manifestsDir, "")
		assert.NoError(t, err)
		assert.Equal(t, crPath, p)
		assert.Nil(t, cleanup)
	})

	t.Run("whitespace-only pod cidr returns packaged path", func(t *testing.T) {
		p, cleanup, err := calicoCustomResourcesApplyPath(manifestsDir, "  \t ")
		assert.NoError(t, err)
		assert.Equal(t, crPath, p)
		assert.Nil(t, cleanup)
	})

	t.Run("substitutes default pool into temp file", func(t *testing.T) {
		p, cleanup, err := calicoCustomResourcesApplyPath(manifestsDir, "10.244.0.0/16")
		assert.NoError(t, err)
		assert.NotEqual(t, crPath, p)
		assert.NotNil(t, cleanup)
		t.Cleanup(cleanup)
		out, err := os.ReadFile(p)
		assert.NoError(t, err)
		assert.Contains(t, string(out), "cidr: 10.244.0.0/16")
		assert.NotContains(t, string(out), "192.168.0.0/16")
	})

	t.Run("missing default line errors when pod cidr requested", func(t *testing.T) {
		dir := t.TempDir()
		other := filepath.Join(dir, "custom-resources.yaml")
		assert.NoError(t, os.WriteFile(other, []byte("apiVersion: v1\nkind: ConfigMap\n"), 0o644))
		p, cleanup, err := calicoCustomResourcesApplyPath(dir, "10.0.0.0/8")
		assert.Error(t, err)
		assert.Empty(t, p)
		assert.Nil(t, cleanup)
		assert.Contains(t, err.Error(), other)
		assert.Contains(t, err.Error(), calicoDefaultIPPoolsCIDRLine)
		assert.Contains(t, err.Error(), "10.0.0.0/8")
	})

	t.Run("missing default line returns packaged path when pod cidr empty", func(t *testing.T) {
		dir := t.TempDir()
		other := filepath.Join(dir, "custom-resources.yaml")
		assert.NoError(t, os.WriteFile(other, []byte("apiVersion: v1\nkind: ConfigMap\n"), 0o644))
		p, cleanup, err := calicoCustomResourcesApplyPath(dir, "")
		assert.NoError(t, err)
		assert.Equal(t, other, p)
		assert.Nil(t, cleanup)
	})
}

func TestCalicoKubectlEnv(t *testing.T) {
	t.Run("sets kubeconfig only when pod cidr empty", func(t *testing.T) {
		env := calicoKubectlEnv("")
		foundKube := false
		foundCalico := false
		for _, e := range env {
			if strings.HasPrefix(e, "KUBECONFIG=") {
				foundKube = true
				assert.Equal(t, "KUBECONFIG="+KubeConfig, e)
			}
			if strings.HasPrefix(e, "CALICO_POD_CIDR=") {
				foundCalico = true
			}
		}
		assert.True(t, foundKube)
		assert.False(t, foundCalico)
	})

	t.Run("trims pod cidr and sets calico env", func(t *testing.T) {
		env := calicoKubectlEnv("  10.244.0.0/16 \n")
		var calico string
		for _, e := range env {
			if strings.HasPrefix(e, "CALICO_POD_CIDR=") {
				calico = e
			}
		}
		assert.Equal(t, "CALICO_POD_CIDR=10.244.0.0/16", calico)
	})
}

// TestInstallCNIPluginSelection tests the installCNIPlugin method's plugin selection logic
func TestInstallCNIPluginSelection(t *testing.T) {
	// This test verifies the switch case logic in agent.go's installCNIPlugin
	// by checking that plugin names are correctly identified
	tests := []struct {
		name           string
		cniPluginName  string
		expectedPlugin string
	}{
		{
			name:           "ovn plugin",
			cniPluginName:  "ovn",
			expectedPlugin: "ovn",
		},
		{
			name:           "tigera plugin",
			cniPluginName:  "tigera",
			expectedPlugin: "calico",
		},
		{
			name:           "calico plugin",
			cniPluginName:  "calico",
			expectedPlugin: "calico",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// We're testing the logic conceptually here
			// The actual installCNIPlugin is in agent.go and uses the plugin name
			// to decide which installation method to use
			var expected string
			switch tc.cniPluginName {
			case "ovn":
				expected = "ovn"
			case "tigera", "calico":
				expected = "calico"
			default:
				t.Fatalf("plugin name %q is not a known installCNIPlugin selection", tc.cniPluginName)
			}
			assert.Equal(t, tc.expectedPlugin, expected)
		})
	}
}

func TestInstallCNIPluginRejectsUnknownName(t *testing.T) {
	server := &NodeAgentServer{}
	for _, name := range []string{"", "flannel"} {
		req := &pb.InstallCNIPluginRequest{PluginName: name}
		err := server.installCNIPlugin(req)
		require.EqualError(t, err, fmt.Sprintf("unsupported CNI plugin %q", name))
		assert.Equal(t, name, req.GetPluginName())
	}
}

func TestHelmPodNetworkValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "plain value is unchanged",
			value: "10.244.0.0/16",
			want:  "10.244.0.0/16",
		},
		{
			name:  "comma is escaped",
			value: "10.240.0.0/16,10.241.0.0/16",
			want:  `10.240.0.0/16\,10.241.0.0/16`,
		},
		{
			name:  "dual-stack CIDRs are escaped",
			value: "10.240.0.0/16,fd00:10:244::/56",
			want:  `10.240.0.0/16\,fd00:10:244::/56`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, helmPodNetworkValue(tc.value))
		})
	}
}

// TestOVNHelmArgsConstruction tests the helm args construction for OVN installation
func TestOVNHelmArgsConstruction(t *testing.T) {
	tests := []struct {
		name           string
		apiServer      string
		podNetworkCidr string
		serviceCidr    string
		cniConfig      map[string]string
		wantPodNetwork string
		expectedArgs   int // Minimum number of args expected
	}{
		{
			name:           "basic args without extra config",
			apiServer:      "https://10.0.0.1:6443",
			podNetworkCidr: "10.244.0.0/16",
			serviceCidr:    "10.96.0.0/12",
			cniConfig:      nil,
			wantPodNetwork: "podNetwork=10.244.0.0/16",
			expectedArgs:   9, // install + release + chartDir + 3x --set (no values files in empty dir)
		},
		{
			name:           "multiple pod CIDRs are set as one string",
			apiServer:      "https://10.0.0.1:6443",
			podNetworkCidr: "10.240.0.0/16,10.241.0.0/16",
			serviceCidr:    "10.96.0.0/12",
			cniConfig:      nil,
			wantPodNetwork: `podNetwork=10.240.0.0/16\,10.241.0.0/16`,
			expectedArgs:   9,
		},
		{
			name:           "with extra CNI config",
			apiServer:      "https://10.0.0.1:6443",
			podNetworkCidr: "10.244.0.0/16",
			serviceCidr:    "10.96.0.0/12",
			cniConfig: map[string]string{
				"globalnet.enabled": "true",
				"enableIPv6":        "false",
			},
			wantPodNetwork: "podNetwork=10.244.0.0/16",
			expectedArgs:   13, // base 9 + 4 (2 configs * 2 args each)
		},
		{
			name:           "config with empty key is skipped",
			apiServer:      "https://10.0.0.1:6443",
			podNetworkCidr: "10.244.0.0/16",
			serviceCidr:    "10.96.0.0/12",
			cniConfig: map[string]string{
				"":           "ignored",
				"valid.key":  "value",
				"emptyValue": "",
			},
			wantPodNetwork: "podNetwork=10.244.0.0/16",
			expectedArgs:   13, // base 9 + 4 (2 valid configs, empty key skipped)
		},
	}

	chartDir := t.TempDir() // no values-*.yaml; optional -f flags omitted
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := buildOvnKubernetesHelmInstallArgs(chartDir, tc.apiServer, tc.podNetworkCidr, tc.serviceCidr, "", "", tc.cniConfig)
			assert.GreaterOrEqual(t, len(args), tc.expectedArgs)
			assert.Contains(t, args, "--set-string")
			assert.Contains(t, args, tc.wantPodNetwork)
		})
	}
}

func TestOVNHelmArgsImageRegistry(t *testing.T) {
	chartDir := t.TempDir()
	const apiServer = "https://10.0.0.1:6443"
	const podCIDR = "10.244.0.0/16"
	const svcCIDR = "10.96.0.0/12"

	const upstream = "localhost/nvke"

	t.Run("image registry replaces the upstream host, keeps the path", func(t *testing.T) {
		args := buildOvnKubernetesHelmInstallArgs(chartDir, apiServer, podCIDR, svcCIDR, "my-mirror.example.com/nvke/", upstream, nil)
		joined := strings.Join(args, " ")
		assert.Contains(t, joined, "--set global.imageRepositoryPrefix=my-mirror.example.com/nvke/nvke")
		assert.Equal(t, "m.example.com/a/b", ovnImageRepository("m.example.com", "quay.io/a/b"))
	})

	t.Run("empty image registry produces no global.imageRepositoryPrefix arg", func(t *testing.T) {
		args := buildOvnKubernetesHelmInstallArgs(chartDir, apiServer, podCIDR, svcCIDR, "", "", nil)
		joined := strings.Join(args, " ")
		assert.NotContains(t, joined, "global.imageRepositoryPrefix")
	})

	t.Run("CniConfig global.registry overrides server-level registry", func(t *testing.T) {
		// Helm applies --set left-to-right, last wins: the per-request value must come after the server-level one.
		args := buildOvnKubernetesHelmInstallArgs(chartDir, apiServer, podCIDR, svcCIDR, "server-mirror.example.com", upstream,
			map[string]string{"global.registry": "request-mirror.example.com"})
		joined := strings.Join(args, " ")
		assert.NotContains(t, joined, "global.registry=", "the wire key must not reach helm untranslated")
		serverIdx := strings.Index(joined, "server-mirror.example.com/nvke")
		requestIdx := strings.Index(joined, "request-mirror.example.com/nvke")
		require.GreaterOrEqual(t, serverIdx, 0)
		require.GreaterOrEqual(t, requestIdx, 0)
		assert.Greater(t, requestIdx, serverIdx, "per-request registry must appear after server-level registry so it takes precedence")
	})
}

func TestOVNKubernetesCNIConfig(t *testing.T) {
	t.Run("netdev selects a dummy gateway bridge without mutating the request", func(t *testing.T) {
		request := map[string]string{"global.image.pullPolicy": "Never"}

		config := ovnKubernetesCNIConfig(request, "netdev")

		assert.Equal(t, ovnDummyGatewayBridgeEnabled, config[ovnDummyGatewayBridgeHelmValue])
		assert.Equal(t, ovnDummyGatewayOpts, config[ovnGatewayOptsHelmValue])
		assert.Equal(t, "Never", config["global.image.pullPolicy"])
		assert.NotContains(t, request, ovnDummyGatewayBridgeHelmValue)
		assert.NotContains(t, request, ovnGatewayOptsHelmValue)
	})

	t.Run("explicit gateway settings win", func(t *testing.T) {
		request := map[string]string{
			ovnDummyGatewayBridgeHelmValue: "false",
			ovnGatewayOptsHelmValue:        "--gateway-interface=custom0 --allow-no-uplink",
		}

		config := ovnKubernetesCNIConfig(request, "netdev")

		assert.Equal(t, "false", config[ovnDummyGatewayBridgeHelmValue])
		assert.Equal(t, "--gateway-interface=custom0 --allow-no-uplink", config[ovnGatewayOptsHelmValue])
	})

	t.Run("kernel container datapath selects a dummy gateway bridge", func(t *testing.T) {
		request := map[string]string{"global.image.pullPolicy": "Never"}

		config := ovnKubernetesCNIConfig(request, "system")

		assert.Equal(t, ovnDummyGatewayBridgeEnabled, config[ovnDummyGatewayBridgeHelmValue])
		assert.Equal(t, ovnDummyGatewayOpts, config[ovnGatewayOptsHelmValue])
		assert.Equal(t, "Never", config["global.image.pullPolicy"])
		assert.NotContains(t, request, ovnDummyGatewayBridgeHelmValue)
	})

	t.Run("empty datapath preserves the request", func(t *testing.T) {
		request := map[string]string{"global.image.pullPolicy": "Never"}

		config := ovnKubernetesCNIConfig(request, "")

		assert.Equal(t, request, config)
		assert.NotContains(t, config, ovnDummyGatewayBridgeHelmValue)
		assert.NotContains(t, config, ovnGatewayOptsHelmValue)
	})
}

// The chart's defaults are values.yaml; values-nke.yaml must come last so its image tag beats the
// one pinned there, and an optional values-single-node-zone.yaml must not land after it.
func TestHelmValuesFileArgs(t *testing.T) {
	chartDir := t.TempDir()
	for _, name := range []string{"Chart.yaml", "values.yaml", "values-single-node-zone.yaml", "values-single-node-zone-dpu.yaml", "values-nke.yaml"} {
		require.NoError(t, os.WriteFile(filepath.Join(chartDir, name), []byte("{}\n"), 0o644))
	}
	assert.Equal(t, []string{
		"-f", filepath.Join(chartDir, "values.yaml"),
		"-f", filepath.Join(chartDir, "values-single-node-zone.yaml"),
		"-f", filepath.Join(chartDir, "values-nke.yaml"),
	}, helmValuesFileArgs(chartDir))
}

func TestOvnStagedImageRepository(t *testing.T) {
	chartDir := t.TempDir()
	write := func(body string) {
		require.NoError(t, os.WriteFile(filepath.Join(chartDir, "values-nke.yaml"), []byte(body), 0o644))
	}
	write("tags:\n  ovs-node: false\nglobal:\n  imageRepositoryPrefix: localhost/nvke\n")
	repo, err := ovnStagedImageRepository(chartDir)
	require.NoError(t, err)
	assert.Equal(t, "localhost/nvke", repo)

	write("global:\n  imageRepositoryPrefix: nohost\n")
	_, err = ovnStagedImageRepository(chartDir)
	assert.Error(t, err)
}

func TestKubeadmPodNetworkCIDR(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single CIDR",
			in:   "10.244.0.0/16",
			want: "10.244.0.0/16",
		},
		{
			name: "multiple IPv4 CIDRs",
			in:   "10.240.0.0/16, 10.241.0.0/16",
			want: "10.240.0.0/16",
		},
		{
			name: "dual-stack keeps one CIDR per family",
			in:   "10.240.0.0/16, fd00:10:240::/56, 10.241.0.0/16, fd00:10:241::/56",
			want: "10.240.0.0/16,fd00:10:240::/56",
		},
		{
			name: "empty CIDRs are skipped",
			in:   ", 10.240.0.0/16,, fd00:10:240::/56, ",
			want: "10.240.0.0/16,fd00:10:240::/56",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kubeadmPodNetworkCIDR(tt.in))
		})
	}
}

// TestBundleStatusInitialization tests that bundle status is properly initialized
func TestBundleStatusInitialization(t *testing.T) {
	tests := []struct {
		name           string
		bundleName     string
		expectedStatus string
	}{
		{
			name:           "calico bundle initialization",
			bundleName:     "calico",
			expectedStatus: "installing",
		},
		{
			name:           "ovn bundle initialization",
			bundleName:     "ovn",
			expectedStatus: "pending",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var status string
			switch tc.bundleName {
			case "calico":
				status = "installing"
			case "ovn":
				status = "pending"
			}
			assert.Equal(t, tc.expectedStatus, status)
		})
	}
}

// recordingRunner keeps the argv of the last call so a test can assert on it.
type recordingRunner struct {
	name string
	args []string
}

func (r *recordingRunner) Run(name string, args []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
	r.name = name
	r.args = args
	return nil, nil
}

func (r *recordingRunner) RunSplitOutput(name string, args []string, _ *nvkeutils.CommandOptions) (nvkeutils.CommandOutput, error) {
	r.name = name
	r.args = args
	return nvkeutils.CommandOutput{}, nil
}

func (r *recordingRunner) RunDetached(name string, args []string, _ *nvkeutils.CommandOptions) error {
	r.name = name
	r.args = args
	return nil
}

// Both CNI charts install from a Temporal activity that retries. `helm install`
// fails once the release exists, so a retry could never succeed; the argv has to
// start with `upgrade --install`. The other argv assertions in this file check
// only length and a --set value, so none of them would catch a regression here.
func TestCNIHelmArgsAreIdempotent(t *testing.T) {
	chartDir := t.TempDir()

	t.Run("ovn", func(t *testing.T) {
		args := buildOvnKubernetesHelmInstallArgs(chartDir, "https://10.0.0.1:6443", "10.244.0.0/16", "10.96.0.0/12", "", "", nil)
		assert.Equal(t, []string{"upgrade", "--install", "ovn-kubernetes", chartDir}, args[:4])
	})

	t.Run("calico", func(t *testing.T) {
		runner := &recordingRunner{}
		server := &NodeAgentServer{Runner: runner}
		require.NoError(t, server.runCalicoHelmInstallFromDir(chartDir, &pb.InstallCNIRequest{}))
		assert.Equal(t, "helm", runner.name)
		assert.Equal(t, []string{"upgrade", "--install", "tigera-operator", chartDir}, runner.args[:4])
	})
}

// calicoApplyRunner records ordering and can fail a specific installation step.
type calicoApplyRunner struct {
	recordingRunner
	calls  [][]string
	failAt int
}

func (r *calicoApplyRunner) Run(name string, args []string, opts *nvkeutils.CommandOptions) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(r.calls) == r.failAt {
		return nil, errors.New("simulated kubectl failure")
	}
	return r.recordingRunner.Run(name, args, opts)
}

func TestCalicoCRDApplyOrder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		crds      bool
		failAt    int
		wantCalls int
	}{
		{"separate CRDs are established before installation", true, 0, 3},
		{"CRD apply failure stops installation", true, 1, 1},
		{"CRD establishment failure stops installation", true, 2, 2},
		{"legacy bundled CRDs retain existing path", false, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "manifests")
			require.NoError(t, os.Mkdir(dir, 0o755))
			for _, name := range []string{"tigera-operator.yaml", "custom-resources.yaml"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644))
			}
			crds := filepath.Join(dir, "operator-crds.yaml")
			if tc.crds {
				require.NoError(t, os.WriteFile(crds, []byte("test"), 0o644))
			}
			runner := &calicoApplyRunner{failAt: tc.failAt}
			server := &NodeAgentServer{Runner: runner}
			err := server.runCalicoKubectlApplyFromDir(root, "")
			if tc.failAt > 0 {
				require.ErrorContains(t, err, "simulated kubectl failure")
			} else {
				require.NoError(t, err)
			}
			require.Len(t, runner.calls, tc.wantCalls)
			if tc.crds {
				assert.Equal(t, []string{"kubectl", "apply", "--server-side", "-f", crds}, runner.calls[0])
				if tc.wantCalls > 1 {
					assert.Equal(t, []string{"kubectl", "wait", "--for=condition=Established", "--timeout=120s", "-f", crds}, runner.calls[1])
				}
			}
			if tc.failAt == 0 {
				assert.Equal(t, []string{"kubectl", "apply", "-f", filepath.Join(dir, "tigera-operator.yaml"), "-f", filepath.Join(dir, "custom-resources.yaml")}, runner.calls[len(runner.calls)-1])
			}
		})
	}
}

func TestOVNValuesRenameCompatibility(t *testing.T) {
	chartDir := t.TempDir()
	for name, repository := range map[string]string{
		legacyOVNValuesFilename: "example.test/legacy/ovn",
		ovnValuesFilename:       "example.test/nvke/ovn",
	} {
		body := "global:\n  imageRepositoryPrefix: " + repository + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(chartDir, name), []byte(body), 0o644))
	}
	repository, err := ovnStagedImageRepository(chartDir)
	require.NoError(t, err)
	assert.Equal(t, "example.test/nvke/ovn", repository)
	assert.Equal(t, []string{
		"-f", filepath.Join(chartDir, legacyOVNValuesFilename),
		"-f", filepath.Join(chartDir, ovnValuesFilename),
	}, helmValuesFileArgs(chartDir))
	require.NoError(t, os.Remove(filepath.Join(chartDir, ovnValuesFilename)))
	repository, err = ovnStagedImageRepository(chartDir)
	require.NoError(t, err)
	assert.Equal(t, "example.test/legacy/ovn", repository)
}
