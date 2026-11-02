// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
)

var testProbeImage = probeImage{ref: "image"}

func TestUnit_SiteProbeImageReachesEveryPod(t *testing.T) {
	image := probeImage{ref: "mirror.example/k8s:1", pullSecrets: []corev1.LocalObjectReference{{Name: "mirror"}}}
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: map[string]string{corev1.LabelHostname: "n"}}}
	labels := map[string]string{siteRunLabel: "abc123"}
	specs := map[string]corev1.PodSpec{
		"server":   siteServerPod("ns", image, labels, "traffic", node).Spec,
		"probe":    siteProbePod("ns", image, labels, siteTrafficClientRole, node, nil).Spec,
		"endpoint": endpointProbePod("ns", image, labels, node, "10.0.0.1:6443").Spec,
		"volume":   siteVolumePod("ns", image, labels, "claim").Spec,
		"stateful": siteStatefulSet("ns", image, labels, "headless", node).Spec.Template.Spec,
	}
	for name, spec := range specs {
		require.Equal(t, image.pullSecrets, spec.ImagePullSecrets, name)
		require.NotNil(t, spec.SecurityContext, name)
		require.Equal(t, ptr.To(true), spec.SecurityContext.RunAsNonRoot, name)
		require.Equal(t, ptr.To[int64](probePodUser), spec.SecurityContext.RunAsUser, name)
		require.Equal(t, &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}, spec.SecurityContext.SeccompProfile, name)
		for _, container := range spec.Containers {
			require.Equal(t, image.ref, container.Image, name)
			require.Equal(t, corev1.PullIfNotPresent, container.ImagePullPolicy, name)
			require.NotNil(t, container.SecurityContext, name)
			require.Equal(t, ptr.To(false), container.SecurityContext.AllowPrivilegeEscalation, name)
			require.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop, name)
		}
	}
	require.Equal(t, ptr.To[int64](probePodUser), specs["volume"].SecurityContext.FSGroup, "the non-root probe must be able to write the volume")
}

func TestUnit_PolicySelectingAnyFindsPoliciesThatDecideTheProbes(t *testing.T) {
	run := map[string]string{siteRunLabel: "abc123", validationProbeLabel: "true"}
	roles := []string{ovnPolicyTargetRole, ovnPolicyAllowedSourceRole}
	policy := func(name string, selector metav1.LabelSelector) networkingv1.NetworkPolicy {
		return networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: networkingv1.NetworkPolicySpec{PodSelector: selector}}
	}
	otherRun := policy("other-run", metav1.LabelSelector{MatchLabels: map[string]string{siteRunLabel: "zzz999"}})
	unrelated := policy("app", metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}})

	require.Empty(t, policySelectingAny(nil, run, roles))
	require.Empty(t, policySelectingAny([]networkingv1.NetworkPolicy{otherRun, unrelated}, run, roles),
		"another run's policy and an app's select none of this run's pods")
	require.Equal(t, "default-deny", policySelectingAny([]networkingv1.NetworkPolicy{unrelated, policy("default-deny", metav1.LabelSelector{})}, run, roles),
		"an empty selector selects every pod in the namespace")
	require.Equal(t, "sources", policySelectingAny([]networkingv1.NetworkPolicy{
		policy("sources", metav1.LabelSelector{MatchLabels: map[string]string{ovnPolicyRoleLabel: ovnPolicyAllowedSourceRole}}),
	}, run, roles), "a policy on a source decides the check's traffic as much as one on the target")
}

func TestUnit_SiteExpectationsFromEnvUnset(t *testing.T) {
	expect, err := siteExpectationsFromEnv(envMap(nil), sitePrefix)
	require.NoError(t, err)
	require.Equal(t, siteExpectations{prefix: sitePrefix}, expect, "nothing is expected unless the run says so")
}

func TestUnit_SiteExpectationsFromEnvTenantPrefix(t *testing.T) {
	expect, err := siteExpectationsFromEnv(envMap(map[string]string{
		siteComponentsEnv:           "argocd",
		"NVKE_TENANT_COMPONENTS":    "kube-vip, cert-manager",
		"NVKE_TENANT_STORAGE_CLASS": "ignored",
	}), tenantPrefix)
	require.NoError(t, err)
	require.Equal(t, map[string]string{componentKubeVIP: kubeSystemNamespace, componentCertManager: "cert-manager"}, expect.components,
		"a tenant reads its own variables, not the site's")
	require.Equal(t, "NVKE_TENANT_COMPONENTS", expect.env(siteComponentsEnv))
	require.Equal(t, siteComponentsEnv, siteExpectations{}.env(siteComponentsEnv))

	_, err = siteExpectationsFromEnv(envMap(map[string]string{"NVKE_TENANT_COMPONENTS": "flux"}), tenantPrefix)
	require.ErrorContains(t, err, "NVKE_TENANT_COMPONENTS")
}

func TestUnit_SiteExpectationsFromEnv(t *testing.T) {
	expect, err := siteExpectationsFromEnv(envMap(map[string]string{
		siteComponentsEnv:        "argocd, cnpg=databases ,,kube-vip",
		siteStorageClassesEnv:    "local-path, fast",
		sitePVCStorageClassEnv:   " fast ",
		siteWorkloadVersionsEnv:  "kube-system/calico-node=v3.29.1, ovn-kubernetes/ovnkube-node=1.1.0",
		siteRequiredWorkloadsEnv: "spire-server/spire-server, spire-system/spire-agent",
		siteAPIServerFlagsEnv:    "--profiling=false; enable-admission-plugins=NodeRestriction,PodSecurity;",
		siteEtcdEncryptedEnv:     "true",
		siteReservedMemoryEnv:    "2Gi",
	}), sitePrefix)
	require.NoError(t, err)
	require.Equal(t, map[string]string{componentArgoCD: "argocd", componentCNPG: "databases", componentKubeVIP: kubeSystemNamespace}, expect.components)
	require.Equal(t, []string{"local-path", "fast"}, expect.storageClasses)
	require.Equal(t, "fast", expect.pvcStorageClass)
	require.Equal(t, []workloadVersion{
		{namespace: "kube-system", name: "calico-node", version: "v3.29.1"},
		{namespace: "ovn-kubernetes", name: "ovnkube-node", version: "1.1.0"},
	}, expect.workloadVersions)
	require.Equal(t, []workloadRef{{namespace: "spire-server", name: "spire-server"}, {namespace: "spire-system", name: "spire-agent"}}, expect.workloads)
	require.Equal(t, map[string]string{"profiling": "false", "enable-admission-plugins": "NodeRestriction,PodSecurity"}, expect.apiServerFlags)
	require.True(t, expect.etcdEncrypted)
	require.NotNil(t, expect.reservedMemory)
	require.Equal(t, "2Gi", expect.reservedMemory.String())
}

func TestUnit_SiteExpectationsFromEnvRejects(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unknown component":       {siteComponentsEnv: "argocd,flux"},
		"version without name":    {siteWorkloadVersionsEnv: "kube-system=v1"},
		"version without value":   {siteWorkloadVersionsEnv: "kube-system/calico-node="},
		"flag without value":      {siteAPIServerFlagsEnv: "--profiling"},
		"workload without name":   {siteRequiredWorkloadsEnv: "spire-server"},
		"workload with two parts": {siteRequiredWorkloadsEnv: "a/b/c"},
		"encrypted not boolean":   {siteEtcdEncryptedEnv: "sometimes"},
		"memory not a quantity":   {siteReservedMemoryEnv: "two gigs"},
		"error reaches settings":  {siteComponentsEnv: "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := siteExpectationsFromEnv(envMap(env), sitePrefix)
			require.Error(t, err)
			_, err = siteValidateFromEnv(envMap(env), filepath.Join(t.TempDir(), "absent"))
			require.Error(t, err)
		})
	}
}

func TestUnit_SiteImageTag(t *testing.T) {
	for image, want := range map[string]string{
		"calico/node:v3.29.1":                           "v3.29.1",
		"registry.local:5000/calico/node:v3.29.1":       "v3.29.1",
		"registry.local:5000/calico/node":               "",
		"ghcr.io/ovn/ovnkube:1.1.0@sha256:0123456789ab": "1.1.0",
		"nginx@sha256:0123456789ab":                     "",
	} {
		require.Equal(t, want, imageTag(image), image)
	}
}

func TestUnit_SiteWorkloadVersionProblems(t *testing.T) {
	daemonSet := appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "calico-node"}}
	daemonSet.Spec.Template.Spec.Containers = []corev1.Container{{Image: "calico/node:v3.29.1"}, {Image: "sidecar:1"}}
	want := []workloadVersion{
		{namespace: "kube-system", name: "calico-node", version: "3.29.1"},
		{namespace: "kube-system", name: "calico-node", version: "v3.30.0"},
		{namespace: "kube-system", name: "absent", version: "v1"},
	}
	require.Equal(t, []string{
		"kube-system/calico-node: no container runs v3.30.0 (images calico/node:v3.29.1, sidecar:1)",
		"kube-system/absent: no Deployment, StatefulSet or DaemonSet",
	}, workloadVersionProblems(want, nil, nil, []appsv1.DaemonSet{daemonSet}))
}

func TestUnit_SiteRequiredWorkloadProblems(t *testing.T) {
	server := appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "spire-server", Name: "spire-server"}}
	server.Status.ReadyReplicas = 1
	agent := appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "spire-system", Name: "spire-agent"}}
	require.Equal(t, []string{
		"spire-system/spire-agent: no ready pod",
		"nktp/collector: no Deployment, StatefulSet or DaemonSet",
	}, requiredWorkloadProblems([]workloadRef{
		{namespace: "spire-server", name: "spire-server"},
		{namespace: "spire-system", name: "spire-agent"},
		{namespace: "nktp", name: "collector"},
	}, nil, []appsv1.StatefulSet{server}, []appsv1.DaemonSet{agent}))
}

func TestUnit_SiteMissingWorkloads(t *testing.T) {
	require.Equal(t, []string{"namespace argocd: no workload named *argocd-server*"}, missingWorkloads("argocd",
		[]string{"argo-cd-argocd-application-controller", "argo-cd-argocd-repo-server"}, argoCDWorkloads))
	require.Empty(t, missingWorkloads("argocd",
		[]string{"argo-cd-argocd-application-controller", "argo-cd-argocd-repo-server", "argo-cd-argocd-server"}, argoCDWorkloads))
}

func unstructuredWith(t *testing.T, namespace, name string, fields map[string]any) unstructured.Unstructured {
	t.Helper()
	object := unstructured.Unstructured{Object: map[string]any{}}
	object.SetNamespace(namespace)
	object.SetName(name)
	for path, value := range fields {
		require.NoError(t, unstructured.SetNestedField(object.Object, value, strings.Split(path, ".")...))
	}
	return object
}

func TestUnit_SiteArgoApplicationProblems(t *testing.T) {
	require.Equal(t, []string{"the cluster has no Argo CD Applications"}, argoApplicationProblems(nil))
	healthy := unstructuredWith(t, "argocd", "site", map[string]any{"status.sync.status": "Synced", "status.health.status": "Healthy"})
	drifted := unstructuredWith(t, "argocd", "drifted", map[string]any{"status.sync.status": "OutOfSync", "status.health.status": "Healthy"})
	unknown := unstructuredWith(t, "argocd", "new", nil)
	require.Equal(t, []string{
		`Application argocd/drifted: sync "OutOfSync", health "Healthy"`,
		`Application argocd/new: sync "", health ""`,
	}, argoApplicationProblems([]unstructured.Unstructured{healthy, drifted, unknown}))
}

func TestUnit_SiteCNPGClusterProblems(t *testing.T) {
	require.Empty(t, cnpgClusterProblems(nil), "an operator with no clusters is healthy")
	healthy := unstructuredWith(t, "db", "a", map[string]any{"spec.instances": int64(3), "status.readyInstances": int64(3), "status.phase": cnpgHealthy})
	degraded := unstructuredWith(t, "db", "b", map[string]any{"spec.instances": int64(3), "status.readyInstances": int64(2), "status.phase": cnpgHealthy})
	failing := unstructuredWith(t, "db", "c", map[string]any{"spec.instances": int64(1), "status.readyInstances": int64(1), "status.phase": "Failing over"})
	require.Equal(t, []string{
		`Cluster db/b: 2 of 3 instances ready, phase "Cluster in healthy state"`,
		`Cluster db/c: 1 of 1 instances ready, phase "Failing over"`,
	}, cnpgClusterProblems([]unstructured.Unstructured{healthy, degraded, failing}))
}

func controlPlaneNode(name string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{controlPlaneTaint: ""}}}
}

func readyPod(namespace, name, node string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
}

func TestUnit_SiteKubeVIPProblems(t *testing.T) {
	nodes := []corev1.Node{controlPlaneNode("cp-1"), controlPlaneNode("cp-2"), {ObjectMeta: metav1.ObjectMeta{Name: "worker"}}}
	notReady := readyPod("kube-system", "kube-vip-ds-abcde", "cp-2")
	notReady.Status.Conditions = nil
	require.Equal(t, []string{"cp-2: no Ready kube-vip pod"},
		kubeVIPProblems(nodes, []corev1.Pod{readyPod("kube-system", "kube-vip-cp-1", "cp-1"), notReady}))
	require.Equal(t, []string{"no node is labeled " + controlPlaneTaint}, kubeVIPProblems(nodes[2:], nil))
}

const v1beta3Configuration = `apiVersion: kubeadm.k8s.io/v1beta3
kind: ClusterConfiguration
controlPlaneEndpoint: 10.0.0.10
apiServer:
  extraArgs:
    profiling: "false"
    audit-log-maxage: "30"
etcd:
  local:
    extraArgs:
      quota-backend-bytes: "8589934592"
`

const v1beta4Configuration = `apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
controlPlaneEndpoint: "[fd00::10]:8443"
apiServer:
  extraArgs:
  - name: profiling
    value: "false"
etcd:
  local:
    dataDir: /var/lib/etcd
`

func TestUnit_SiteKubeadmConfiguration(t *testing.T) {
	args, err := kubeadmAPIServerExtraArgs(v1beta3Configuration)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"profiling": "false", "audit-log-maxage": "30"}, args)
	args, err = kubeadmAPIServerExtraArgs(v1beta4Configuration)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"profiling": "false"}, args)
	args, err = kubeadmAPIServerExtraArgs("kind: ClusterConfiguration\n")
	require.NoError(t, err)
	require.Empty(t, args, "no extraArgs is no flags, not an error")

	quota, local, err := kubeadmEtcdQuota(v1beta3Configuration)
	require.NoError(t, err)
	require.True(t, local)
	require.Equal(t, "8589934592", quota)
	quota, local, err = kubeadmEtcdQuota(v1beta4Configuration)
	require.NoError(t, err)
	require.True(t, local)
	require.Empty(t, quota)
	_, local, err = kubeadmEtcdQuota("etcd:\n  external:\n    endpoints: [https://etcd:2379]\n")
	require.NoError(t, err)
	require.False(t, local)

	endpoint, err := controlPlaneEndpoint(v1beta3Configuration)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.10:6443", endpoint, "kubeadm's default port")
	endpoint, err = controlPlaneEndpoint(v1beta4Configuration)
	require.NoError(t, err)
	require.Equal(t, "[fd00::10]:8443", endpoint)
	endpoint, err = controlPlaneEndpoint("kind: ClusterConfiguration\n")
	require.NoError(t, err)
	require.Empty(t, endpoint)

	_, err = kubeadmAPIServerExtraArgs("apiServer:\n  extraArgs: 3\n")
	require.Error(t, err)
}

func TestUnit_SiteParseKubeadmExtraArgsStillRequiresArgs(t *testing.T) {
	_, err := parseKubeadmExtraArgs("kind: ClusterConfiguration\n")
	require.Error(t, err, "the tenant encryption check needs apiServer.extraArgs to exist")
	args, err := parseKubeadmExtraArgs(v1beta4Configuration)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"profiling": "false"}, args)
}

func staticPod(name, container string, args ...string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Command: append([]string{container}, args...)}}},
	}
}

func TestUnit_SiteAPIServerFlagProblems(t *testing.T) {
	pods := []corev1.Pod{
		staticPod("kube-apiserver-cp-1", "kube-apiserver", "--profiling=false", "--audit-log-maxage=30"),
		staticPod("kube-apiserver-cp-2", "kube-apiserver", "--profiling=true"),
	}
	require.Equal(t, []string{
		"kube-apiserver-cp-2: no --audit-log-maxage (set by " + siteAPIServerFlagsEnv + ")",
		"kube-apiserver-cp-2: --profiling=true, want false (set by kubeadm-config)",
	}, apiServerFlagProblems(map[string]string{"profiling": "false"}, map[string]string{"audit-log-maxage": "30"}, siteAPIServerFlagsEnv, pods))
	require.Equal(t, []string{
		`--profiling: kubeadm-config sets "false", ` + siteAPIServerFlagsEnv + ` wants "true"`,
	}, apiServerFlagProblems(map[string]string{"profiling": "false"}, map[string]string{"profiling": "true"}, siteAPIServerFlagsEnv, pods[:1]))
	require.Equal(t, []string{"no kube-apiserver pod in kube-system"}, apiServerFlagProblems(nil, nil, siteAPIServerFlagsEnv, nil))
	require.Empty(t, apiServerFlagProblems(map[string]string{"runtime-config": ""}, nil, siteAPIServerFlagsEnv,
		[]corev1.Pod{staticPod("kube-apiserver-kind", "kube-apiserver", "--runtime-config=")}),
		"kubeadm renders an empty value as --name=, as kind's runtime-config")
	require.Equal(t, []string{"kube-apiserver-cp-1: no --runtime-config (set by kubeadm-config)"},
		apiServerFlagProblems(map[string]string{"runtime-config": ""}, nil, siteAPIServerFlagsEnv, pods[:1]))
}

func TestUnit_SiteEncryptionProblems(t *testing.T) {
	require.Equal(t, []string{"kube-apiserver-cp-2: no --encryption-provider-config, so Secrets are stored in plain text"},
		encryptionProblems([]corev1.Pod{
			staticPod("kube-apiserver-cp-1", "kube-apiserver", "--encryption-provider-config=/etc/kubernetes/enc.yaml"),
			staticPod("kube-apiserver-cp-2", "kube-apiserver"),
		}))
}

func TestUnit_SiteEtcdQuotaProblems(t *testing.T) {
	set := staticPod("etcd-cp-1", "etcd", "--quota-backend-bytes=8589934592")
	unset := staticPod("etcd-cp-2", "etcd")
	require.Empty(t, etcdQuotaProblems("8589934592", []corev1.Pod{set}))
	require.Empty(t, etcdQuotaProblems("", []corev1.Pod{unset}), "kubeadm's default is etcd's default")
	require.Equal(t, []string{`etcd-cp-2: --quota-backend-bytes="", kubeadm-config sets "8589934592"`},
		etcdQuotaProblems("8589934592", []corev1.Pod{set, unset}))
	require.Equal(t, []string{`etcd-cp-1: --quota-backend-bytes="8589934592", kubeadm-config sets ""`},
		etcdQuotaProblems("", []corev1.Pod{set}))
	require.Equal(t, []string{"no etcd pod in kube-system"}, etcdQuotaProblems("", nil))
}

func nodeWithMemory(name, capacity, allocatable string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(capacity)},
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(allocatable)},
		},
	}
}

func TestUnit_SiteReservedMemoryProblems(t *testing.T) {
	require.Equal(t, []string{"small: holds back 1Gi from pods (capacity less allocatable), want at least 2Gi"}, reservedMemoryProblems([]corev1.Node{
		nodeWithMemory("enough", "64Gi", "61Gi"),
		nodeWithMemory("small", "64Gi", "63Gi"),
	}, resource.MustParse("2Gi")))
	require.Equal(t, []string{"blank: reports no memory capacity or allocatable"},
		reservedMemoryProblems([]corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "blank"}}}, resource.MustParse("1Gi")))
}

func TestUnit_SiteStorageClasses(t *testing.T) {
	classes := []storagev1.StorageClass{
		{ObjectMeta: metav1.ObjectMeta{Name: "slow"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fast", Annotations: map[string]string{defaultStorageClassAnnotation: "true"}}},
	}
	require.Equal(t, []string{"no StorageClass nvme"}, storageClassProblems([]string{"fast", "nvme"}, classes))
	require.Equal(t, "fast", pvcStorageClass("", classes))
	require.Equal(t, "slow", pvcStorageClass("slow", classes))
	require.Empty(t, pvcStorageClass("", classes[:1]))
}

func TestUnit_SiteAnonymousProblem(t *testing.T) {
	secrets := schema.GroupResource{Resource: "secrets"}
	require.Empty(t, anonymousProblem(apierrors.NewUnauthorized("no")))
	require.Empty(t, anonymousProblem(apierrors.NewForbidden(secrets, "", errors.New("no"))))
	require.Equal(t, "an anonymous request listed kube-system's Secrets", anonymousProblem(nil))
	require.Contains(t, anonymousProblem(errors.New("connection refused")), "failed other than as refused")
}

func TestUnit_SiteUnauthenticatedBindingProblems(t *testing.T) {
	anonymous := rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "system:anonymous"}
	unauthenticated := rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:unauthenticated"}
	authenticated := rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:authenticated"}
	clusterRole := func(name string) rbacv1.RoleRef {
		return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}
	}
	role := func(name string) rbacv1.RoleRef {
		return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}
	}

	clusterBinding := func(name string, ref rbacv1.RoleRef, subjects ...rbacv1.Subject) rbacv1.ClusterRoleBinding {
		return rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, RoleRef: ref, Subjects: subjects}
	}
	binding := func(namespace, name string, ref rbacv1.RoleRef, subjects ...rbacv1.Subject) rbacv1.RoleBinding {
		return rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, RoleRef: ref, Subjects: subjects}
	}

	clusterBindings := []rbacv1.ClusterRoleBinding{
		clusterBinding("system:public-info-viewer", clusterRole("system:public-info-viewer"), authenticated, unauthenticated),
		clusterBinding("oidc-discovery", clusterRole("system:service-account-issuer-discovery"), unauthenticated),
		clusterBinding("open-view", clusterRole("view"), unauthenticated),
		clusterBinding("everyone-edits", clusterRole("edit"), authenticated),
	}
	bindings := []rbacv1.RoleBinding{
		binding("kube-public", "kubeadm:bootstrap-signer-clusterinfo", role("kubeadm:bootstrap-signer-clusterinfo"), anonymous),
		binding("kube-system", "kubeadm:bootstrap-signer-clusterinfo", role("kubeadm:bootstrap-signer-clusterinfo"), anonymous),
		binding("apps", "anyone-admin", clusterRole("admin"), anonymous),
	}
	require.Equal(t, []string{
		"ClusterRoleBinding open-view grants ClusterRole view to system:unauthenticated",
		"RoleBinding apps/anyone-admin grants ClusterRole admin to system:anonymous",
		"RoleBinding kube-system/kubeadm:bootstrap-signer-clusterinfo grants Role kube-system/kubeadm:bootstrap-signer-clusterinfo to system:anonymous",
	}, unauthenticatedBindingProblems(clusterBindings, bindings),
		"only the public grants pass, a Role only in its own namespace, and authenticated subjects are not this check's")
}

func TestUnit_SiteEndpointProbeNode(t *testing.T) {
	node := func(name, hostname string) corev1.Node {
		return corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: hostname}},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
		}
	}
	nodes := []corev1.Node{node("cp-1", "cp-1"), node("cp-2.example.com", "cp-2")}

	chosen, away, ok := endpointProbeNode(nodes, "cp-1")
	require.True(t, ok)
	require.True(t, away)
	require.Equal(t, "cp-2.example.com", chosen.Name)

	chosen, away, ok = endpointProbeNode(nodes, "cp-2")
	require.True(t, ok)
	require.True(t, away, "a holder that names the node by hostname is still that node")
	require.Equal(t, "cp-1", chosen.Name)

	for _, holder := range []string{"", "some-pod-name"} {
		_, away, ok = endpointProbeNode(nodes, holder)
		require.True(t, ok)
		require.False(t, away, "holder %q names no node, so the probe may share the endpoint's node", holder)
	}

	_, away, ok = endpointProbeNode(nodes[:1], "cp-1")
	require.False(t, ok)
	require.True(t, away, "only the holder is left: skip, not fail")

	_, away, ok = endpointProbeNode(nil, "")
	require.False(t, ok)
	require.False(t, away, "no node at all: fail")
}

func TestUnit_SiteWebhookProblems(t *testing.T) {
	service := func(namespace, name string) admissionregistrationv1.WebhookClientConfig {
		return admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: namespace, Name: name}}
	}
	validating := []admissionregistrationv1.ValidatingWebhookConfiguration{{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-manager-webhook"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{Name: "webhook.cert-manager.io", ClientConfig: service("cert-manager", "cert-manager-webhook")},
			{Name: "external.example.com", ClientConfig: admissionregistrationv1.WebhookClientConfig{URL: ptr.To("https://example.com")}},
		},
	}}
	mutating := []admissionregistrationv1.MutatingWebhookConfiguration{{
		ObjectMeta: metav1.ObjectMeta{Name: "capi"},
		Webhooks:   []admissionregistrationv1.MutatingWebhook{{Name: "default.cluster.x-k8s.io", ClientConfig: service("capi-system", "capi-webhook-service")}},
	}}
	services := webhookServices(validating, mutating)
	require.Len(t, services, 2, "URL webhooks name no Service")

	slice := func(namespace, name string, ready *bool) discoveryv1.EndpointSlice {
		return discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Labels: map[string]string{discoveryv1.LabelServiceName: name}},
			Endpoints:  []discoveryv1.Endpoint{{Conditions: discoveryv1.EndpointConditions{Ready: ready}}},
		}
	}
	require.Equal(t, []string{
		"MutatingWebhookConfiguration capi/default.cluster.x-k8s.io: Service capi-system/capi-webhook-service has no ready endpoint",
	}, webhookProblems(services, []discoveryv1.EndpointSlice{
		slice("cert-manager", "cert-manager-webhook", nil),
		slice("capi-system", "capi-webhook-service", ptr.To(false)),
		slice("other", "capi-webhook-service", ptr.To(true)),
	}), "a nil Ready counts as ready")
}

func TestUnit_SiteProbeNodes(t *testing.T) {
	node := func(name string, mutate func(*corev1.Node)) corev1.Node {
		n := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name}},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
		}
		if mutate != nil {
			mutate(&n)
		}
		return n
	}
	nodes := []corev1.Node{
		node("worker", nil),
		node("control-plane", func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: controlPlaneTaint, Effect: corev1.TaintEffectNoSchedule}}
		}),
		node("gpu-only", func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}}
		}),
		node("cordoned", func(n *corev1.Node) { n.Spec.Unschedulable = true }),
		node("not-ready", func(n *corev1.Node) { n.Status.Conditions = nil }),
		node("no-hostname", func(n *corev1.Node) { n.Labels = nil }),
	}
	usable := probeNodes(nodes)
	names := make([]string, 0, len(usable))
	for _, n := range usable {
		names = append(names, n.Name)
	}
	require.Equal(t, []string{"worker", "control-plane"}, names)
}

func TestUnit_SiteObjectsSelectOnlyTheirRun(t *testing.T) {
	run := map[string]string{siteRunLabel: "abc123"}
	other := map[string]string{siteRunLabel: "zzz999"}
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: map[string]string{corev1.LabelHostname: "n"}}}

	matches := func(selector metav1.LabelSelector, labels map[string]string) bool {
		s, err := metav1.LabelSelectorAsSelector(&selector)
		require.NoError(t, err)
		return s.Matches(klabels.Set(labels))
	}

	policy := siteIngressPolicy("ns", run)
	target := siteServerPod("ns", testProbeImage, run, ovnPolicyTargetRole, node)
	control := siteServerPod("ns", testProbeImage, run, ovnPolicyControlRole, node)
	allowed := siteProbePod("ns", testProbeImage, run, ovnPolicyAllowedSourceRole, node, nil)
	denied := siteProbePod("ns", testProbeImage, run, ovnPolicyDeniedSourceRole, node, nil)
	otherTarget := siteServerPod("ns", testProbeImage, other, ovnPolicyTargetRole, node)
	otherAllowed := siteProbePod("ns", testProbeImage, other, ovnPolicyAllowedSourceRole, node, nil)

	require.True(t, matches(policy.Spec.PodSelector, target.Labels))
	require.False(t, matches(policy.Spec.PodSelector, control.Labels), "the control pod stays reachable")
	require.False(t, matches(policy.Spec.PodSelector, otherTarget.Labels), "another run's target is not this policy's")
	from := *policy.Spec.Ingress[0].From[0].PodSelector
	require.True(t, matches(from, allowed.Labels))
	require.False(t, matches(from, denied.Labels))
	require.False(t, matches(from, otherAllowed.Labels))

	service := siteService("ns", run, "traffic", false)
	server := siteServerPod("ns", testProbeImage, run, "traffic", node)
	require.True(t, matches(metav1.LabelSelector{MatchLabels: service.Spec.Selector}, server.Labels))
	trafficClient := siteProbePod("ns", testProbeImage, run, siteTrafficClientRole, node, nil)
	require.False(t, matches(metav1.LabelSelector{MatchLabels: service.Spec.Selector}, trafficClient.Labels),
		"the Service must not send traffic to the probe itself")
	headlessClient := siteProbePod("ns", testProbeImage, run, siteHeadlessClientRole, node, nil)
	require.False(t, matches(metav1.LabelSelector{MatchLabels: siteService("ns", run, "headless", true).Spec.Selector}, headlessClient.Labels),
		"the headless Service must not name the probe")
	require.Empty(t, service.Spec.ClusterIP)
	require.Equal(t, corev1.ClusterIPNone, siteService("ns", run, "headless", true).Spec.ClusterIP)

	statefulSet := siteStatefulSet("ns", testProbeImage, run, "nvke-validate-headless-x", node)
	require.True(t, matches(*statefulSet.Spec.Selector, statefulSet.Spec.Template.Labels))
	require.True(t, matches(metav1.LabelSelector{MatchLabels: siteService("ns", run, "headless", true).Spec.Selector}, statefulSet.Spec.Template.Labels))
	require.Equal(t, "nvke-validate-headless-x", statefulSet.Spec.ServiceName)

	for _, pod := range []*corev1.Pod{target, allowed} {
		require.Equal(t, "ns", pod.Namespace)
		require.Equal(t, "image", pod.Spec.Containers[0].Image, "the probe image is the run's, for mirrors")
		require.Equal(t, "n", pod.Spec.NodeSelector[corev1.LabelHostname])
	}
}

func TestUnit_SiteCertificateObjects(t *testing.T) {
	issuer, certificate := siteCertificate("ns", "nvke-validate-abc123", map[string]string{siteRunLabel: "abc123"})
	issuerRef, _, _ := unstructured.NestedString(certificate.Object, "spec", "issuerRef", "name")
	secretName, _, _ := unstructured.NestedString(certificate.Object, "spec", "secretName")
	require.Equal(t, issuer.GetName(), issuerRef)
	require.Equal(t, certificate.GetName(), secretName, "the spec deletes the Secret by the Certificate's name")
	require.Equal(t, "abc123", certificate.GetLabels()[siteRunLabel])
}

func TestUnit_SiteEndpointProbeScript(t *testing.T) {
	directory := t.TempDir()
	terminationLog := filepath.Join(directory, "termination.log")
	sa := filepath.Join(directory, "sa")
	require.NoError(t, os.Mkdir(sa, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sa, "token"), []byte("token"), 0o600))

	run := func(curlExit int) (string, error) {
		script := `curl() { last=""; for arg; do last="$arg"; done; [ "$last" = "https://10.0.0.10:6443/readyz" ] || return 99; echo ok; return ` +
			string(rune('0'+curlExit)) + `; }
` + strings.NewReplacer("/dev/termination-log", terminationLog, "/var/run/secrets/kubernetes.io/serviceaccount", sa).Replace(endpointProbeScript)
		_ = os.Remove(terminationLog)
		err := exec.CommandContext(t.Context(), "/bin/sh", "-c", script, "probe", "10.0.0.10:6443").Run()
		message, _ := os.ReadFile(terminationLog)
		return string(message), err
	}
	message, err := run(0)
	require.NoError(t, err)
	require.Equal(t, "/readyz at 10.0.0.10:6443: ok", message)
	message, err = run(7)
	require.Error(t, err, "an unreachable endpoint fails the probe")
	require.Empty(t, message)
}
