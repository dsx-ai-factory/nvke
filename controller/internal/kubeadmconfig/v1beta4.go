// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"sort"

	"k8s.io/apimachinery/pkg/util/version"

	kubeadmv1beta3 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta3"
	kubeadmv1beta4 "github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig/thirdparty/kubeadmtypes/v1beta4"
)

// kubeadmV1beta4MinVersion is the first release whose kubeadm reads v1beta4.
// kubeadm 1.37 no longer reads v1beta3, so newer clusters must get v1beta4.
func kubeadmV1beta4MinVersion() *version.Version { return version.MustParseGeneric("1.31.0") }

// usesKubeadmV1beta4 reports whether kubernetesVersion renders kubeadm config
// as v1beta4.
//
// An unparseable or empty version yields false on purpose, keeping the v1beta3
// output every release before 1.37 already accepts.
func usesKubeadmV1beta4(kubernetesVersion string) bool {
	parsed, err := version.ParseGeneric(kubernetesVersion)
	if err != nil {
		return false
	}
	return !parsed.LessThan(kubeadmV1beta4MinVersion())
}

// argsV1beta4 converts a v1beta3 extraArgs map to the v1beta4 list, sorted by
// name so the rendered config is stable.
func argsV1beta4(args map[string]string) []kubeadmv1beta4.Arg {
	if len(args) == 0 {
		return nil
	}
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]kubeadmv1beta4.Arg, 0, len(names))
	for _, name := range names {
		out = append(out, kubeadmv1beta4.Arg{Name: name, Value: args[name]})
	}
	return out
}

func hostPathMountsV1beta4(mounts []kubeadmv1beta3.HostPathMount) []kubeadmv1beta4.HostPathMount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]kubeadmv1beta4.HostPathMount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, kubeadmv1beta4.HostPathMount{
			Name:      m.Name,
			HostPath:  m.HostPath,
			MountPath: m.MountPath,
			ReadOnly:  m.ReadOnly,
			PathType:  m.PathType,
		})
	}
	return out
}

func controlPlaneComponentV1beta4(c kubeadmv1beta3.ControlPlaneComponent) kubeadmv1beta4.ControlPlaneComponent {
	return kubeadmv1beta4.ControlPlaneComponent{
		ExtraArgs:    argsV1beta4(c.ExtraArgs),
		ExtraVolumes: hostPathMountsV1beta4(c.ExtraVolumes),
	}
}

func apiEndpointV1beta4(e kubeadmv1beta3.APIEndpoint) kubeadmv1beta4.APIEndpoint {
	return kubeadmv1beta4.APIEndpoint{AdvertiseAddress: e.AdvertiseAddress, BindPort: e.BindPort}
}

func patchesV1beta4(p *kubeadmv1beta3.Patches) *kubeadmv1beta4.Patches {
	if p == nil {
		return nil
	}
	return &kubeadmv1beta4.Patches{Directory: p.Directory}
}

func nodeRegistrationV1beta4(n kubeadmv1beta3.NodeRegistrationOptions) kubeadmv1beta4.NodeRegistrationOptions {
	return kubeadmv1beta4.NodeRegistrationOptions{
		Name:                  n.Name,
		CRISocket:             n.CRISocket,
		Taints:                n.Taints,
		KubeletExtraArgs:      argsV1beta4(n.KubeletExtraArgs),
		IgnorePreflightErrors: n.IgnorePreflightErrors,
		ImagePullPolicy:       n.ImagePullPolicy,
	}
}

// clusterConfigurationV1beta4 converts the rendered v1beta3 ClusterConfiguration.
// APIServer.TimeoutForControlPlane has no v1beta4 equivalent here; it moves to
// InitConfiguration.Timeouts (see initConfigurationV1beta4).
func clusterConfigurationV1beta4(cc kubeadmv1beta3.ClusterConfiguration) kubeadmv1beta4.ClusterConfiguration {
	out := kubeadmv1beta4.ClusterConfiguration{
		TypeMeta:             cc.TypeMeta,
		Networking:           kubeadmv1beta4.Networking(cc.Networking),
		KubernetesVersion:    cc.KubernetesVersion,
		ControlPlaneEndpoint: cc.ControlPlaneEndpoint,
		APIServer: kubeadmv1beta4.APIServer{
			ControlPlaneComponent: controlPlaneComponentV1beta4(cc.APIServer.ControlPlaneComponent),
			CertSANs:              cc.APIServer.CertSANs,
		},
		ControllerManager: controlPlaneComponentV1beta4(cc.ControllerManager),
		Scheduler:         controlPlaneComponentV1beta4(cc.Scheduler),
		DNS:               kubeadmv1beta4.DNS{ImageMeta: kubeadmv1beta4.ImageMeta(cc.DNS.ImageMeta)},
		CertificatesDir:   cc.CertificatesDir,
		ImageRepository:   cc.ImageRepository,
		FeatureGates:      cc.FeatureGates,
		ClusterName:       cc.ClusterName,
	}
	out.APIVersion = kubeadmv1beta4.APIVersion

	if local := cc.Etcd.Local; local != nil {
		out.Etcd.Local = &kubeadmv1beta4.LocalEtcd{
			ImageMeta:      kubeadmv1beta4.ImageMeta(local.ImageMeta),
			DataDir:        local.DataDir,
			ExtraArgs:      argsV1beta4(local.ExtraArgs),
			ServerCertSANs: local.ServerCertSANs,
			PeerCertSANs:   local.PeerCertSANs,
		}
	}
	if external := cc.Etcd.External; external != nil {
		converted := kubeadmv1beta4.ExternalEtcd(*external)
		out.Etcd.External = &converted
	}

	return out
}

// initConfigurationV1beta4 converts the rendered v1beta3 InitConfiguration and
// carries the control-plane wait from the v1beta3 ClusterConfiguration into
// timeouts.controlPlaneComponentHealthCheck.
func initConfigurationV1beta4(
	ic kubeadmv1beta3.InitConfiguration,
	cc kubeadmv1beta3.ClusterConfiguration,
) kubeadmv1beta4.InitConfiguration {
	out := kubeadmv1beta4.InitConfiguration{
		TypeMeta:         ic.TypeMeta,
		BootstrapTokens:  ic.BootstrapTokens,
		NodeRegistration: nodeRegistrationV1beta4(ic.NodeRegistration),
		LocalAPIEndpoint: apiEndpointV1beta4(ic.LocalAPIEndpoint),
		CertificateKey:   ic.CertificateKey,
		SkipPhases:       ic.SkipPhases,
		Patches:          patchesV1beta4(ic.Patches),
	}
	out.APIVersion = kubeadmv1beta4.APIVersion

	if cc.APIServer.TimeoutForControlPlane != nil {
		out.Timeouts = &kubeadmv1beta4.Timeouts{
			ControlPlaneComponentHealthCheck: cc.APIServer.TimeoutForControlPlane,
		}
	}

	return out
}

// joinConfigurationV1beta4 converts the rendered v1beta3 JoinConfiguration and
// moves discovery.timeout to timeouts.discovery.
func joinConfigurationV1beta4(jc kubeadmv1beta3.JoinConfiguration) kubeadmv1beta4.JoinConfiguration {
	out := kubeadmv1beta4.JoinConfiguration{
		TypeMeta:         jc.TypeMeta,
		NodeRegistration: nodeRegistrationV1beta4(jc.NodeRegistration),
		CACertPath:       jc.CACertPath,
		Discovery: kubeadmv1beta4.Discovery{
			TLSBootstrapToken: jc.Discovery.TLSBootstrapToken,
		},
		SkipPhases: jc.SkipPhases,
		Patches:    patchesV1beta4(jc.Patches),
	}
	out.APIVersion = kubeadmv1beta4.APIVersion

	if bt := jc.Discovery.BootstrapToken; bt != nil {
		converted := kubeadmv1beta4.BootstrapTokenDiscovery(*bt)
		out.Discovery.BootstrapToken = &converted
	}
	if f := jc.Discovery.File; f != nil {
		converted := kubeadmv1beta4.FileDiscovery(*f)
		out.Discovery.File = &converted
	}
	if jc.Discovery.Timeout != nil {
		out.Timeouts = &kubeadmv1beta4.Timeouts{Discovery: jc.Discovery.Timeout}
	}
	if cp := jc.ControlPlane; cp != nil {
		out.ControlPlane = &kubeadmv1beta4.JoinControlPlane{
			LocalAPIEndpoint: apiEndpointV1beta4(cp.LocalAPIEndpoint),
			CertificateKey:   cp.CertificateKey,
		}
	}

	return out
}

// initKubeadmDocuments returns the InitConfiguration and ClusterConfiguration
// documents in the kubeadm API version kubernetesVersion reads.
func initKubeadmDocuments(
	ic kubeadmv1beta3.InitConfiguration,
	cc kubeadmv1beta3.ClusterConfiguration,
	kubernetesVersion string,
) (any, any) {
	if !usesKubeadmV1beta4(kubernetesVersion) {
		return ic, cc
	}
	return initConfigurationV1beta4(ic, cc), clusterConfigurationV1beta4(cc)
}

// joinKubeadmDocument returns the JoinConfiguration document in the kubeadm API
// version kubernetesVersion reads.
func joinKubeadmDocument(jc kubeadmv1beta3.JoinConfiguration, kubernetesVersion string) any {
	if !usesKubeadmV1beta4(kubernetesVersion) {
		return jc
	}
	return joinConfigurationV1beta4(jc)
}
