// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

const (
	kubeVipManifestPath        = "/etc/kubernetes/manifests/kube-vip.yaml"
	kubeVipKubeconfigPath      = "/etc/kubernetes/kube-vip.conf"
	kubeVipContainerKubeconfig = "/etc/kubernetes/admin.conf"
	kubeVipCACertPath          = "/etc/kubernetes/pki/ca.crt"
	kubeVipFMDSASNURL          = "http://169.254.169.254:7777/latest/meta-data/asn"
	kubeVipImage               = "ghcr.io/kube-vip/kube-vip:v1.2.0@sha256:fe8c7b6641ba7633a12b337821e9e83a6456e8edc3434f97577058edd4eecaf6"
	kubeVipLocalAPIServer      = "https://kubernetes:6443"
	kubeVipFMDSConnectTimeout  = 10 * time.Second
	kubeVipFMDSRequestTimeout  = 30 * time.Second
	kubeVipJSONFlag            = "-j"
	kubeVipAddressAction       = "addr"
	kubeVipDeleteAction        = "del"
	kubeVipDeviceFlag          = "dev"
	kubeVipFalse               = "false"
	kubeVipTrue                = "true"
)

type kubeVipAddress struct {
	Interface string `json:"ifname"`
	Addresses []struct {
		Local string `json:"local"`
	} `json:"addr_info"`
}

type kubeVipRoute struct {
	Destination string `json:"dst"`
	Gateway     string `json:"gateway"`
}

// ReconcileKubeVip converges NKE's credential-less, active/active BGP
// kube-vip static pod. The fixed manifest deliberately excludes every
// Kubernetes API-backed kube-vip feature and models the single-peer settings
// CWE historically supplied to the runbook rather than its operator-only
// environment overrides.
func (s *NodeAgentServer) ReconcileKubeVip(
	ctx context.Context,
	req *pb.ReconcileKubeVipRequest,
) (*pb.ReconcileKubeVipResponse, error) {
	defer logRPCSpan(ctx, "ReconcileKubeVip")()

	if !s.isKubeVipEligibleRole() {
		return nil, status.Error(codes.FailedPrecondition, "kube-vip reconciliation requires a control-plane or unassigned node")
	}
	if err := validateKubeVipRequest(req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid kube-vip request: %v", err)
	}

	addresses, err := s.readKubeVipAddresses()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "discover node interfaces: %v", err)
	}
	// NODE_IP selects the BGP source interface. The advertised /32 VIP remains
	// on loopback so it is independent of physical-interface address changes.
	bgpInterface, err := interfaceForAddress(addresses, req.GetNodeIp())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "discover BGP interface: %v", err)
	}

	peerAddress := req.GetBgpPeerAddress()
	if peerAddress == "" {
		// Forge's subnet gateway is the historical single-peer default.
		peerAddress, err = s.defaultKubeVipGateway()
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "discover BGP peer address: %v", err)
		}
	}
	peerASN, err := s.resolveKubeVipPeerASN(ctx, req)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "resolve BGP peer ASN: %v", err)
	}

	kubeconfig, err := credentiallessKubeVipConfig()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "render credential-less kubeconfig: %v", err)
	}
	manifest, err := renderKubeVipManifest(req, bgpInterface, peerAddress, peerASN)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "render kube-vip manifest: %v", err)
	}

	// The static-pod manifest is the activation boundary: kubelet may start it as
	// soon as the rename completes. Converge its kubeconfig and remove stale VIP
	// assignments first so the pod never starts with missing or duplicate state.
	kubeconfigChanged, err := s.writeKubeVipFile(kubeVipKubeconfigPath, kubeconfig)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reconcile credential-less kubeconfig: %v", err)
	}
	// Preserve runbook convergence after an interface change: remove stale
	// copies without flapping the desired loopback address on a no-op rerun.
	cleanedInterfaces := s.cleanupKubeVipInterfaces(addresses, req.GetVipAddress(), loopbackInterface)
	outcome, err := s.writeKubeVipManifest(manifest)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reconcile kube-vip manifest: %v", err)
	}

	slog.InfoContext(ctx, "reconciled kube-vip static pod",
		"outcome", outcome.String(),
		"vip", req.GetVipAddress(),
		"bgpInterface", bgpInterface,
		"bgpPeerAddress", peerAddress,
		"bgpPeerASN", peerASN,
		"credentiallessKubeconfigChanged", kubeconfigChanged,
	)
	return &pb.ReconcileKubeVipResponse{
		Outcome:           outcome,
		ManifestPath:      kubeVipManifestPath,
		BgpInterface:      bgpInterface,
		BgpPeerAddress:    peerAddress,
		BgpPeerAsn:        peerASN,
		KubeconfigPath:    kubeVipKubeconfigPath,
		CleanedInterfaces: cleanedInterfaces,
		KubeconfigChanged: kubeconfigChanged,
	}, nil
}

// isKubeVipEligibleRole permits the unassigned role used before kubeadm init.
// CWE selects the target from a control-plane node pool and must configure
// kube-vip before the static control-plane manifests exist. Once the agent has
// assigned a role, only a control-plane node remains eligible.
func (s *NodeAgentServer) isKubeVipEligibleRole() bool {
	if s == nil || s.NodeStatus == nil {
		return false
	}
	role := strings.TrimSpace(s.NodeStatus.Role)
	return role == "" || strings.EqualFold(role, ControlPlaneRole)
}

func validateKubeVipRequest(req *pb.ReconcileKubeVipRequest) error {
	if req == nil {
		return fmt.Errorf("request is required")
	}
	if err := validateKubeVipIPv4("vip_address", req.GetVipAddress()); err != nil {
		return err
	}
	if err := validateKubeVipIPv4("node_ip", req.GetNodeIp()); err != nil {
		return err
	}
	if req.GetBgpAsn() == 0 {
		return fmt.Errorf("bgp_asn must be non-zero")
	}
	if req.GetBgpPeerAddress() != "" {
		if err := validateKubeVipIPv4("bgp_peer_address", req.GetBgpPeerAddress()); err != nil {
			return err
		}
	}
	switch req.GetBgpPeerAsnSource() {
	case pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_UNSPECIFIED:
		return fmt.Errorf("bgp_peer_asn_source must be STATIC or FMDS")
	case pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_STATIC:
		if req.GetBgpPeerAsn() == 0 {
			return fmt.Errorf("bgp_peer_asn must be non-zero for a static peer ASN")
		}
	case pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_FMDS:
		if req.GetBgpPeerAsn() != 0 {
			return fmt.Errorf("bgp_peer_asn must be zero when the peer ASN comes from FMDS")
		}
	default:
		return fmt.Errorf("bgp_peer_asn_source must be STATIC or FMDS")
	}
	return nil
}

func validateKubeVipIPv4(field, value string) error {
	if strings.TrimSpace(value) != value || value == "" {
		return fmt.Errorf("%s must be set without surrounding whitespace", field)
	}
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() {
		return fmt.Errorf("%s must be an IPv4 address", field)
	}
	return nil
}

func (s *NodeAgentServer) readKubeVipAddresses() ([]kubeVipAddress, error) {
	output, err := s.Runner.Run("ip", []string{kubeVipJSONFlag, kubeVipAddressAction}, nil)
	if err != nil {
		return nil, fmt.Errorf("ip -j addr: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var addresses []kubeVipAddress
	if err := json.Unmarshal(output, &addresses); err != nil {
		return nil, fmt.Errorf("parse ip address JSON: %w", err)
	}
	return addresses, nil
}

func interfaceForAddress(addresses []kubeVipAddress, address string) (string, error) {
	for _, current := range addresses {
		for _, assigned := range current.Addresses {
			if assigned.Local == address {
				return current.Interface, nil
			}
		}
	}
	return "", fmt.Errorf("no interface owns node IP %s", address)
}

func (s *NodeAgentServer) defaultKubeVipGateway() (string, error) {
	output, err := s.Runner.Run("ip", []string{kubeVipJSONFlag, "route"}, nil)
	if err != nil {
		return "", fmt.Errorf("ip -j route: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var routes []kubeVipRoute
	if err := json.Unmarshal(output, &routes); err != nil {
		return "", fmt.Errorf("parse IP route JSON: %w", err)
	}
	for _, route := range routes {
		if route.Destination == "default" {
			if err := validateKubeVipIPv4("default gateway", route.Gateway); err != nil {
				return "", err
			}
			return route.Gateway, nil
		}
	}
	return "", fmt.Errorf("default gateway is not present")
}

func (s *NodeAgentServer) resolveKubeVipPeerASN(ctx context.Context, req *pb.ReconcileKubeVipRequest) (uint32, error) {
	if req.GetBgpPeerAsnSource() == pb.ReconcileKubeVipRequest_BGP_PEER_ASN_SOURCE_STATIC {
		return req.GetBgpPeerAsn(), nil
	}
	if s.kubeVipFetchASN != nil {
		return s.kubeVipFetchASN(ctx)
	}
	return fetchKubeVipASNFromFMDS(ctx)
}

func fetchKubeVipASNFromFMDS(ctx context.Context) (uint32, error) {
	// FNN sites learn the DPU peer ASN from Forge Metadata Service. Bound both
	// connection establishment and the complete request so reconciliation does
	// not hang during metadata-service failures.
	dialer := &net.Dialer{Timeout: kubeVipFMDSConnectTimeout}
	transport := &http.Transport{DialContext: dialer.DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   kubeVipFMDSRequestTimeout,
		Transport: transport,
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, kubeVipFMDSASNURL, nil)
	if err != nil {
		return 0, fmt.Errorf("create FMDS request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("request FMDS ASN: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("FMDS returned HTTP %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil {
		return 0, fmt.Errorf("read FMDS ASN: %w", err)
	}
	asn, err := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 32)
	if err != nil || asn == 0 {
		return 0, fmt.Errorf("FMDS returned invalid ASN %q", strings.TrimSpace(string(body)))
	}
	return uint32(asn), nil
}

func (s *NodeAgentServer) cleanupKubeVipInterfaces(addresses []kubeVipAddress, vip, desiredInterface string) []string {
	var cleaned []string
	for _, current := range addresses {
		if current.Interface == desiredInterface {
			continue
		}
		for _, assigned := range current.Addresses {
			if assigned.Local != vip {
				continue
			}
			output, err := s.Runner.Run("ip", []string{kubeVipAddressAction, kubeVipDeleteAction, vip + "/32", kubeVipDeviceFlag, current.Interface}, nil)
			if err != nil {
				slog.Warn("failed to remove stale kube-vip address",
					"vip", vip,
					"interface", current.Interface,
					"err", err,
					"output", strings.TrimSpace(string(output)),
				)
				continue
			}
			cleaned = append(cleaned, current.Interface)
			break
		}
	}
	return cleaned
}

func credentiallessKubeVipConfig() ([]byte, error) {
	return clientcmd.Write(credentiallessKubeVipAPIConfig())
}

func credentiallessKubeVipAPIConfig() clientcmdapi.Config {
	const name = "kube-vip"
	// The empty AuthInfo is intentional: current fixed-mode kube-vip only needs
	// client-go to construct successfully. If an API-backed feature is enabled
	// later, its requests fail closed as unauthenticated instead of inheriting a
	// control-plane private key.
	return clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			name: {
				Server:               kubeVipLocalAPIServer,
				CertificateAuthority: kubeVipCACertPath,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{name: {}},
		Contexts: map[string]*clientcmdapi.Context{
			name: {Cluster: name, AuthInfo: name},
		},
		CurrentContext: name,
	}
}

func (s *NodeAgentServer) writeKubeVipManifest(manifest []byte) (pb.ReconcileKubeVipResponse_Outcome, error) {
	path := s.rootedPath(kubeVipManifestPath)
	_, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return pb.ReconcileKubeVipResponse_OUTCOME_UNSPECIFIED, fmt.Errorf("stat %s: %w", kubeVipManifestPath, statErr)
	}
	changed, err := s.writeKubeVipFile(kubeVipManifestPath, manifest)
	if err != nil {
		return pb.ReconcileKubeVipResponse_OUTCOME_UNSPECIFIED, err
	}
	if !changed {
		return pb.ReconcileKubeVipResponse_OUTCOME_UNCHANGED, nil
	}
	if existed {
		return pb.ReconcileKubeVipResponse_OUTCOME_UPDATED, nil
	}
	return pb.ReconcileKubeVipResponse_OUTCOME_CREATED, nil
}

// writeKubeVipFile compares the complete desired state before an atomic write.
// This catches every manifest change while avoiding an unnecessary static-pod
// restart when the content is already current. atomicWriteFile renders through
// a hidden file in the destination directory, which kubelet ignores until the
// same-filesystem rename makes the complete manifest visible.
func (s *NodeAgentServer) writeKubeVipFile(path string, desired []byte) (bool, error) {
	resolved := s.rootedPath(path)
	if err := validateKubeVipFileDestination(resolved); err != nil {
		return false, fmt.Errorf("invalid destination %s: %w", path, err)
	}
	current, err := os.ReadFile(resolved)
	if err == nil && bytes.Equal(current, desired) {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return false, fmt.Errorf("create parent directory for %s: %w", path, err)
	}
	if err := validateKubeVipFileDestination(resolved); err != nil {
		return false, fmt.Errorf("invalid destination %s: %w", path, err)
	}
	if err := atomicWriteFile(resolved, desired, 0o644); err != nil {
		return false, fmt.Errorf("write %s atomically: %w", path, err)
	}
	written, err := os.ReadFile(resolved)
	if err != nil {
		return false, fmt.Errorf("verify %s: %w", path, err)
	}
	if !bytes.Equal(written, desired) {
		return false, fmt.Errorf("verify %s: content differs after atomic write", path)
	}
	return true, nil
}

func validateKubeVipFileDestination(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect destination: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("destination must be a regular file, got mode %s", info.Mode())
	}
	return nil
}

func renderKubeVipManifest(
	req *pb.ReconcileKubeVipRequest,
	bgpInterface, peerAddress string,
	peerASN uint32,
) ([]byte, error) {
	falseValue := false
	fileType := corev1.HostPathFile
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeVipContainerName,
			Namespace: kubeSystemNamespace,
			Annotations: map[string]string{
				"scheduler.alpha.kubernetes.io/critical-pod": "",
			},
			Labels: map[string]string{
				"component": "kube-vip",
				"tier":      "control-plane",
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &falseValue,
			HostNetwork:                  true,
			HostAliases: []corev1.HostAlias{{
				IP:        "127.0.0.1",
				Hostnames: []string{"kubernetes"},
			}},
			// v1.2.0 introduced the control-plane health check used below. Keep
			// the readable tag and immutable multi-arch digest together.
			Containers: []corev1.Container{{
				Name:            kubeVipContainerName,
				Image:           kubeVipImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"manager"},
				Env:             kubeVipEnvironment(req, bgpInterface, peerAddress, peerASN),
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "kubeconfig", MountPath: kubeVipContainerKubeconfig, ReadOnly: true},
					{Name: "health-check-ca", MountPath: kubeVipCACertPath, ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "kubeconfig",
					VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
						Path: kubeVipKubeconfigPath,
						Type: &fileType,
					}},
				},
				{
					Name: "health-check-ca",
					VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
						Path: kubeVipCACertPath,
						Type: &fileType,
					}},
				},
			},
		},
	}
	return yaml.Marshal(pod)
}

func kubeVipEnvironment(
	req *pb.ReconcileKubeVipRequest,
	bgpInterface, peerAddress string,
	peerASN uint32,
) []corev1.EnvVar {
	// Probe the node IP rather than localhost because kubeadm's serving
	// certificate covers node addresses and the VIP, not 127.0.0.1. /readyz is
	// intentional: a starting or gracefully stopping API server must withdraw
	// its ECMP path even while /livez still reports the process alive. Because
	// readyz includes dependencies, the five-second period and three-failure
	// threshold damp transient route churn. The 10s timeout accommodates its
	// etcd linearizable-read path under load, and the mounted kubeadm CA verifies
	// the private HTTPS serving certificate.
	return []corev1.EnvVar{
		{Name: "vip_arp", Value: kubeVipFalse},
		{Name: "port", Value: "6443"},
		{Name: "vip_nodename", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "vip_interface", Value: loopbackInterface},
		{Name: "vip_cidr", Value: "32"},
		{Name: "dns_mode", Value: "first"},
		{Name: "cp_enable", Value: kubeVipTrue},
		{Name: "cp_namespace", Value: kubeSystemNamespace},
		{Name: "svc_enable", Value: kubeVipFalse},
		{Name: "svc_leasename", Value: "plndr-svcs-lock"},
		{Name: "bgp_enable", Value: kubeVipTrue},
		{Name: "address", Value: req.GetVipAddress()},
		{Name: "bgp_peeraddress", Value: peerAddress},
		{Name: "bgp_peeras", Value: strconv.FormatUint(uint64(peerASN), 10)},
		{Name: "bgp_as", Value: strconv.FormatUint(uint64(req.GetBgpAsn()), 10)},
		{Name: "bgp_routerid", Value: req.GetNodeIp()},
		{Name: "bgp_sourceif", Value: bgpInterface},
		{Name: "prometheus_server", Value: ":2112"},
		{Name: "control_plane_health_check_address", Value: "https://" + req.GetNodeIp() + ":6443/readyz"},
		{Name: "control_plane_health_check_period_seconds", Value: "5"},
		{Name: "control_plane_health_check_timeout_seconds", Value: "10"},
		{Name: "control_plane_health_check_failure_threshold", Value: "3"},
		{Name: "control_plane_health_check_ca_path", Value: kubeVipCACertPath},
	}
}
