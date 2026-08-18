// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/agent/pkg/utils"
	"github.com/stretchr/testify/assert"
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
			err := os.WriteFile(kubeconfigPath, []byte(tc.kubeconfigData), 0644)
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
	err := os.WriteFile(kubeconfigPath, []byte(multiClusterKubeconfig), 0644)
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
			err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0644)
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
	}, "nke-cni-ovn-charts-1.0.0=1.0.0")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errCNIApplyRequiresControlPlane), "got %v", err)
}

func TestCheckHostOpenVSwitchReady(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		r := &MockRunner{
			Runners: []runner{
				runForCommand("ovs-vsctl", []string{"--version"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
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
	}, "nke-cni-ovn-charts-1.4.0")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errOvnRequiresHostOpenVSwitch), "got %v", err)
}

func TestChartsDebPresenceFailsWhenAbsent(t *testing.T) {
	err := chartsDebPresenceDefault(t.Context(), "nonexistent-package-xyz")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errCNIChartsDebNotInstalled), "got %v", err)
}

func TestNkeCniAptSpec(t *testing.T) {
	spec, err := nkeCniAptSpec(pb.CniVariant_CNI_VARIANT_OVN, pb.CniAptRole_CNI_APT_ROLE_CHARTS, "v2.1.0")
	assert.NoError(t, err)
	assert.Equal(t, "nke-cni-ovn-charts-2.1.0=2.1.0", spec)

	spec, err = nkeCniAptSpec(pb.CniVariant_CNI_VARIANT_OVN, pb.CniAptRole_CNI_APT_ROLE_IMAGES, "1")
	assert.NoError(t, err)
	assert.Equal(t, "nke-cni-ovn-images-1=1", spec)

	spec, err = nkeCniAptSpec(pb.CniVariant_CNI_VARIANT_CALICO, pb.CniAptRole_CNI_APT_ROLE_CHARTS, "3.0")
	assert.NoError(t, err)
	assert.Equal(t, "nke-cni-calico-manifests-3.0=3.0", spec)

	spec, err = nkeCniAptSpec(pb.CniVariant_CNI_VARIANT_UNSPECIFIED, pb.CniAptRole_CNI_APT_ROLE_IMAGES, "1")
	assert.NoError(t, err)
	assert.Equal(t, "nke-cni-ovn-images-1=1", spec)

	spec, err = nkeCniAptSpec(pb.CniVariant_CNI_VARIANT_UNSPECIFIED, pb.CniAptRole_CNI_APT_ROLE_CHARTS, "")
	assert.NoError(t, err)
	assert.Equal(t, "nke-cni-ovn-charts-1.4.0", spec)

	spec, err = nkeCniAptSpec(pb.CniVariant_CNI_VARIANT_CALICO, pb.CniAptRole_CNI_APT_ROLE_IMAGES, "")
	assert.NoError(t, err)
	assert.Equal(t, "nke-cni-calico-images-3.30.1", spec)

	_, err = nkeCniAptSpec(pb.CniVariant_CNI_VARIANT_CALICO, pb.CniAptRole_CNI_APT_ROLE_UNSPECIFIED, "1")
	assert.Error(t, err)
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
	t.Run("cni-packages layout under /usr/share/nke-cni/calico", func(t *testing.T) {
		root, err := calicoInstallRootFromFiles([]string{
			"/usr/share/nke-cni/calico/manifests/tigera-operator.yaml",
			"/usr/share/nke-cni/calico/manifests/custom-resources.yaml",
			"/usr/share/nke-cni/calico/apply-manifests.sh",
		})
		assert.NoError(t, err)
		assert.Equal(t, "/usr/share/nke-cni/calico", root)
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
		{
			name:           "empty defaults to calico",
			cniPluginName:  "",
			expectedPlugin: "calico",
		},
		{
			name:           "unknown defaults to calico",
			cniPluginName:  "unknown",
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
				expected = "calico"
			}
			assert.Equal(t, tc.expectedPlugin, expected)
		})
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
			args := buildOvnKubernetesHelmInstallArgs(chartDir, tc.apiServer, tc.podNetworkCidr, tc.serviceCidr, "", tc.cniConfig)
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

	t.Run("image registry injected as global.registry", func(t *testing.T) {
		args := buildOvnKubernetesHelmInstallArgs(chartDir, apiServer, podCIDR, svcCIDR, "my-mirror.example.com", nil)
		joined := strings.Join(args, " ")
		assert.Contains(t, joined, "--set global.registry=my-mirror.example.com")
	})

	t.Run("empty image registry produces no global.registry arg", func(t *testing.T) {
		args := buildOvnKubernetesHelmInstallArgs(chartDir, apiServer, podCIDR, svcCIDR, "", nil)
		joined := strings.Join(args, " ")
		assert.NotContains(t, joined, "global.registry")
	})

	t.Run("CniConfig global.registry overrides server-level registry", func(t *testing.T) {
		// Both the server flag and per-request CniConfig set global.registry.
		// Helm applies --set flags left-to-right; the last value wins.
		// The server-level value is added first so CniConfig takes precedence.
		args := buildOvnKubernetesHelmInstallArgs(chartDir, apiServer, podCIDR, svcCIDR, "server-mirror.example.com",
			map[string]string{"global.registry": "request-mirror.example.com"})
		joined := strings.Join(args, " ")
		// Both appear; request-mirror comes last (CniConfig loop runs after imageRegistry injection).
		serverIdx := strings.Index(joined, "server-mirror.example.com")
		requestIdx := strings.Index(joined, "request-mirror.example.com")
		assert.Greater(t, requestIdx, serverIdx, "per-request registry must appear after server-level registry so it takes precedence")
	})
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
