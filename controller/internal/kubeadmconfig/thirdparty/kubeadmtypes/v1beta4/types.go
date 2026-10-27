/*
Copyright 2023 The Kubernetes Authors.

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

// Package v1beta4 contains local copies of the kubeadm v1beta4 config structs,
// tracking the upstream kubeadm file as closely as practical. Brownfield
// kubeadm-config reconciliation reads and rewrites the persisted
// ClusterConfiguration, and a typed round-trip drops any field absent from these
// structs — so this copy must stay faithful to upstream. The greenfield path
// renders InitConfiguration/JoinConfiguration in v1beta4 for Kubernetes 1.31 and
// newer, the first release whose kubeadm reads it.
//
// Source (Kubernetes v1.34.0, commit f28b4c9efbca5c5c0af716d9f2d5702667ee8a45):
//
//	https://github.com/kubernetes/kubernetes/blob/v1.34.0/cmd/kubeadm/app/apis/kubeadm/v1beta4/types.go
//
// The revision is inferred; see THIRD_PARTY_NOTICES.md.
//
// Local changes:
//   - ResetConfiguration, UpgradeConfiguration and ExternalEtcd.HTTPEndpoints
//     (added upstream in v1.35) are not copied
//   - bootstraptoken/v1 is imported from the local copy
//   - APIVersion and the *Kind constants are added
//   - datapolicy struct tags and most field doc comments are omitted
package v1beta4

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	bootstraptokenv1 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/bootstraptoken/v1"
)

const (
	APIVersion               = "kubeadm.k8s.io/v1beta4"
	InitConfigurationKind    = "InitConfiguration"
	ClusterConfigurationKind = "ClusterConfiguration"
	JoinConfigurationKind    = "JoinConfiguration"
)

// InitConfiguration contains a list of elements that is specific "kubeadm init"-only runtime information.
type InitConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	BootstrapTokens  []bootstraptokenv1.BootstrapToken `json:"bootstrapTokens,omitempty"`
	DryRun           bool                              `json:"dryRun,omitempty"`
	NodeRegistration NodeRegistrationOptions           `json:"nodeRegistration,omitempty"`
	LocalAPIEndpoint APIEndpoint                       `json:"localAPIEndpoint,omitempty"`
	CertificateKey   string                            `json:"certificateKey,omitempty"`
	SkipPhases       []string                          `json:"skipPhases,omitempty"`
	Patches          *Patches                          `json:"patches,omitempty"`
	Timeouts         *Timeouts                         `json:"timeouts,omitempty"`
}

// ClusterConfiguration contains cluster-wide configuration for a kubeadm cluster.
type ClusterConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	Etcd                        Etcd                    `json:"etcd,omitempty"`
	Networking                  Networking              `json:"networking,omitempty"`
	KubernetesVersion           string                  `json:"kubernetesVersion,omitempty"`
	ControlPlaneEndpoint        string                  `json:"controlPlaneEndpoint,omitempty"`
	APIServer                   APIServer               `json:"apiServer,omitempty"`
	ControllerManager           ControlPlaneComponent   `json:"controllerManager,omitempty"`
	Scheduler                   ControlPlaneComponent   `json:"scheduler,omitempty"`
	DNS                         DNS                     `json:"dns,omitempty"`
	Proxy                       Proxy                   `json:"proxy,omitempty"`
	CertificatesDir             string                  `json:"certificatesDir,omitempty"`
	ImageRepository             string                  `json:"imageRepository,omitempty"`
	FeatureGates                map[string]bool         `json:"featureGates,omitempty"`
	ClusterName                 string                  `json:"clusterName,omitempty"`
	EncryptionAlgorithm         EncryptionAlgorithmType `json:"encryptionAlgorithm,omitempty"`
	CertificateValidityPeriod   *metav1.Duration        `json:"certificateValidityPeriod,omitempty"`
	CACertificateValidityPeriod *metav1.Duration        `json:"caCertificateValidityPeriod,omitempty"`
}

// ControlPlaneComponent holds settings common to control plane components. In
// v1beta4 ExtraArgs is a list of name/value pairs (v1beta3 used a map).
type ControlPlaneComponent struct {
	ExtraArgs    []Arg           `json:"extraArgs,omitempty"`
	ExtraVolumes []HostPathMount `json:"extraVolumes,omitempty"`
	ExtraEnvs    []EnvVar        `json:"extraEnvs,omitempty"`
}

// APIServer holds settings necessary for API server deployments in the cluster.
type APIServer struct {
	ControlPlaneComponent `json:",inline"`
	CertSANs              []string `json:"certSANs,omitempty"`
}

// Arg represents a name/value pair; v1beta4 extraArgs are a list of these.
type Arg struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EnvVar represents an environment variable present in a container.
type EnvVar struct {
	corev1.EnvVar `json:",inline"`
}

// HostPathMount contains elements describing volumes that are mounted from the host.
type HostPathMount struct {
	Name      string              `json:"name"`
	HostPath  string              `json:"hostPath"`
	MountPath string              `json:"mountPath"`
	ReadOnly  bool                `json:"readOnly,omitempty"`
	PathType  corev1.HostPathType `json:"pathType,omitempty"`
}

// DNS defines the DNS addon that should be used in the cluster.
type DNS struct {
	ImageMeta `json:",inline"`
	Disabled  bool `json:"disabled,omitempty"`
}

// ImageMeta allows to customize the image used for components that are not
// originated from the Kubernetes release process.
type ImageMeta struct {
	ImageRepository string `json:"imageRepository,omitempty"`
	ImageTag        string `json:"imageTag,omitempty"`
}

// Proxy defines the proxy addon that should be used in the cluster.
type Proxy struct {
	Disabled bool `json:"disabled,omitempty"`
}

// Networking contains elements describing cluster's networking configuration.
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

// LocalEtcd describes that kubeadm should run an etcd cluster locally.
type LocalEtcd struct {
	ImageMeta `json:",inline"`

	DataDir        string   `json:"dataDir"`
	ExtraArgs      []Arg    `json:"extraArgs,omitempty"`
	ExtraEnvs      []EnvVar `json:"extraEnvs,omitempty"`
	ServerCertSANs []string `json:"serverCertSANs,omitempty"`
	PeerCertSANs   []string `json:"peerCertSANs,omitempty"`
}

// ExternalEtcd describes an external etcd cluster.
type ExternalEtcd struct {
	Endpoints []string `json:"endpoints"`
	CAFile    string   `json:"caFile"`
	CertFile  string   `json:"certFile"`
	KeyFile   string   `json:"keyFile"`
}

// EncryptionAlgorithmType defines the asymmetric encryption algorithm used for
// keys and certificates (e.g. "RSA-2048", "ECDSA-P256").
type EncryptionAlgorithmType string

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
	KubeletExtraArgs      []Arg             `json:"kubeletExtraArgs,omitempty"`
	IgnorePreflightErrors []string          `json:"ignorePreflightErrors,omitempty"`
	ImagePullPolicy       corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	ImagePullSerial       *bool             `json:"imagePullSerial,omitempty"`
}

// JoinConfiguration contains elements describing a particular node.
type JoinConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	DryRun           bool                    `json:"dryRun,omitempty"`
	NodeRegistration NodeRegistrationOptions `json:"nodeRegistration,omitempty"`
	CACertPath       string                  `json:"caCertPath,omitempty"`
	Discovery        Discovery               `json:"discovery"`
	ControlPlane     *JoinControlPlane       `json:"controlPlane,omitempty"`
	SkipPhases       []string                `json:"skipPhases,omitempty"`
	Patches          *Patches                `json:"patches,omitempty"`
	Timeouts         *Timeouts               `json:"timeouts,omitempty"`
}

// JoinControlPlane contains elements describing an additional control plane instance to be deployed on the joining node.
type JoinControlPlane struct {
	LocalAPIEndpoint APIEndpoint `json:"localAPIEndpoint,omitempty"`
	CertificateKey   string      `json:"certificateKey,omitempty"`
}

// Discovery specifies the options for the kubelet to use during the TLS Bootstrap process.
type Discovery struct {
	BootstrapToken    *BootstrapTokenDiscovery `json:"bootstrapToken,omitempty"`
	File              *FileDiscovery           `json:"file,omitempty"`
	TLSBootstrapToken string                   `json:"tlsBootstrapToken,omitempty"`
}

// BootstrapTokenDiscovery is used to set the options for bootstrap token based discovery.
type BootstrapTokenDiscovery struct {
	Token                    string   `json:"token"`
	APIServerEndpoint        string   `json:"apiServerEndpoint,omitempty"`
	CACertHashes             []string `json:"caCertHashes,omitempty"`
	UnsafeSkipCAVerification bool     `json:"unsafeSkipCAVerification,omitempty"`
}

// FileDiscovery is used to specify a file or URL to a kubeconfig file from which to load cluster information.
type FileDiscovery struct {
	KubeConfigPath string `json:"kubeConfigPath"`
}

// Patches contains options related to applying patches to components deployed by kubeadm.
type Patches struct {
	Directory string `json:"directory,omitempty"`
}

// Timeouts holds various timeouts that apply to kubeadm commands. In v1beta3
// these lived on APIServer.TimeoutForControlPlane and Discovery.Timeout.
type Timeouts struct {
	ControlPlaneComponentHealthCheck *metav1.Duration `json:"controlPlaneComponentHealthCheck,omitempty"`
	KubeletHealthCheck               *metav1.Duration `json:"kubeletHealthCheck,omitempty"`
	KubernetesAPICall                *metav1.Duration `json:"kubernetesAPICall,omitempty"`
	EtcdAPICall                      *metav1.Duration `json:"etcdAPICall,omitempty"`
	TLSBootstrap                     *metav1.Duration `json:"tlsBootstrap,omitempty"`
	Discovery                        *metav1.Duration `json:"discovery,omitempty"`
	UpgradeManifests                 *metav1.Duration `json:"upgradeManifests,omitempty"`
}
