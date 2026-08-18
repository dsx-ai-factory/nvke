// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1beta3 contains local copies of the kubeadm v1beta3 config
// structs, tracking the upstream kubeadm file as closely as practical.
// Upstream external Kubernetes types are reused where practical; only the
// pieces not locally available as dependencies remain copied here.
//
// Source:
//
//	https://github.com/kubernetes/kubernetes/blob/master/cmd/kubeadm/app/apis/kubeadm/v1beta3/types.go
package v1beta3

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	bootstraptokenv1 "github.com/NVIDIA/nke/controller/internal/kubeadmconfig/thirdparty/bootstraptoken/v1"
)

const (
	APIVersion               = "kubeadm.k8s.io/v1beta3"
	InitConfigurationKind    = "InitConfiguration"
	ClusterConfigurationKind = "ClusterConfiguration"
	JoinConfigurationKind    = "JoinConfiguration"
)

// InitConfiguration contains a list of elements that is specific "kubeadm init"-only runtime information.
type InitConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	BootstrapTokens  []bootstraptokenv1.BootstrapToken `json:"bootstrapTokens,omitempty"`
	NodeRegistration NodeRegistrationOptions           `json:"nodeRegistration,omitempty"`
	LocalAPIEndpoint APIEndpoint                       `json:"localAPIEndpoint,omitempty"`
	CertificateKey   string                            `json:"certificateKey,omitempty"`
	SkipPhases       []string                          `json:"skipPhases,omitempty"`
	Patches          *Patches                          `json:"patches,omitempty"`
}

// ClusterConfiguration contains cluster-wide configuration for a kubeadm cluster
type ClusterConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	Etcd                 Etcd                  `json:"etcd,omitempty"`
	Networking           Networking            `json:"networking,omitempty"`
	KubernetesVersion    string                `json:"kubernetesVersion,omitempty"`
	ControlPlaneEndpoint string                `json:"controlPlaneEndpoint,omitempty"`
	APIServer            APIServer             `json:"apiServer,omitempty"`
	ControllerManager    ControlPlaneComponent `json:"controllerManager,omitempty"`
	Scheduler            ControlPlaneComponent `json:"scheduler,omitempty"`
	DNS                  DNS                   `json:"dns,omitempty"`
	CertificatesDir      string                `json:"certificatesDir,omitempty"`
	ImageRepository      string                `json:"imageRepository,omitempty"`
	FeatureGates         map[string]bool       `json:"featureGates,omitempty"`
	ClusterName          string                `json:"clusterName,omitempty"`
}

// ControlPlaneComponent holds settings common to control plane component of the cluster
type ControlPlaneComponent struct {
	ExtraArgs    map[string]string `json:"extraArgs,omitempty"`
	ExtraVolumes []HostPathMount   `json:"extraVolumes,omitempty"`
}

// APIServer holds settings necessary for API server deployments in the cluster
type APIServer struct {
	ControlPlaneComponent  `json:",inline"`
	CertSANs               []string         `json:"certSANs,omitempty"`
	TimeoutForControlPlane *metav1.Duration `json:"timeoutForControlPlane,omitempty"`
}

// DNSAddOnType defines string identifying DNS add-on types
type DNSAddOnType string

// DNS defines the DNS addon that should be used in the cluster
type DNS struct {
	ImageMeta `json:",inline"`
}

// ImageMeta allows to customize the image used for components that are not originated from the Kubernetes release process
type ImageMeta struct {
	ImageRepository string `json:"imageRepository,omitempty"`
	ImageTag        string `json:"imageTag,omitempty"`
}

// APIEndpoint struct contains elements of API server instance deployed on a node.
type APIEndpoint struct {
	AdvertiseAddress string `json:"advertiseAddress,omitempty"`
	BindPort         int32  `json:"bindPort,omitempty"`
}

// NodeRegistrationOptions holds fields that relate to registering a new control-plane or node to the cluster.
type NodeRegistrationOptions struct {
	Name                  string            `json:"name,omitempty"`
	CRISocket             string            `json:"criSocket,omitempty"`
	Taints                []corev1.Taint    `json:"taints"`
	KubeletExtraArgs      map[string]string `json:"kubeletExtraArgs,omitempty"`
	IgnorePreflightErrors []string          `json:"ignorePreflightErrors,omitempty"`
	ImagePullPolicy       corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// Networking contains elements describing cluster's networking configuration
type Networking struct {
	ServiceSubnet string `json:"serviceSubnet,omitempty"`
	PodSubnet     string `json:"podSubnet,omitempty"`
	DNSDomain     string `json:"dnsDomain,omitempty"`
}

// Etcd contains elements describing Etcd configuration.
type Etcd struct {
	Local    *LocalEtcd    `json:"local,omitempty"`
	External *ExternalEtcd `json:"external,omitempty"`
}

// LocalEtcd describes that kubeadm should run an etcd cluster locally
type LocalEtcd struct {
	ImageMeta      `json:",inline"`
	DataDir        string            `json:"dataDir"`
	ExtraArgs      map[string]string `json:"extraArgs,omitempty"`
	ServerCertSANs []string          `json:"serverCertSANs,omitempty"`
	PeerCertSANs   []string          `json:"peerCertSANs,omitempty"`
}

// ExternalEtcd describes an external etcd cluster.
type ExternalEtcd struct {
	Endpoints []string `json:"endpoints"`
	CAFile    string   `json:"caFile"`
	CertFile  string   `json:"certFile"`
	KeyFile   string   `json:"keyFile"`
}

// JoinConfiguration contains elements describing a particular node.
type JoinConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	NodeRegistration NodeRegistrationOptions `json:"nodeRegistration,omitempty"`
	CACertPath       string                  `json:"caCertPath,omitempty"`
	Discovery        Discovery               `json:"discovery"`
	ControlPlane     *JoinControlPlane       `json:"controlPlane,omitempty"`
	SkipPhases       []string                `json:"skipPhases,omitempty"`
	Patches          *Patches                `json:"patches,omitempty"`
}

// JoinControlPlane contains elements describing an additional control plane instance to be deployed on the joining node.
type JoinControlPlane struct {
	LocalAPIEndpoint APIEndpoint `json:"localAPIEndpoint,omitempty"`
	CertificateKey   string      `json:"certificateKey,omitempty"`
}

// Discovery specifies the options for the kubelet to use during the TLS Bootstrap process
type Discovery struct {
	BootstrapToken    *BootstrapTokenDiscovery `json:"bootstrapToken,omitempty"`
	File              *FileDiscovery           `json:"file,omitempty"`
	TLSBootstrapToken string                   `json:"tlsBootstrapToken,omitempty"`
	Timeout           *metav1.Duration         `json:"timeout,omitempty"`
}

// BootstrapTokenDiscovery is used to set the options for bootstrap token based discovery
type BootstrapTokenDiscovery struct {
	Token                    string   `json:"token"`
	APIServerEndpoint        string   `json:"apiServerEndpoint,omitempty"`
	CACertHashes             []string `json:"caCertHashes,omitempty"`
	UnsafeSkipCAVerification bool     `json:"unsafeSkipCAVerification,omitempty"`
}

// FileDiscovery is used to specify a file or URL to a kubeconfig file from which to load cluster information
type FileDiscovery struct {
	KubeConfigPath string `json:"kubeConfigPath"`
}

// HostPathMount contains elements describing volumes that are mounted from the host.
type HostPathMount struct {
	Name      string              `json:"name"`
	HostPath  string              `json:"hostPath"`
	MountPath string              `json:"mountPath"`
	ReadOnly  bool                `json:"readOnly,omitempty"`
	PathType  corev1.HostPathType `json:"pathType,omitempty"`
}

// Patches contains options related to applying patches to components deployed by kubeadm.
type Patches struct {
	Directory string `json:"directory,omitempty"`
}
