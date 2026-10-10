/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1beta1 contains a local copy of the upstream
// k8s.io/kubelet/config/v1beta1 KubeletConfiguration top-level struct.
//
// Source (Kubernetes v1.34.0, commit f28b4c9efbca5c5c0af716d9f2d5702667ee8a45):
//
//	https://github.com/kubernetes/kubernetes/blob/v1.34.0/staging/src/k8s.io/kubelet/config/v1beta1/types.go
//
// The revision is inferred; see THIRD_PARTY_NOTICES.md.
//
// Local changes:
//   - only the top-level KubeletConfiguration struct is copied locally
//   - nested kubelet and component-base types remain imported from upstream modules
//   - APIVersion, Kind constant and upstream type aliases are added
//   - doc comment spelling is normalised
//   - top-level json tags are adjusted to use omitzero; map and slice fields
//     also retain omitempty so empty collections are omitted from controller-rendered
//     YAML unless explicitly set
//
// Refresh process:
//  1. copy KubeletConfiguration from the upstream package for the target version
//  2. keep the alias type section in this file in sync with upstream imports
//  3. reapply the top-level tag adjustments described above
package v1beta1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logsapi "k8s.io/component-base/logs/api/v1"
	tracingapi "k8s.io/component-base/tracing/api/v1"
	upstream "k8s.io/kubelet/config/v1beta1"
)

const (
	APIVersion               = "kubelet.config.k8s.io/v1beta1"
	KubeletConfigurationKind = "KubeletConfiguration"
)

type HairpinMode = upstream.HairpinMode

type ResourceChangeDetectionStrategy = upstream.ResourceChangeDetectionStrategy

type ImagePullCredentialsVerificationPolicy = upstream.ImagePullCredentialsVerificationPolicy

type KubeletAuthorization = upstream.KubeletAuthorization

type KubeletAuthentication = upstream.KubeletAuthentication

type MemoryReservation = upstream.MemoryReservation

type ShutdownGracePeriodByPodPriority = upstream.ShutdownGracePeriodByPodPriority

type MemorySwapConfiguration = upstream.MemorySwapConfiguration

type CrashLoopBackOffConfig = upstream.CrashLoopBackOffConfig

type UserNamespaces = upstream.UserNamespaces

type KubeletConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// enableServer enables Kubelet's secured server.
	// Note: Kubelet's insecure port is controlled by the readOnlyPort option.
	// Default: true
	EnableServer *bool `json:"enableServer,omitzero"`
	// staticPodPath is the path to the directory containing local (static) pods to
	// run, or the path to a single static pod file.
	// Default: ""
	// +optional
	StaticPodPath string `json:"staticPodPath,omitzero"`
	// podLogsDir is a custom root directory path kubelet will use to place pod's log files.
	// Default: "/var/log/pods/"
	// Note: it is not recommended to use the temp folder as a log directory as it may cause
	// unexpected behavior in many places.
	// +optional
	PodLogsDir string `json:"podLogsDir,omitzero"`
	// syncFrequency is the max period between synchronizing running
	// containers and config.
	// Default: "1m"
	// +optional
	SyncFrequency metav1.Duration `json:"syncFrequency,omitzero"`
	// fileCheckFrequency is the duration between checking config files for
	// new data.
	// Default: "20s"
	// +optional
	FileCheckFrequency metav1.Duration `json:"fileCheckFrequency,omitzero"`
	// httpCheckFrequency is the duration between checking http for new data.
	// Default: "20s"
	// +optional
	HTTPCheckFrequency metav1.Duration `json:"httpCheckFrequency,omitzero"`
	// staticPodURL is the URL for accessing static pods to run.
	// Default: ""
	// +optional
	StaticPodURL string `json:"staticPodURL,omitzero"`
	// staticPodURLHeader is a map of slices with HTTP headers to use when accessing the podURL.
	// Default: nil
	// +optional
	StaticPodURLHeader map[string][]string `json:"staticPodURLHeader,omitempty,omitzero"`
	// address is the IP address for the Kubelet to serve on (set to 0.0.0.0
	// for all interfaces).
	// Default: "0.0.0.0"
	// +optional
	Address string `json:"address,omitzero"`
	// port is the port for the Kubelet to serve on.
	// The port number must be between 1 and 65535, inclusive.
	// Default: 10250
	// +optional
	Port int32 `json:"port,omitzero"`
	// readOnlyPort is the read-only port for the Kubelet to serve on with
	// no authentication/authorization.
	// The port number must be between 1 and 65535, inclusive.
	// Setting this field to 0 disables the read-only service.
	// Default: 0 (disabled)
	// +optional
	ReadOnlyPort int32 `json:"readOnlyPort,omitzero"`
	// tlsCertFile is the file containing x509 Certificate for HTTPS. (CA cert,
	// if any, concatenated after server cert). If tlsCertFile and
	// tlsPrivateKeyFile are not provided, a self-signed certificate
	// and key are generated for the public address and saved to the directory
	// passed to the Kubelet's --cert-dir flag.
	// Default: ""
	// +optional
	TLSCertFile string `json:"tlsCertFile,omitzero"`
	// tlsPrivateKeyFile is the file containing x509 private key matching tlsCertFile.
	// Default: ""
	// +optional
	TLSPrivateKeyFile string `json:"tlsPrivateKeyFile,omitzero"`
	// tlsCipherSuites is the list of allowed cipher suites for the server.
	// Note that TLS 1.3 ciphersuites are not configurable.
	// Values are from tls package constants (https://golang.org/pkg/crypto/tls/#pkg-constants).
	// Default: nil
	// +optional
	TLSCipherSuites []string `json:"tlsCipherSuites,omitempty,omitzero"`
	// tlsMinVersion is the minimum TLS version supported.
	// Values are from tls package constants (https://golang.org/pkg/crypto/tls/#pkg-constants).
	// Default: ""
	// +optional
	TLSMinVersion string `json:"tlsMinVersion,omitzero"`
	// rotateCertificates enables client certificate rotation. The Kubelet will request a
	// new certificate from the certificates.k8s.io API. This requires an approver to approve the
	// certificate signing requests.
	// Default: false
	// +optional
	RotateCertificates bool `json:"rotateCertificates,omitzero"`
	// serverTLSBootstrap enables server certificate bootstrap. Instead of self
	// signing a serving certificate, the Kubelet will request a certificate from
	// the 'certificates.k8s.io' API. This requires an approver to approve the
	// certificate signing requests (CSR). The RotateKubeletServerCertificate feature
	// must be enabled when setting this field.
	// Default: false
	// +optional
	ServerTLSBootstrap bool `json:"serverTLSBootstrap,omitzero"`
	// authentication specifies how requests to the Kubelet's server are authenticated.
	// Defaults:
	//   anonymous:
	//     enabled: false
	//   webhook:
	//     enabled: true
	//     cacheTTL: "2m"
	// +optional
	Authentication KubeletAuthentication `json:"authentication,omitzero"`
	// authorization specifies how requests to the Kubelet's server are authorized.
	// Defaults:
	//   mode: Webhook
	//   webhook:
	//     cacheAuthorizedTTL: "5m"
	//     cacheUnauthorizedTTL: "30s"
	// +optional
	Authorization KubeletAuthorization `json:"authorization,omitzero"`
	// registryPullQPS is the limit of registry pulls per second.
	// The value must not be a negative number.
	// Setting it to 0 means no limit.
	// Default: 5
	// +optional
	RegistryPullQPS *int32 `json:"registryPullQPS,omitzero"`
	// registryBurst is the maximum size of bursty pulls, temporarily allows
	// pulls to burst to this number, while still not exceeding registryPullQPS.
	// The value must not be a negative number.
	// Only used if registryPullQPS is greater than 0.
	// Default: 10
	// +optional
	RegistryBurst int32 `json:"registryBurst,omitzero"`
	// imagePullCredentialsVerificationPolicy determines how credentials should be
	// verified when pod requests an image that is already present on the node:
	//   - NeverVerify
	//       - anyone on a node can use any image present on the node
	//   - NeverVerifyPreloadedImages
	//       - images that were pulled to the node by something else than the kubelet
	//         can be used without reverifying pull credentials
	//   - NeverVerifyAllowlistedImages
	//       - like "NeverVerifyPreloadedImages" but only node images from
	//         `preloadedImagesVerificationAllowlist` don't require reverification
	//   - AlwaysVerify
	//       - all images require credential reverification
	// +optional
	ImagePullCredentialsVerificationPolicy ImagePullCredentialsVerificationPolicy `json:"imagePullCredentialsVerificationPolicy,omitzero"`
	// preloadedImagesVerificationAllowlist specifies a list of images that are
	// exempted from credential reverification for the "NeverVerifyAllowlistedImages"
	// `imagePullCredentialsVerificationPolicy`.
	// The list accepts a full path segment wildcard suffix "/*".
	// Only use image specs without an image tag or digest.
	// +optional
	// +listType=set
	PreloadedImagesVerificationAllowlist []string `json:"preloadedImagesVerificationAllowlist,omitempty,omitzero"`
	// eventRecordQPS is the maximum event creations per second. If 0, there
	// is no limit enforced. The value cannot be a negative number.
	// Default: 50
	// +optional
	EventRecordQPS *int32 `json:"eventRecordQPS,omitzero"`
	// eventBurst is the maximum size of a burst of event creations, temporarily
	// allows event creations to burst to this number, while still not exceeding
	// eventRecordQPS. This field canot be a negative number and it is only used
	// when eventRecordQPS > 0.
	// Default: 100
	// +optional
	EventBurst int32 `json:"eventBurst,omitzero"`
	// enableDebuggingHandlers enables server endpoints for log access
	// and local running of containers and commands, including the exec,
	// attach, logs, and portforward features.
	// Default: true
	// +optional
	EnableDebuggingHandlers *bool `json:"enableDebuggingHandlers,omitzero"`
	// enableContentionProfiling enables block profiling, if enableDebuggingHandlers is true.
	// Default: false
	// +optional
	EnableContentionProfiling bool `json:"enableContentionProfiling,omitzero"`
	// healthzPort is the port of the localhost healthz endpoint (set to 0 to disable).
	// A valid number is between 1 and 65535.
	// Default: 10248
	// +optional
	HealthzPort *int32 `json:"healthzPort,omitzero"`
	// healthzBindAddress is the IP address for the healthz server to serve on.
	// Default: "127.0.0.1"
	// +optional
	HealthzBindAddress string `json:"healthzBindAddress,omitzero"`
	// oomScoreAdj is The oom-score-adj value for kubelet process. Values
	// must be within the range [-1000, 1000].
	// Default: -999
	// +optional
	OOMScoreAdj *int32 `json:"oomScoreAdj,omitzero"`
	// clusterDomain is the DNS domain for this cluster. If set, kubelet will
	// configure all containers to search this domain in addition to the
	// host's search domains.
	// Default: ""
	// +optional
	ClusterDomain string `json:"clusterDomain,omitzero"`
	// clusterDNS is a list of IP addresses for the cluster DNS server. If set,
	// kubelet will configure all containers to use this for DNS resolution
	// instead of the host's DNS servers.
	// Default: nil
	// +optional
	ClusterDNS []string `json:"clusterDNS,omitempty,omitzero"`
	// streamingConnectionIdleTimeout is the maximum time a streaming connection
	// can be idle before the connection is automatically closed.
	//
	// Deprecated: no longer has any effect.
	// Default: "4h"
	// +optional
	StreamingConnectionIdleTimeout metav1.Duration `json:"streamingConnectionIdleTimeout,omitzero"`
	// nodeStatusUpdateFrequency is the frequency that kubelet computes node
	// status. If node lease feature is not enabled, it is also the frequency that
	// kubelet posts node status to master.
	// Note: When node lease feature is not enabled, be cautious when changing the
	// constant, it must work with nodeMonitorGracePeriod in nodecontroller.
	// Default: "10s"
	// +optional
	NodeStatusUpdateFrequency metav1.Duration `json:"nodeStatusUpdateFrequency,omitzero"`
	// nodeStatusReportFrequency is the frequency that kubelet posts node
	// status to master if node status does not change. Kubelet will ignore this
	// frequency and post node status immediately if any change is detected. It is
	// only used when node lease feature is enabled. nodeStatusReportFrequency's
	// default value is 5m. But if nodeStatusUpdateFrequency is set explicitly,
	// nodeStatusReportFrequency's default value will be set to
	// nodeStatusUpdateFrequency for backward compatibility.
	// Default: "5m"
	// +optional
	NodeStatusReportFrequency metav1.Duration `json:"nodeStatusReportFrequency,omitzero"`
	// nodeLeaseDurationSeconds is the duration the Kubelet will set on its corresponding Lease.
	// NodeLease provides an indicator of node health by having the Kubelet create and
	// periodically renew a lease, named after the node, in the kube-node-lease namespace.
	// If the lease expires, the node can be considered unhealthy.
	// The lease is currently renewed every 10s, per KEP-0009. In the future, the lease renewal
	// interval may be set based on the lease duration.
	// The field value must be greater than 0.
	// Default: 40
	// +optional
	NodeLeaseDurationSeconds int32 `json:"nodeLeaseDurationSeconds,omitzero"`
	// imageMinimumGCAge is the minimum age for an unused image before it is
	// garbage collected.
	// Default: "2m"
	// +optional
	ImageMinimumGCAge metav1.Duration `json:"imageMinimumGCAge,omitzero"`
	// imageMaximumGCAge is the maximum age an image can be unused before it is garbage collected.
	// The default of this field is "0s", which disables this field--meaning images won't be garbage
	// collected based on being unused for too long.
	// Default: "0s" (disabled)
	// +optional
	ImageMaximumGCAge metav1.Duration `json:"imageMaximumGCAge,omitzero"`
	// imageGCHighThresholdPercent is the percent of disk usage after which
	// image garbage collection is always run. The percent is calculated by
	// dividing this field value by 100, so this field must be between 0 and
	// 100, inclusive. When specified, the value must be greater than
	// imageGCLowThresholdPercent.
	// Default: 85
	// +optional
	ImageGCHighThresholdPercent *int32 `json:"imageGCHighThresholdPercent,omitzero"`
	// imageGCLowThresholdPercent is the percent of disk usage before which
	// image garbage collection is never run. Lowest disk usage to garbage
	// collect to. The percent is calculated by dividing this field value by 100,
	// so the field value must be between 0 and 100, inclusive. When specified, the
	// value must be less than imageGCHighThresholdPercent.
	// Default: 80
	// +optional
	ImageGCLowThresholdPercent *int32 `json:"imageGCLowThresholdPercent,omitzero"`
	// volumeStatsAggPeriod is the frequency for calculating and caching volume
	// disk usage for all pods.
	// Default: "1m"
	// +optional
	VolumeStatsAggPeriod metav1.Duration `json:"volumeStatsAggPeriod,omitzero"`
	// kubeletCgroups is the absolute name of cgroups to isolate the kubelet in
	// Default: ""
	// +optional
	KubeletCgroups string `json:"kubeletCgroups,omitzero"`
	// systemCgroups is absolute name of cgroups in which to place
	// all non-kernel processes that are not already in a container. Empty
	// for no container. Rolling back the flag requires a reboot.
	// The cgroupRoot must be specified if this field is not empty.
	// Default: ""
	// +optional
	SystemCgroups string `json:"systemCgroups,omitzero"`
	// cgroupRoot is the root cgroup to use for pods. This is handled by the
	// container runtime on a best effort basis.
	// +optional
	CgroupRoot string `json:"cgroupRoot,omitzero"`
	// cgroupsPerQOS enable QoS based CGroup hierarchy: top level CGroups for QoS classes
	// and all Burstable and BestEffort Pods are brought up under their specific top level
	// QoS CGroup.
	// Default: true
	// +optional
	CgroupsPerQOS *bool `json:"cgroupsPerQOS,omitzero"`
	// cgroupDriver is the driver kubelet uses to manipulate CGroups on the host (cgroupfs
	// or systemd).
	// Default: "cgroupfs"
	// +optional
	CgroupDriver string `json:"cgroupDriver,omitzero"`
	// cpuManagerPolicy is the name of the policy to use.
	// Default: "None"
	// +optional
	CPUManagerPolicy string `json:"cpuManagerPolicy,omitzero"`
	// singleProcessOOMKill, if true, will prevent the `memory.oom.group` flag from being set for container
	// cgroups in cgroups v2. This causes processes in the container to be OOM killed individually instead of as
	// a group. It means that if true, the behavior aligns with the behavior of cgroups v1.
	// The default value is determined automatically when you don't specify.
	// On non-linux such as windows, only null / absent is allowed.
	// On cgroup v1 linux, only null / absent and true are allowed.
	// On cgroup v2 linux, null / absent, true and false are allowed. The default value is false.
	// +optional
	SingleProcessOOMKill *bool `json:"singleProcessOOMKill,omitzero"`
	// cpuManagerPolicyOptions is a set of key=value which 	allows to set extra options
	// to fine tune the behavior of the cpu manager policies.
	// Default: nil
	// +optional
	CPUManagerPolicyOptions map[string]string `json:"cpuManagerPolicyOptions,omitempty,omitzero"`
	// cpuManagerReconcilePeriod is the reconciliation period for the CPU Manager.
	// Default: "10s"
	// +optional
	CPUManagerReconcilePeriod metav1.Duration `json:"cpuManagerReconcilePeriod,omitzero"`
	// memoryManagerPolicy is the name of the policy to use by memory manager.
	// Requires the MemoryManager feature gate to be enabled.
	// Default: "none"
	// +optional
	MemoryManagerPolicy string `json:"memoryManagerPolicy,omitzero"`
	// topologyManagerPolicy is the name of the topology manager policy to use.
	// Valid values include:
	//
	// - `restricted`: kubelet only allows pods with optimal NUMA node alignment for
	//   requested resources;
	// - `best-effort`: kubelet will favor pods with NUMA alignment of CPU and device
	//   resources;
	// - `none`: kubelet has no knowledge of NUMA alignment of a pod's CPU and device resources.
	// - `single-numa-node`: kubelet only allows pods with a single NUMA alignment
	//   of CPU and device resources.
	//
	// Default: "none"
	// +optional
	TopologyManagerPolicy string `json:"topologyManagerPolicy,omitzero"`
	// topologyManagerScope represents the scope of topology hint generation
	// that topology manager requests and hint providers generate. Valid values include:
	//
	// - `container`: topology policy is applied on a per-container basis.
	// - `pod`: topology policy is applied on a per-pod basis.
	//
	// Default: "container"
	// +optional
	TopologyManagerScope string `json:"topologyManagerScope,omitzero"`
	// TopologyManagerPolicyOptions is a set of key=value which allows to set extra options
	// to fine tune the behavior of the topology manager policies.
	// Requires  both the "TopologyManager" and "TopologyManagerPolicyOptions" feature gates to be enabled.
	// Default: nil
	// +optional
	TopologyManagerPolicyOptions map[string]string `json:"topologyManagerPolicyOptions,omitempty,omitzero"`
	// qosReserved is a set of resource name to percentage pairs that specify
	// the minimum percentage of a resource reserved for exclusive use by the
	// guaranteed QoS tier.
	// Currently supported resources: "memory"
	// Requires the QOSReserved feature gate to be enabled.
	// Default: nil
	// +optional
	QOSReserved map[string]string `json:"qosReserved,omitempty,omitzero"`
	// runtimeRequestTimeout is the timeout for all runtime requests except long running
	// requests - pull, logs, exec and attach.
	// Default: "2m"
	// +optional
	RuntimeRequestTimeout metav1.Duration `json:"runtimeRequestTimeout,omitzero"`
	// hairpinMode specifies how the Kubelet should configure the container
	// bridge for hairpin packets.
	// Setting this flag allows endpoints in a Service to loadbalance back to
	// themselves if they should try to access their own Service. Values:
	//
	// - "promiscuous-bridge": make the container bridge promiscuous.
	// - "hairpin-veth":       set the hairpin flag on container veth interfaces.
	// - "none":               do nothing.
	//
	// Generally, one must set `--hairpin-mode=hairpin-veth to` achieve hairpin NAT,
	// because promiscuous-bridge assumes the existence of a container bridge named cbr0.
	// Default: "promiscuous-bridge"
	// +optional
	HairpinMode string `json:"hairpinMode,omitzero"`
	// maxPods is the maximum number of Pods that can run on this Kubelet.
	// The value must be a non-negative integer.
	// Default: 110
	// +optional
	MaxPods int32 `json:"maxPods,omitzero"`
	// podCIDR is the CIDR to use for pod IP addresses, only used in standalone mode.
	// In cluster mode, this is obtained from the control plane.
	// Default: ""
	// +optional
	PodCIDR string `json:"podCIDR,omitzero"`
	// podPidsLimit is the maximum number of PIDs in any pod.
	// Default: -1
	// +optional
	PodPidsLimit *int64 `json:"podPidsLimit,omitzero"`
	// resolvConf is the resolver configuration file used as the basis
	// for the container DNS resolution configuration.
	// If set to the empty string, will override the default and effectively disable DNS lookups.
	// Default: "/etc/resolv.conf"
	// +optional
	ResolverConfig *string `json:"resolvConf,omitzero"`
	// runOnce causes the Kubelet to check the API server once for pods,
	// run those in addition to the pods specified by static pod files, and exit.
	// Default: false
	// +optional
	RunOnce bool `json:"runOnce,omitzero"`
	// cpuCFSQuota enables CPU CFS quota enforcement for containers that
	// specify CPU limits.
	// Default: true
	// +optional
	CPUCFSQuota *bool `json:"cpuCFSQuota,omitzero"`
	// cpuCFSQuotaPeriod is the CPU CFS quota period value, `cpu.cfs_period_us`.
	// The value must be between 1 ms and 1 second, inclusive.
	// Requires the CustomCPUCFSQuotaPeriod feature gate to be enabled.
	// Default: "100ms"
	// +optional
	CPUCFSQuotaPeriod *metav1.Duration `json:"cpuCFSQuotaPeriod,omitzero"`
	// nodeStatusMaxImages caps the number of images reported in Node.status.images.
	// The value must be greater than -2.
	// Note: If -1 is specified, no cap will be applied. If 0 is specified, no image is returned.
	// Default: 50
	// +optional
	NodeStatusMaxImages *int32 `json:"nodeStatusMaxImages,omitzero"`
	// maxOpenFiles is Number of files that can be opened by Kubelet process.
	// The value must be a non-negative number.
	// Default: 1000000
	// +optional
	MaxOpenFiles int64 `json:"maxOpenFiles,omitzero"`
	// contentType is contentType of requests sent to apiserver.
	// Default: "application/vnd.kubernetes.protobuf"
	// +optional
	ContentType string `json:"contentType,omitzero"`
	// kubeAPIQPS is the QPS to use while talking with kubernetes apiserver.
	// Default: 50
	// +optional
	KubeAPIQPS *int32 `json:"kubeAPIQPS,omitzero"`
	// kubeAPIBurst is the burst to allow while talking with kubernetes API server.
	// This field cannot be a negative number.
	// Default: 100
	// +optional
	KubeAPIBurst int32 `json:"kubeAPIBurst,omitzero"`
	// serializeImagePulls when enabled, tells the Kubelet to pull images one
	// at a time. We recommend *not* changing the default value on nodes that
	// run docker daemon with version  < 1.9 or an Aufs storage backend.
	// Issue #10959 has more details.
	// Default: true
	// +optional
	SerializeImagePulls *bool `json:"serializeImagePulls,omitzero"`
	// MaxParallelImagePulls sets the maximum number of image pulls in parallel.
	// This field cannot be set if SerializeImagePulls is true.
	// Setting it to nil means no limit.
	// Default: nil
	// +optional
	MaxParallelImagePulls *int32 `json:"maxParallelImagePulls,omitzero"`
	// evictionHard is a map of signal names to quantities that defines hard eviction
	// thresholds. For example: `{"memory.available": "300Mi"}`.
	// To explicitly disable, pass a 0% or 100% threshold on an arbitrary resource.
	// Default:
	//   memory.available:  "100Mi"
	//   nodefs.available:  "10%"
	//   nodefs.inodesFree: "5%"
	//   imagefs.available: "15%"
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty,omitzero"`
	// evictionSoft is a map of signal names to quantities that defines soft eviction thresholds.
	// For example: `{"memory.available": "300Mi"}`.
	// Default: nil
	// +optional
	EvictionSoft map[string]string `json:"evictionSoft,omitempty,omitzero"`
	// evictionSoftGracePeriod is a map of signal names to quantities that defines grace
	// periods for each soft eviction signal. For example: `{"memory.available": "30s"}`.
	// Default: nil
	// +optional
	EvictionSoftGracePeriod map[string]string `json:"evictionSoftGracePeriod,omitempty,omitzero"`
	// evictionPressureTransitionPeriod is the duration for which the kubelet has to wait
	// before transitioning out of an eviction pressure condition.
	// A duration of 0s will be converted to the default value of 5m
	// Default: "5m"
	// +optional
	EvictionPressureTransitionPeriod metav1.Duration `json:"evictionPressureTransitionPeriod,omitzero"`
	// evictionMaxPodGracePeriod is the maximum allowed grace period (in seconds) to use
	// when terminating pods in response to a soft eviction threshold being met. This value
	// effectively caps the Pod's terminationGracePeriodSeconds value during soft evictions.
	// Default: 0
	// +optional
	EvictionMaxPodGracePeriod int32 `json:"evictionMaxPodGracePeriod,omitzero"`
	// evictionMinimumReclaim is a map of signal names to quantities that defines minimum reclaims,
	// which describe the minimum amount of a given resource the kubelet will reclaim when
	// performing a pod eviction while that resource is under pressure.
	// For example: `{"imagefs.available": "2Gi"}`.
	// Default: nil
	// +optional
	EvictionMinimumReclaim map[string]string `json:"evictionMinimumReclaim,omitempty,omitzero"`
	// mergeDefaultEvictionSettings indicates that defaults for the evictionHard, evictionSoft, evictionSoftGracePeriod, and evictionMinimumReclaim
	// fields should be merged into values specified for those fields in this configuration.
	// Signals specified in this configuration take precedence.
	// Signals not specified in this configuration inherit their defaults.
	// If false, and if any signal is specified in this configuration then other signals that
	// are not specified in this configuration will be set to 0.
	// It applies to merging the fields for which the default exists, and currently only evictionHard has default values.
	// Default: false
	// +optional
	MergeDefaultEvictionSettings *bool `json:"mergeDefaultEvictionSettings,omitzero"`
	// podsPerCore is the maximum number of pods per core. Cannot exceed maxPods.
	// The value must be a non-negative integer.
	// If 0, there is no limit on the number of Pods.
	// Default: 0
	// +optional
	PodsPerCore int32 `json:"podsPerCore,omitzero"`
	// enableControllerAttachDetach enables the Attach/Detach controller to
	// manage attachment/detachment of volumes scheduled to this node, and
	// disables kubelet from executing any attach/detach operations.
	// Note: attaching/detaching CSI volumes is not supported by the kubelet,
	// so this option needs to be true for that use case.
	// Default: true
	// +optional
	EnableControllerAttachDetach *bool `json:"enableControllerAttachDetach,omitzero"`
	// protectKernelDefaults, if true, causes the Kubelet to error if kernel
	// flags are not as it expects. Otherwise the Kubelet will attempt to modify
	// kernel flags to match its expectation.
	// Default: false
	// +optional
	ProtectKernelDefaults bool `json:"protectKernelDefaults,omitzero"`
	// makeIPTablesUtilChains, if true, causes the Kubelet to create the
	// KUBE-IPTABLES-HINT chain in iptables as a hint to other components about the
	// configuration of iptables on the system.
	// Default: true
	// +optional
	MakeIPTablesUtilChains *bool `json:"makeIPTablesUtilChains,omitzero"`
	// iptablesMasqueradeBit formerly controlled the creation of the KUBE-MARK-MASQ
	// chain.
	//
	// Deprecated: no longer has any effect.
	// Default: 14
	// +optional
	IPTablesMasqueradeBit *int32 `json:"iptablesMasqueradeBit,omitzero"`
	// iptablesDropBit formerly controlled the creation of the KUBE-MARK-DROP chain.
	//
	// Deprecated: no longer has any effect.
	// Default: 15
	// +optional
	IPTablesDropBit *int32 `json:"iptablesDropBit,omitzero"`
	// featureGates is a map of feature names to bools that enable or disable experimental
	// features. This field modifies piecemeal the built-in default values from
	// "k8s.io/kubernetes/pkg/features/kube_features.go".
	// Default: nil
	// +optional
	FeatureGates map[string]bool `json:"featureGates,omitempty,omitzero"`
	// failSwapOn tells the Kubelet to fail to start if swap is enabled on the node.
	// Default: true
	// +optional
	FailSwapOn *bool `json:"failSwapOn,omitzero"`
	// memorySwap configures swap memory available to container workloads.
	// +featureGate=NodeSwap
	// +optional
	MemorySwap MemorySwapConfiguration `json:"memorySwap,omitzero"`
	// containerLogMaxSize is a quantity defining the maximum size of the container log
	// file before it is rotated. For example: "5Mi" or "256Ki".
	// Default: "10Mi"
	// +optional
	ContainerLogMaxSize string `json:"containerLogMaxSize,omitzero"`
	// containerLogMaxFiles specifies the maximum number of container log files that can
	// be present for a container.
	// Default: 5
	// +optional
	ContainerLogMaxFiles *int32 `json:"containerLogMaxFiles,omitzero"`

	// ContainerLogMaxWorkers specifies the maximum number of concurrent workers to spawn
	// for performing the log rotate operations. Set this count to 1 for disabling the
	// concurrent log rotation workflows
	// Default: 1
	// +optional
	ContainerLogMaxWorkers *int32 `json:"containerLogMaxWorkers,omitzero"`

	// ContainerLogMonitorInterval specifies the duration at which the container logs are monitored
	// for performing the log rotate operation. This defaults to 10 * time.Seconds. But can be
	// customized to a smaller value based on the log generation rate and the size required to be
	// rotated against
	// Default: 10s
	// +optional
	ContainerLogMonitorInterval *metav1.Duration `json:"containerLogMonitorInterval,omitzero"`
	// configMapAndSecretChangeDetectionStrategy is a mode in which ConfigMap and Secret
	// managers are running. Valid values include:
	//
	// - `Get`: kubelet fetches necessary objects directly from the API server;
	// - `Cache`: kubelet uses TTL cache for object fetched from the API server;
	// - `Watch`: kubelet uses watches to observe changes to objects that are in its interest.
	//
	// Default: "Watch"
	// +optional
	ConfigMapAndSecretChangeDetectionStrategy ResourceChangeDetectionStrategy `json:"configMapAndSecretChangeDetectionStrategy,omitzero"`

	/* the following fields are meant for Node Allocatable */

	// systemReserved is a set of ResourceName=ResourceQuantity (e.g. cpu=200m,memory=150G)
	// pairs that describe resources reserved for non-kubernetes components.
	// Currently only cpu and memory are supported.
	// See https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources for more detail.
	// Default: nil
	// +optional
	SystemReserved map[string]string `json:"systemReserved,omitempty,omitzero"`
	// kubeReserved is a set of ResourceName=ResourceQuantity (e.g. cpu=200m,memory=150G) pairs
	// that describe resources reserved for kubernetes system components.
	// Currently cpu, memory and local storage for root file system are supported.
	// See https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources
	// for more details.
	// Default: nil
	// +optional
	KubeReserved map[string]string `json:"kubeReserved,omitempty,omitzero"`
	// The reservedSystemCPUs option specifies the CPU list reserved for the host
	// level system threads and kubernetes related threads. This provide a "static"
	// CPU list rather than the "dynamic" list by systemReserved and kubeReserved.
	// This option does not support systemReservedCgroup or kubeReservedCgroup.
	ReservedSystemCPUs string `json:"reservedSystemCPUs,omitzero"`
	// showHiddenMetricsForVersion is the previous version for which you want to show
	// hidden metrics.
	// Only the previous minor version is meaningful, other values will not be allowed.
	// The format is `<major>.<minor>`, e.g.: `1.16`.
	// The purpose of this format is make sure you have the opportunity to notice
	// if the next release hides additional metrics, rather than being surprised
	// when they are permanently removed in the release after that.
	// Default: ""
	// +optional
	ShowHiddenMetricsForVersion string `json:"showHiddenMetricsForVersion,omitzero"`
	// systemReservedCgroup helps the kubelet identify absolute name of top level CGroup used
	// to enforce `systemReserved` compute resource reservation for OS system daemons.
	// Refer to [Node Allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable)
	// doc for more information.
	// Default: ""
	// +optional
	SystemReservedCgroup string `json:"systemReservedCgroup,omitzero"`
	// kubeReservedCgroup helps the kubelet identify absolute name of top level CGroup used
	// to enforce `KubeReserved` compute resource reservation for Kubernetes node system daemons.
	// Refer to [Node Allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable)
	// doc for more information.
	// Default: ""
	// +optional
	KubeReservedCgroup string `json:"kubeReservedCgroup,omitzero"`
	// This flag specifies the various Node Allocatable enforcements that Kubelet needs to perform.
	// This flag accepts a list of options. Acceptable options are `none`, `pods`,
	// `system-reserved` and `kube-reserved`.
	// If `none` is specified, no other options may be specified.
	// When `system-reserved` is in the list, systemReservedCgroup must be specified.
	// When `kube-reserved` is in the list, kubeReservedCgroup must be specified.
	// This field is supported only when `cgroupsPerQOS` is set to true.
	// Refer to [Node Allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable)
	// for more information.
	// Default: ["pods"]
	// +optional
	EnforceNodeAllocatable []string `json:"enforceNodeAllocatable,omitempty,omitzero"`
	// A comma separated whitelist of unsafe sysctls or sysctl patterns (ending in `*`).
	// Unsafe sysctl groups are `kernel.shm*`, `kernel.msg*`, `kernel.sem`, `fs.mqueue.*`,
	// and `net.*`. For example: "`kernel.msg*,net.ipv4.route.min_pmtu`"
	// Default: []
	// +optional
	AllowedUnsafeSysctls []string `json:"allowedUnsafeSysctls,omitempty,omitzero"`
	// volumePluginDir is the full path of the directory in which to search
	// for additional third party volume plugins.
	// Default: "/usr/libexec/kubernetes/kubelet-plugins/volume/exec/"
	// +optional
	VolumePluginDir string `json:"volumePluginDir,omitzero"`
	// providerID, if set, sets the unique ID of the instance that an external
	// provider (i.e. cloudprovider) can use to identify a specific node.
	// Default: ""
	// +optional
	ProviderID string `json:"providerID,omitzero"`
	// kernelMemcgNotification, if set, instructs the kubelet to integrate with the
	// kernel memcg notification for determining if memory eviction thresholds are
	// exceeded rather than polling.
	// Default: false
	// +optional
	KernelMemcgNotification bool `json:"kernelMemcgNotification,omitzero"`
	// logging specifies the options of logging.
	// Refer to [Logs Options](https://github.com/kubernetes/component-base/blob/master/logs/options.go)
	// for more information.
	// Default:
	//   Format: text
	// + optional
	Logging logsapi.LoggingConfiguration `json:"logging,omitzero"`
	// enableSystemLogHandler enables system logs via web interface host:port/logs/
	// Default: true
	// +optional
	EnableSystemLogHandler *bool `json:"enableSystemLogHandler,omitzero"`
	// enableSystemLogQuery enables the node log query feature on the /logs endpoint.
	// EnableSystemLogHandler has to be enabled in addition for this feature to work.
	// Enabling this feature has security implications. The recommendation is to enable it on a need basis for debugging
	// purposes and disabling otherwise.
	// Default: false
	// +featureGate=NodeLogQuery
	// +optional
	EnableSystemLogQuery *bool `json:"enableSystemLogQuery,omitzero"`
	// shutdownGracePeriod specifies the total duration that the node should delay the
	// shutdown and total grace period for pod termination during a node shutdown.
	// Default: "0s"
	// +featureGate=GracefulNodeShutdown
	// +optional
	ShutdownGracePeriod metav1.Duration `json:"shutdownGracePeriod,omitzero"`
	// shutdownGracePeriodCriticalPods specifies the duration used to terminate critical
	// pods during a node shutdown. This should be less than shutdownGracePeriod.
	// For example, if shutdownGracePeriod=30s, and shutdownGracePeriodCriticalPods=10s,
	// during a node shutdown the first 20 seconds would be reserved for gracefully
	// terminating normal pods, and the last 10 seconds would be reserved for terminating
	// critical pods.
	// Default: "0s"
	// +featureGate=GracefulNodeShutdown
	// +optional
	ShutdownGracePeriodCriticalPods metav1.Duration `json:"shutdownGracePeriodCriticalPods,omitzero"`
	// shutdownGracePeriodByPodPriority specifies the shutdown grace period for Pods based
	// on their associated priority class value.
	// When a shutdown request is received, the Kubelet will initiate shutdown on all pods
	// running on the node with a grace period that depends on the priority of the pod,
	// and then wait for all pods to exit.
	// Each entry in the array represents the graceful shutdown time a pod with a priority
	// class value that lies in the range of that value and the next higher entry in the
	// list when the node is shutting down.
	// For example, to allow critical pods 10s to shutdown, priority>=10000 pods 20s to
	// shutdown, and all remaining pods 30s to shutdown.
	//
	// shutdownGracePeriodByPodPriority:
	//   - priority: 2000000000
	//     shutdownGracePeriodSeconds: 10
	//   - priority: 10000
	//     shutdownGracePeriodSeconds: 20
	//   - priority: 0
	//     shutdownGracePeriodSeconds: 30
	//
	// The time the Kubelet will wait before exiting will at most be the maximum of all
	// shutdownGracePeriodSeconds for each priority class range represented on the node.
	// When all pods have exited or reached their grace periods, the Kubelet will release
	// the shutdown inhibit lock.
	// Requires the GracefulNodeShutdown feature gate to be enabled.
	// This configuration must be empty if either ShutdownGracePeriod or ShutdownGracePeriodCriticalPods is set.
	// Default: nil
	// +featureGate=GracefulNodeShutdownBasedOnPodPriority
	// +optional
	ShutdownGracePeriodByPodPriority []ShutdownGracePeriodByPodPriority `json:"shutdownGracePeriodByPodPriority,omitempty,omitzero"`
	// CrashLoopBackOff contains config to modify node-level parameters for
	// container restart behavior
	// +featureGate=KubeletCrashLoopBackOffMax
	// +optional
	CrashLoopBackOff CrashLoopBackOffConfig `json:"crashLoopBackOff,omitzero"`
	// reservedMemory specifies a comma-separated list of memory reservations for NUMA nodes.
	// The parameter makes sense only in the context of the memory manager feature.
	// The memory manager will not allocate reserved memory for container workloads.
	// For example, if you have a NUMA0 with 10Gi of memory and the reservedMemory was
	// specified to reserve 1Gi of memory at NUMA0, the memory manager will assume that
	// only 9Gi is available for allocation.
	// You can specify a different amount of NUMA node and memory types.
	// You can omit this parameter at all, but you should be aware that the amount of
	// reserved memory from all NUMA nodes should be equal to the amount of memory specified
	// by the [node allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable).
	// If at least one node allocatable parameter has a non-zero value, you will need
	// to specify at least one NUMA node.
	// Also, avoid specifying:
	//
	// 1. Duplicates, the same NUMA node, and memory type, but with a different value.
	// 2. zero limits for any memory type.
	// 3. NUMAs nodes IDs that do not exist under the machine.
	// 4. memory types except for memory and hugepages-<size>
	//
	// Default: nil
	// +optional
	ReservedMemory []MemoryReservation `json:"reservedMemory,omitempty,omitzero"`
	// enableProfilingHandler enables profiling via web interface host:port/debug/pprof/
	// Default: true
	// +optional
	EnableProfilingHandler *bool `json:"enableProfilingHandler,omitzero"`
	// enableDebugFlagsHandler enables flags endpoint via web interface host:port/debug/flags/v
	// Default: true
	// +optional
	EnableDebugFlagsHandler *bool `json:"enableDebugFlagsHandler,omitzero"`
	// SeccompDefault enables the use of `RuntimeDefault` as the default seccomp profile for all workloads.
	// Default: false
	// +optional
	SeccompDefault *bool `json:"seccompDefault,omitzero"`
	// MemoryThrottlingFactor specifies the factor multiplied by the memory limit or node allocatable memory
	// when setting the cgroupv2 memory.high value to enforce MemoryQoS.
	// Decreasing this factor will set lower high limit for container cgroups and put heavier reclaim pressure
	// while increasing will put less reclaim pressure.
	// See https://kep.k8s.io/2570 for more details.
	// Default: 0.9
	// +featureGate=MemoryQoS
	// +optional
	MemoryThrottlingFactor *float64 `json:"memoryThrottlingFactor,omitzero"`
	// registerWithTaints are an array of taints to add to a node object when
	// the kubelet registers itself. This only takes effect when registerNode
	// is true and upon the initial registration of the node.
	// Default: nil
	// +optional
	RegisterWithTaints []v1.Taint `json:"registerWithTaints,omitempty,omitzero"`
	// registerNode enables automatic registration with the apiserver.
	// Default: true
	// +optional
	RegisterNode *bool `json:"registerNode,omitzero"`
	// Tracing specifies the versioned configuration for OpenTelemetry tracing clients.
	// See https://kep.k8s.io/2832 for more details.
	// Default: nil
	// +optional
	Tracing *tracingapi.TracingConfiguration `json:"tracing,omitzero"`

	// LocalStorageCapacityIsolation enables local ephemeral storage isolation feature. The default setting is true.
	// This feature allows users to set request/limit for container's ephemeral storage and manage it in a similar way
	// as cpu and memory. It also allows setting sizeLimit for emptyDir volume, which will trigger pod eviction if disk
	// usage from the volume exceeds the limit.
	// This feature depends on the capability of detecting correct root file system disk usage. For certain systems,
	// such as kind rootless, if this capability cannot be supported, the feature LocalStorageCapacityIsolation should be
	// disabled. Once disabled, user should not set request/limit for container's ephemeral storage, or sizeLimit for emptyDir.
	// Default: true
	// +optional
	LocalStorageCapacityIsolation *bool `json:"localStorageCapacityIsolation,omitzero"`

	// ContainerRuntimeEndpoint is the endpoint of container runtime.
	// Unix Domain Sockets are supported on Linux, while npipe and tcp endpoints are supported on Windows.
	// Examples:'unix:///path/to/runtime.sock', 'npipe:////./pipe/runtime'
	ContainerRuntimeEndpoint string `json:"containerRuntimeEndpoint,omitzero"`

	// ImageServiceEndpoint is the endpoint of container image service.
	// Unix Domain Socket are supported on Linux, while npipe and tcp endpoints are supported on Windows.
	// Examples:'unix:///path/to/runtime.sock', 'npipe:////./pipe/runtime'.
	// If not specified, the value in containerRuntimeEndpoint is used.
	// +optional
	ImageServiceEndpoint string `json:"imageServiceEndpoint,omitzero"`

	// FailCgroupV1 prevents the kubelet from starting on hosts
	// that use cgroup v1. By default, this is set to 'true', meaning
	// the kubelet will not start on cgroup v1 hosts unless this
	// option is explicitly disabled.
	// Default: true
	// +optional
	FailCgroupV1 *bool `json:"failCgroupV1,omitzero"`

	// UserNamespaces contains User Namespace configurations.
	// +featureGate=UserNamespacesSupport
	// +optional
	UserNamespaces *UserNamespaces `json:"userNamespaces,omitzero"`
}
