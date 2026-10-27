// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

func TestUnit_TenantValidateFromEnvDefaults(t *testing.T) {
	settings, err := tenantValidateFromEnv(envMap(map[string]string{tenantClusterEnv: " blue "}), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.Equal(t, "blue", settings.cluster)
	require.Equal(t, "nvke-validate-blue", settings.tokenSecret)
	require.Equal(t, "default", settings.runnerNamespace)
	require.Equal(t, []siteNamespace{{name: kubeSystemNamespace, source: "kubeadm"}}, settings.checks.namespaces)
	require.Equal(t, defaultTenantNamespace, settings.checks.probeNamespace)
	require.Equal(t, defaultTenantNamespace, settings.checks.excusedNamespace, "the probe namespace's own leftovers do not fail the pod check")
	require.Equal(t, probeImage{ref: kube.KubectlImage}, settings.checks.probeImage)
	require.Equal(t, defaultTenantGPUImage, settings.gpuImage)
	require.Nil(t, settings.gpuNodes)
	require.True(t, settings.checks.tenant)
	require.False(t, settings.checks.certManager)
	require.False(t, settings.checks.failFast)
	require.Equal(t, tenantPrefix, settings.expect.prefix)
}

func TestUnit_TenantValidateFromEnvUnsetSkipsEverything(t *testing.T) {
	settings, err := tenantValidateFromEnv(envMap(nil), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.False(t, settings.enabled)
	require.Empty(t, settings.cluster)
	require.Empty(t, settings.tokenSecret)
}

func TestUnit_TenantValidateToggle(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")

	settings, err := tenantValidateFromEnv(envMap(map[string]string{tenantClusterEnv: "blue"}), absent)
	require.NoError(t, err)
	require.False(t, settings.enabled, "naming a tenant alone does not turn the tenant checks on")

	settings, err = tenantValidateFromEnv(envMap(map[string]string{tenantValidateEnv: "true", tenantClusterEnv: "blue"}), absent)
	require.NoError(t, err)
	require.True(t, settings.enabled)

	_, err = tenantValidateFromEnv(envMap(map[string]string{tenantValidateEnv: "true"}), absent)
	require.ErrorContains(t, err, tenantClusterEnv, "on with no tenant fails rather than skipping")

	_, err = tenantValidateFromEnv(envMap(map[string]string{tenantValidateEnv: "yes please", tenantClusterEnv: "blue"}), absent)
	require.ErrorContains(t, err, tenantValidateEnv)
}

func TestUnit_TenantValidateFromEnvOverrides(t *testing.T) {
	namespaceFile := filepath.Join(t.TempDir(), "namespace")
	require.NoError(t, os.WriteFile(namespaceFile, []byte("validation-runner\n"), 0o600))
	settings, err := tenantValidateFromEnv(envMap(map[string]string{
		tenantClusterEnv:           "blue",
		tenantTokenSecretEnv:       "blue-reader",
		tenantNamespaceEnv:         "qa",
		tenantKubernetesVersionEnv: "v1.33.2",
		tenantNamespacesEnv:        "kube-system, gpu-operator",
		tenantProbeImageEnv:        "registry.local/probe:1",
		tenantProbePullSecretsEnv:  " tenant-pull ,",
		tenantGPUNodesEnv:          "4",
		tenantGPUImageEnv:          "registry.local/cuda:1",
		"NVKE_TENANT_COMPONENTS":   "cert-manager",
	}), namespaceFile)
	require.NoError(t, err)
	require.Equal(t, "blue-reader", settings.tokenSecret)
	require.Equal(t, "validation-runner", settings.runnerNamespace)
	require.Equal(t, "qa", settings.checks.probeNamespace)
	require.Equal(t, "v1.33.2", settings.kubernetesVersion)
	require.Equal(t, []siteNamespace{{name: "kube-system", source: tenantNamespacesEnv}, {name: "gpu-operator", source: tenantNamespacesEnv}}, settings.checks.namespaces)
	require.Equal(t, probeImage{ref: "registry.local/probe:1", pullSecrets: []corev1.LocalObjectReference{{Name: "tenant-pull"}}}, settings.checks.probeImage)
	require.Equal(t, ptr.To(4), settings.gpuNodes)
	require.Equal(t, "registry.local/cuda:1", settings.gpuImage)
	require.True(t, settings.checks.certManager, "a tenant that lists cert-manager gets the cert-manager checks")
}

func TestUnit_TenantValidateFromEnvRejects(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"GPU count not a number": {tenantGPUNodesEnv: "four"},
		"GPU count negative":     {tenantGPUNodesEnv: "-1"},
		"namespaces empty":       {tenantNamespacesEnv: " , "},
		"unknown component":      {"NVKE_TENANT_COMPONENTS": "flux"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tenantValidateFromEnv(envMap(env), filepath.Join(t.TempDir(), "absent"))
			require.Error(t, err)
		})
	}
}

func TestUnit_TenantClusterNamed(t *testing.T) {
	clusters := []api.Cluster{{Name: "blue"}, {Name: "green"}, {Name: "green"}}
	found, err := clusterNamed(clusters, "blue")
	require.NoError(t, err)
	require.Equal(t, "blue", found.Name)
	_, err = clusterNamed(clusters, "red")
	require.ErrorContains(t, err, "no cluster named")
	_, err = clusterNamed(clusters, "green")
	require.ErrorContains(t, err, "2 clusters named")
}

func TestUnit_TenantRESTConfig(t *testing.T) {
	secret := func(data map[string]string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "runner", Name: "blue-reader"}, Data: map[string][]byte{}}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	withVIP := api.Cluster{Name: "blue", ControlPlaneVIP: ptr.To("10.0.0.10")}

	config, err := tenantRESTConfig(withVIP, secret(map[string]string{tenantTokenKey: " tok\n", tenantCAKey: "CA"}))
	require.NoError(t, err)
	require.Equal(t, "https://10.0.0.10:6443", config.Host)
	require.Equal(t, "tok", config.BearerToken)
	require.Equal(t, []byte("CA"), config.CAData)

	config, err = tenantRESTConfig(api.Cluster{Name: "blue"}, secret(map[string]string{tenantTokenKey: "tok", tenantCAKey: "CA", tenantServerKey: "https://relay:443"}))
	require.NoError(t, err)
	require.Equal(t, "https://relay:443", config.Host, "the Secret's server wins")

	_, err = tenantRESTConfig(api.Cluster{Name: "blue"}, secret(map[string]string{tenantTokenKey: "tok", tenantCAKey: "CA"}))
	require.ErrorContains(t, err, "no control-plane VIP")
	_, err = tenantRESTConfig(withVIP, secret(map[string]string{tenantCAKey: "CA"}))
	require.ErrorContains(t, err, `no "token"`)
	_, err = tenantRESTConfig(withVIP, secret(map[string]string{tenantTokenKey: "tok"}))
	require.ErrorContains(t, err, `no "ca.crt"`)
}

func TestUnit_TenantExpectations(t *testing.T) {
	expect := tenantExpectations(siteExpectations{components: map[string]string{componentCertManager: "cert-manager"}},
		api.Cluster{EtcdEncrypted: true, ControlPlaneVIP: ptr.To("10.0.0.10")})
	require.True(t, expect.etcdEncrypted)
	require.Equal(t, map[string]string{componentCertManager: "cert-manager", componentKubeVIP: kubeSystemNamespace}, expect.components)

	listed := siteExpectations{components: map[string]string{componentKubeVIP: "vip"}}
	require.Equal(t, "vip", tenantExpectations(listed, api.Cluster{ControlPlaneVIP: ptr.To("10.0.0.10")}).components[componentKubeVIP],
		"the run's namespace for kube-vip wins")

	plain := tenantExpectations(siteExpectations{etcdEncrypted: true}, api.Cluster{ControlPlaneVIP: ptr.To(" ")})
	require.True(t, plain.etcdEncrypted, "the record adds to the run's expectations, never removes")
	require.Nil(t, plain.components)
}

func recordedNode(name string, controlPlane bool) api.Node {
	ok := api.Condition{Status: api.ConditionStatusTrue}
	no := api.Condition{Status: api.ConditionStatusFalse}
	return api.Node{Name: name, ControlPlane: controlPlane, Conditions: api.NodeConditions{
		Joined: ok, KubernetesReady: ok, NodeAgentReady: ok, Deleting: no, DeleteRequested: no,
	}}
}

func tenantNode(name string, controlPlane, ready bool, labels map[string]string) corev1.Node {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	for k, v := range labels {
		node.Labels[k] = v
	}
	if controlPlane {
		node.Labels[controlPlaneTaint] = ""
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	return node
}

func TestUnit_TenantRecordedNodeProblems(t *testing.T) {
	agentDown := recordedNode("w-2", false)
	agentDown.Conditions.NodeAgentReady = api.Condition{Status: api.ConditionStatusFalse, Message: "agent not reporting"}
	leaving := recordedNode("w-4", false)
	leaving.Conditions.Deleting.Status = api.ConditionStatusTrue
	recorded := []api.Node{recordedNode("cp-1", true), recordedNode("w-1", true), agentDown, recordedNode("w-3", false), leaving}
	nodes := []corev1.Node{
		tenantNode("cp-1", true, true, nil),
		tenantNode("w-1", false, true, nil),
		tenantNode("w-2", false, false, nil),
		tenantNode("w-4", false, false, nil),
		tenantNode("stray", false, true, nil),
	}
	require.Equal(t, []string{
		"stray: in the cluster, not recorded by the controller",
		"w-1: the controller records control plane true, the node is labeled false",
		"w-2: controller condition nodeAgentReady is False: agent not reporting",
		"w-2: not Ready",
		"w-3: recorded by the controller, not in the cluster",
	}, recordedNodeProblems(recorded, nodes))
	require.Equal(t, []string{"the controller records no node for the cluster"}, recordedNodeProblems(nil, nodes))
	require.Empty(t, recordedNodeProblems([]api.Node{recordedNode("cp-1", true)}, nodes[:1]))
}

func TestUnit_TenantNodePoolMetadataProblems(t *testing.T) {
	pool := api.NodePool{
		Name:       "gpu",
		NodeLabels: map[string]string{"pool": "gpu"},
		NodeTaints: []api.NodeTaint{{Key: "nvidia.com/gpu", Value: "present", Effect: "NoSchedule"}},
	}
	good := tenantNode("g-1", false, true, map[string]string{"pool": "gpu"})
	good.Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule}}
	bad := tenantNode("g-2", false, true, map[string]string{"pool": "cpu"})
	other := tenantNode("c-1", false, true, nil)
	members := []api.Node{{Name: "g-1"}, {Name: "g-2"}, {Name: "g-3"}}
	require.Equal(t, []string{
		`pool gpu: node g-2: label pool="cpu", want "gpu"`,
		"pool gpu: node g-2: no taint nvidia.com/gpu=present:NoSchedule",
		"pool gpu: node g-3 is not in the cluster",
	}, nodePoolMetadataProblems(pool, members, []corev1.Node{good, bad, other}))
	require.Empty(t, nodePoolMetadataProblems(pool, members[:1], []corev1.Node{good, other}))
}

func TestUnit_TenantPoolTaints(t *testing.T) {
	declared := map[string][]corev1.Taint{}
	addPoolTaints(declared, api.NodePool{NodeTaints: []api.NodeTaint{{Key: "nvidia.com/gpu", Value: "present", Effect: "NoSchedule"}}}, []api.Node{{Name: "g-1"}})
	addPoolTaints(declared, api.NodePool{}, []api.Node{{Name: "c-1"}})
	require.Equal(t, map[string][]corev1.Taint{"g-1": {{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule}}}, declared)
}

func TestUnit_TenantChecksIdentity(t *testing.T) {
	settings, err := tenantValidateFromEnv(envMap(map[string]string{tenantClusterEnv: "blue"}), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.True(t, tenantBehaviorSettings(settings, settings.checks, api.Cluster{}).checkIdentity,
		"the image reaches a tenant as the provisioned ServiceAccount, in a pod or not")
	require.False(t, tenantBehaviorSettings(laneTenantSettings, laneTenantSettings.checks, api.Cluster{}).checkIdentity,
		"a lane reaches the tenant as its admin")

	require.Len(t, identityProblems(func(authorizationv1.ResourceAttributes) bool { return true }), len(rbacDenied),
		"an identity allowed everything fails each denied request")
	require.Empty(t, identityProblems(func(authorizationv1.ResourceAttributes) bool { return false }))
}

func TestUnit_TenantPoolReservedMemory(t *testing.T) {
	want, ok, err := poolReservedMemory(api.NodePool{SystemReservedMemory: ptr.To("1Gi"), KubeReservedMemory: ptr.To("512Mi")})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 0, want.Cmp(resource.MustParse("1536Mi")))

	_, ok, err = poolReservedMemory(api.NodePool{KubeReservedMemory: ptr.To(" ")})
	require.NoError(t, err)
	require.False(t, ok)

	_, _, err = poolReservedMemory(api.NodePool{Name: "p", SystemReservedMemory: ptr.To("lots")})
	require.ErrorContains(t, err, "pool p")
}

func TestUnit_TenantGPUNodeProblems(t *testing.T) {
	gpu := func(name, count string, labels map[string]string) corev1.Node {
		node := tenantNode(name, false, true, labels)
		if count != "" {
			node.Status.Allocatable = corev1.ResourceList{gpuResource: resource.MustParse(count)}
		}
		return node
	}
	nodes := []corev1.Node{
		gpu("g-1", "8", nil),
		gpu("g-2", "0", map[string]string{"nvidia.com/gpu.present": "true"}),
		gpu("c-1", "", nil),
	}
	require.Equal(t, []string{
		"g-2: labeled nvidia.com/gpu.present but advertises no allocatable nvidia.com/gpu",
		"1 nodes advertise nvidia.com/gpu, want 2",
	}, gpuNodeProblems(nodes, 2))
	require.Empty(t, gpuNodeProblems(nodes[:1], 1))
	require.Empty(t, gpuNodeProblems([]corev1.Node{nodes[2]}, 0))
}

func TestUnit_TenantGPUPod(t *testing.T) {
	pod := gpuPod("qa", probeImage{ref: "cuda:1", pullSecrets: []corev1.LocalObjectReference{{Name: "ngc"}}},
		map[string]string{siteRunLabel: "abc123"})
	require.Equal(t, "qa", pod.Namespace)
	require.Equal(t, "abc123", pod.Labels[siteRunLabel])
	require.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
	require.Equal(t, []corev1.Toleration{{Operator: corev1.TolerationOpExists}}, pod.Spec.Tolerations)
	container := pod.Spec.Containers[0]
	require.Equal(t, gpuContainer, container.Name)
	require.Equal(t, "cuda:1", container.Image)
	require.Equal(t, int64(1), container.Resources.Limits.Name(gpuResource, resource.DecimalSI).Value())
	require.Equal(t, []corev1.LocalObjectReference{{Name: "ngc"}}, pod.Spec.ImagePullSecrets)
	require.Equal(t, ptr.To[int64](probePodUser), pod.Spec.SecurityContext.RunAsUser, "restricted, as the probe pods")
	require.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
}

func TestUnit_ValidateInputsDescribed(t *testing.T) {
	settings, err := tenantValidateFromEnv(envMap(map[string]string{
		tenantClusterEnv:                 "blue",
		tenantGPUNodesEnv:                "2",
		"NVKE_TENANT_COMPONENTS":         "kube-vip",
		"NVKE_TENANT_APISERVER_FLAGS":    "profiling=false",
		"NVKE_TENANT_RESERVED_MEMORY":    "1Gi",
		"NVKE_TENANT_REQUIRED_WORKLOADS": "gpu-operator/nvidia-device-plugin-daemonset",
	}), filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.Equal(t, []string{
		"NVKE_TENANT_CLUSTER=blue",
		"credential Secret: default/nvke-validate-blue",
		"NVKE_TENANT_GPU_NODES=2",
		"GPU image: " + defaultTenantGPUImage,
		"required namespaces: kube-system",
		"probe namespace: " + defaultTenantNamespace,
		"probe image: " + kube.KubectlImage,
		"NVKE_TENANT_COMPONENTS=kube-vip=kube-system",
		"NVKE_TENANT_REQUIRED_WORKLOADS=gpu-operator/nvidia-device-plugin-daemonset",
		"NVKE_TENANT_APISERVER_FLAGS=profiling=false",
		"NVKE_TENANT_RESERVED_MEMORY=1Gi",
	}, settings.describe())

	target := &tenantTarget{
		record:     api.Cluster{Name: "blue", ControlPlaneKubernetesVersion: "v1.33.2"},
		restConfig: &rest.Config{Host: "https://10.0.0.10:6443", BearerToken: "secret-token"},
	}
	described := strings.Join(target.describe(), "\n")
	require.Contains(t, described, "API server: https://10.0.0.10:6443")
	require.Contains(t, described, "recorded control-plane VIP: none")
	require.NotContains(t, described, "secret-token", "the report never carries the credential")
}

func TestUnit_ValidateKeepOnFailureRejectsGarbage(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	_, err := tenantValidateFromEnv(envMap(map[string]string{keepOnFailureEnv: "sometimes"}), absent)
	require.ErrorContains(t, err, keepOnFailureEnv)
	_, err = siteValidateFromEnv(envMap(map[string]string{keepOnFailureEnv: "sometimes"}), absent)
	require.ErrorContains(t, err, keepOnFailureEnv)
	_, err = siteValidateFromEnv(envMap(map[string]string{keepOnFailureEnv: "true"}), absent)
	require.NoError(t, err)
}
