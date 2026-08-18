// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains a local copy of the upstream
// k8s.io/kube-proxy/config/v1alpha1 KubeProxyConfiguration top-level struct.
//
// Source:
//
//	https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/kube-proxy/config/v1alpha1/types.go
//
// Local changes:
//   - only the top-level KubeProxyConfiguration struct is copied locally
//   - nested kube-proxy and component-base types remain imported from upstream modules
//   - top-level json tags are adjusted to use omitzero; map and slice fields
//     also retain omitempty so empty collections are omitted from CWE-rendered
//     YAML unless explicitly set
//
// Refresh process:
//  1. copy KubeProxyConfiguration from the upstream package for the target version
//  2. keep the alias type section in this file in sync with upstream imports
//  3. reapply the top-level tag adjustments described above
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	componentbaseconfigv1alpha1 "k8s.io/component-base/config/v1alpha1"
	logsapi "k8s.io/component-base/logs/api/v1"
	upstream "k8s.io/kube-proxy/config/v1alpha1"
)

const (
	APIVersion                 = "kubeproxy.config.k8s.io/v1alpha1"
	KubeProxyConfigurationKind = "KubeProxyConfiguration"
)

type KubeProxyIPTablesConfiguration = upstream.KubeProxyIPTablesConfiguration

type KubeProxyIPVSConfiguration = upstream.KubeProxyIPVSConfiguration

type KubeProxyNFTablesConfiguration = upstream.KubeProxyNFTablesConfiguration

type KubeProxyConntrackConfiguration = upstream.KubeProxyConntrackConfiguration

type KubeProxyWinkernelConfiguration = upstream.KubeProxyWinkernelConfiguration

type DetectLocalConfiguration = upstream.DetectLocalConfiguration

type ProxyMode = upstream.ProxyMode

type LocalMode = upstream.LocalMode

type KubeProxyConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// featureGates is a map of feature names to bools that enable or disable alpha/experimental features.
	FeatureGates map[string]bool `json:"featureGates,omitempty,omitzero"`

	// clientConnection specifies the kubeconfig file and client connection settings for the proxy
	// server to use when communicating with the apiserver.
	ClientConnection componentbaseconfigv1alpha1.ClientConnectionConfiguration `json:"clientConnection,omitzero"`
	// logging specifies the options of logging.
	// Refer to [Logs Options](https://github.com/kubernetes/component-base/blob/master/logs/options.go)
	// for more information.
	Logging logsapi.LoggingConfiguration `json:"logging,omitzero"`

	// hostnameOverride, if non-empty, will be used as the name of the Node that
	// kube-proxy is running on. If unset, the node name is assumed to be the same as
	// the node's hostname.
	HostnameOverride string `json:"hostnameOverride,omitzero"`
	// bindAddress can be used to override kube-proxy's idea of what its node's
	// primary IP is. Note that the name is a historical artifact, and kube-proxy does
	// not actually bind any sockets to this IP.
	BindAddress string `json:"bindAddress,omitzero"`
	// healthzBindAddress is the IP address and port for the health check server to
	// serve on, defaulting to "0.0.0.0:10256" (if bindAddress is unset or IPv4), or
	// "[::]:10256" (if bindAddress is IPv6).
	HealthzBindAddress string `json:"healthzBindAddress,omitzero"`
	// metricsBindAddress is the IP address and port for the metrics server to serve
	// on, defaulting to "127.0.0.1:10249" (if bindAddress is unset or IPv4), or
	// "[::1]:10249" (if bindAddress is IPv6). (Set to "0.0.0.0:10249" / "[::]:10249"
	// to bind on all interfaces.)
	MetricsBindAddress string `json:"metricsBindAddress,omitzero"`
	// bindAddressHardFail, if true, tells kube-proxy to treat failure to bind to a
	// port as fatal and exit
	BindAddressHardFail bool `json:"bindAddressHardFail,omitzero"`
	// enableProfiling enables profiling via web interface on /debug/pprof handler.
	// Profiling handlers will be handled by metrics server.
	EnableProfiling bool `json:"enableProfiling,omitzero"`
	// showHiddenMetricsForVersion is the version for which you want to show hidden metrics.
	ShowHiddenMetricsForVersion string `json:"showHiddenMetricsForVersion,omitzero"`

	// mode specifies which proxy mode to use.
	Mode ProxyMode `json:"mode,omitzero"`
	// iptables contains iptables-related configuration options.
	IPTables KubeProxyIPTablesConfiguration `json:"iptables,omitzero"`
	// ipvs contains ipvs-related configuration options.
	IPVS KubeProxyIPVSConfiguration `json:"ipvs,omitzero"`
	// nftables contains nftables-related configuration options.
	NFTables KubeProxyNFTablesConfiguration `json:"nftables,omitzero"`
	// winkernel contains winkernel-related configuration options.
	Winkernel KubeProxyWinkernelConfiguration `json:"winkernel,omitzero"`

	// detectLocalMode determines mode to use for detecting local traffic, defaults to ClusterCIDR
	DetectLocalMode LocalMode `json:"detectLocalMode,omitzero"`
	// detectLocal contains optional configuration settings related to DetectLocalMode.
	DetectLocal DetectLocalConfiguration `json:"detectLocal,omitzero"`
	// clusterCIDR is the CIDR range of the pods in the cluster. (For dual-stack
	// clusters, this can be a comma-separated dual-stack pair of CIDR ranges.). When
	// DetectLocalMode is set to ClusterCIDR, kube-proxy will consider
	// traffic to be local if its source IP is in this range. (Otherwise it is not
	// used.)
	ClusterCIDR string `json:"clusterCIDR,omitzero"`

	// nodePortAddresses is a list of CIDR ranges that contain valid node IPs, or
	// alternatively, the single string 'primary'. If set to a list of CIDRs,
	// connections to NodePort services will only be accepted on node IPs in one of
	// the indicated ranges. If set to 'primary', NodePort services will only be
	// accepted on the node's primary IPv4 and/or IPv6 address according to the Node
	// object. If unset, NodePort connections will be accepted on all local IPs.
	NodePortAddresses []string `json:"nodePortAddresses,omitempty,omitzero"`

	// oomScoreAdj is the oom-score-adj value for kube-proxy process. Values must be within
	// the range [-1000, 1000]
	OOMScoreAdj *int32 `json:"oomScoreAdj,omitzero"`
	// conntrack contains conntrack-related configuration options.
	Conntrack KubeProxyConntrackConfiguration `json:"conntrack,omitzero"`
	// configSyncPeriod is how often configuration from the apiserver is refreshed. Must be greater
	// than 0.
	ConfigSyncPeriod metav1.Duration `json:"configSyncPeriod,omitzero"`

	// portRange was previously used to configure the userspace proxy, but is now unused.
	PortRange string `json:"portRange,omitzero"`

	// windowsRunAsService, if true, enables Windows service control manager API integration.
	WindowsRunAsService bool `json:"windowsRunAsService,omitzero"`
}
