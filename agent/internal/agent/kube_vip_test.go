// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/agent/pkg/utils"
)

func TestReconcileKubeVipValidatesRequest(t *testing.T) {
	valid := func() *pb.ReconcileKubeVipRequest {
		return &pb.ReconcileKubeVipRequest{
			VipAddress:       "192.168.100.10",
			BgpAsn:           65100,
			NodeIp:           "192.168.100.20",
			BgpPeerAsnSource: pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_STATIC,
			BgpPeerAsn:       4294967000,
		}
	}

	tests := []struct {
		name    string
		role    string
		request func() *pb.ReconcileKubeVipRequest
		code    codes.Code
	}{
		{name: "worker node", role: WorkerRole, request: valid, code: codes.FailedPrecondition},
		{name: "nil request", role: ControlPlaneRole, request: func() *pb.ReconcileKubeVipRequest { return nil }, code: codes.InvalidArgument},
		{name: "invalid VIP", role: ControlPlaneRole, request: func() *pb.ReconcileKubeVipRequest {
			req := valid()
			req.VipAddress = "not-an-ip"
			return req
		}, code: codes.InvalidArgument},
		{name: "IPv6 node IP", role: ControlPlaneRole, request: func() *pb.ReconcileKubeVipRequest {
			req := valid()
			req.NodeIp = "2001:db8::1"
			return req
		}, code: codes.InvalidArgument},
		{name: "zero local ASN", role: ControlPlaneRole, request: func() *pb.ReconcileKubeVipRequest {
			req := valid()
			req.BgpAsn = 0
			return req
		}, code: codes.InvalidArgument},
		{name: "unspecified peer ASN source", role: ControlPlaneRole, request: func() *pb.ReconcileKubeVipRequest {
			req := valid()
			req.BgpPeerAsnSource = pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_UNSPECIFIED
			return req
		}, code: codes.InvalidArgument},
		{name: "static source without ASN", role: ControlPlaneRole, request: func() *pb.ReconcileKubeVipRequest {
			req := valid()
			req.BgpPeerAsn = 0
			return req
		}, code: codes.InvalidArgument},
		{name: "FMDS source with static ASN", role: ControlPlaneRole, request: func() *pb.ReconcileKubeVipRequest {
			req := valid()
			req.BgpPeerAsnSource = pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_FMDS
			return req
		}, code: codes.InvalidArgument},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &NodeAgentServer{NodeStatus: &NodeStatus{Role: test.role}}
			response, err := server.ReconcileKubeVip(context.Background(), test.request())
			require.Error(t, err)
			assert.Nil(t, response)
			assert.Equal(t, test.code, status.Code(err))
		})
	}
}

func TestReconcileKubeVipConvergesManifest(t *testing.T) {
	const (
		nodeIP         = "192.168.100.20"
		vip            = "192.168.100.10"
		defaultGateway = "192.168.100.1"
	)
	addressJSON := `[
  {"ifname":"lo","addr_info":[{"local":"127.0.0.1"}]},
  {"ifname":"eth0","addr_info":[{"local":"192.168.100.20"}]},
  {"ifname":"eth1","addr_info":[{"local":"192.168.100.10"}]}
]`
	routeJSON := `[{"dst":"default","gateway":"192.168.100.1"}]`

	tests := []struct {
		name            string
		role            string
		source          pb.ReconcileKubeVipRequest_BGPPeerASNSource
		staticASN       uint32
		fmdsASN         uint32
		fmdsEndpoint    string
		wantEndpoint    string
		wantPeerASN     uint32
		wantConfigFetch bool
		preexisting     bool
		wantOutcome     pb.ReconcileKubeVipResponse_Outcome
	}{
		{
			name:        "static peer ASN on unassigned pre-init node",
			role:        "",
			source:      pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_STATIC,
			staticASN:   4294967000,
			wantPeerASN: 4294967000,
			wantOutcome: pb.ReconcileKubeVipResponse_OUTCOME_CREATED,
		},
		{
			name:            "FMDS peer ASN from requested endpoint",
			role:            ControlPlaneRole,
			source:          pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_FMDS,
			fmdsASN:         64512,
			fmdsEndpoint:    "http://169.254.169.254/latest/meta-data/asn",
			wantEndpoint:    "http://169.254.169.254/latest/meta-data/asn",
			wantPeerASN:     64512,
			wantConfigFetch: true,
			preexisting:     true,
			wantOutcome:     pb.ReconcileKubeVipResponse_OUTCOME_UPDATED,
		},
		{
			name:            "FMDS peer ASN from legacy endpoint",
			role:            ControlPlaneRole,
			source:          pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_FMDS,
			fmdsASN:         64513,
			wantEndpoint:    defaultForgeFMDSASNURL,
			wantPeerASN:     64513,
			wantConfigFetch: true,
			wantOutcome:     pb.ReconcileKubeVipResponse_OUTCOME_CREATED,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var fetchCalls int
			runner := &MockRunner{Runners: []runner{
				runForCommand("ip", []string{"-j", "addr"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
					return []byte(addressJSON), nil
				}),
				runForCommand("ip", []string{"-j", "route"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
					return []byte(routeJSON), nil
				}),
				runForCommand("ip", []string{"addr", "del", vip + "/32", "dev", "eth1"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
					return nil, nil
				}),
			}}
			server := &NodeAgentServer{
				Runner:     runner,
				NodeStatus: &NodeStatus{Role: test.role},
				RootPath:   t.TempDir(),
				kubeVipFetchASN: func(_ context.Context, endpoint string) (uint32, error) {
					fetchCalls++
					assert.Equal(t, test.wantEndpoint, endpoint)
					return test.fmdsASN, nil
				},
			}
			request := &pb.ReconcileKubeVipRequest{
				VipAddress:       vip,
				BgpAsn:           65100,
				NodeIp:           nodeIP,
				BgpPeerAsnSource: test.source,
				BgpPeerAsn:       test.staticASN,
				FmdsEndpoint:     test.fmdsEndpoint,
			}
			manifestPath := filepath.Join(server.RootPath, "etc", "kubernetes", "manifests", "kube-vip.yaml")
			if test.preexisting {
				require.NoError(t, os.MkdirAll(filepath.Dir(manifestPath), 0o755))
				require.NoError(t, os.WriteFile(manifestPath, []byte("stale manifest"), 0o644))
			}

			response, err := server.ReconcileKubeVip(context.Background(), request)
			require.NoError(t, err)
			assert.Equal(t, test.wantOutcome, response.GetOutcome())
			assert.Equal(t, "eth0", response.GetBgpInterface())
			assert.Equal(t, defaultGateway, response.GetBgpPeerAddress())
			assert.Equal(t, test.wantPeerASN, response.GetBgpPeerAsn())
			assert.Equal(t, []string{"eth1"}, response.GetCleanedInterfaces())
			assert.True(t, response.GetKubeconfigChanged())
			if test.wantConfigFetch {
				assert.Equal(t, 1, fetchCalls)
			} else {
				assert.Zero(t, fetchCalls)
			}

			manifest := requireFile(t, manifestPath)
			assert.NotContains(t, string(manifest), "super-admin.conf")
			assert.NotContains(t, string(manifest), "/etc/kubernetes/admin.conf\n      path")
			assertKubeVipPod(t, manifest, test.wantPeerASN)

			kubeconfig := requireFile(t, filepath.Join(server.RootPath, "etc", "kubernetes", "kube-vip.conf"))
			assertCredentiallessKubeVipConfig(t, kubeconfig)

			response, err = server.ReconcileKubeVip(context.Background(), request)
			require.NoError(t, err)
			assert.Equal(t, pb.ReconcileKubeVipResponse_OUTCOME_UNCHANGED, response.GetOutcome())
			assert.False(t, response.GetKubeconfigChanged())
		})
	}
}

func TestFetchKubeVipASNFromFMDSEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/latest/meta-data/asn", request.URL.Path)
		_, err := response.Write([]byte("4268052677\n"))
		require.NoError(t, err)
	}))
	defer server.Close()

	asn, err := fetchKubeVipASNFromFMDS(context.Background(), server.URL+"/latest/meta-data/asn")
	require.NoError(t, err)
	assert.Equal(t, uint32(4268052677), asn)
}

func TestCredentiallessKubeVipConfigBuildsClient(t *testing.T) {
	raw, err := credentiallessKubeVipConfig()
	require.NoError(t, err)
	assertCredentiallessKubeVipConfig(t, raw)

	config := credentiallessKubeVipAPIConfig()
	config.Clusters["kube-vip"].CertificateAuthority = ""
	config.Clusters["kube-vip"].CertificateAuthorityData = testCertificateAuthority(t)
	raw, err = clientcmd.Write(config)
	require.NoError(t, err)
	loadedConfig, err := clientcmd.Load(raw)
	require.NoError(t, err)
	restConfig, err := clientcmd.NewNonInteractiveClientConfig(*loadedConfig, loadedConfig.CurrentContext, nil, nil).ClientConfig()
	require.NoError(t, err)
	assert.Empty(t, restConfig.BearerToken)
	assert.Empty(t, restConfig.Username)
	assert.Empty(t, restConfig.Password)
	assert.Empty(t, restConfig.CertFile)
	assert.Empty(t, restConfig.KeyFile)
	assert.Empty(t, restConfig.CertData)
	assert.Empty(t, restConfig.KeyData)
	assert.Empty(t, restConfig.CAFile)
	assert.NotEmpty(t, restConfig.CAData)
	_, err = kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
}

func testCertificateAuthority(t *testing.T) []byte {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(4102444800, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})
}

func assertCredentiallessKubeVipConfig(t *testing.T, raw []byte) {
	t.Helper()
	config, err := clientcmd.Load(raw)
	require.NoError(t, err)
	require.Len(t, config.Clusters, 1)
	cluster := config.Clusters["kube-vip"]
	require.NotNil(t, cluster)
	assert.Equal(t, kubeVipLocalAPIServer, cluster.Server)
	assert.Equal(t, kubeVipCACertPath, cluster.CertificateAuthority)
	assert.Empty(t, cluster.CertificateAuthorityData)
	require.Len(t, config.AuthInfos, 1)
	for _, auth := range config.AuthInfos {
		assert.Empty(t, auth.Token)
		assert.Empty(t, auth.ClientCertificate)
		assert.Empty(t, auth.ClientCertificateData)
		assert.Empty(t, auth.ClientKey)
		assert.Empty(t, auth.ClientKeyData)
		assert.Empty(t, auth.Username)
		assert.Empty(t, auth.Password)
	}
}

func assertKubeVipPod(t *testing.T, manifest []byte, peerASN uint32) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, yaml.Unmarshal(manifest, &pod))
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	environment := make(map[string]string, len(container.Env))
	for _, variable := range container.Env {
		environment[variable.Name] = variable.Value
	}
	assert.Equal(t, "false", environment["svc_enable"])
	assert.Empty(t, environment["vip_leaderelection"])
	assert.Equal(t, "true", environment["bgp_enable"])
	assert.Equal(t, "192.168.100.10", environment["address"])
	assert.Equal(t, "192.168.100.1", environment["bgp_peeraddress"])
	assert.Equal(t, strconv.FormatUint(uint64(peerASN), 10), environment["bgp_peeras"])
	assert.Equal(t, "eth0", environment["bgp_sourceif"])
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	require.Len(t, pod.Spec.Volumes, 2)
	require.NotNil(t, pod.Spec.Volumes[0].HostPath)
	assert.Equal(t, kubeVipKubeconfigPath, pod.Spec.Volumes[0].HostPath.Path)
	require.NotNil(t, pod.Spec.Volumes[0].HostPath.Type)
	assert.Equal(t, corev1.HostPathFile, *pod.Spec.Volumes[0].HostPath.Type)
	require.NotNil(t, pod.Spec.Volumes[1].HostPath)
	assert.Equal(t, kubeVipCACertPath, pod.Spec.Volumes[1].HostPath.Path)
}

func requireFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return contents
}
