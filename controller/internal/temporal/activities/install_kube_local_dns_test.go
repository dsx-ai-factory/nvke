// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestKubeLocalDNSManifestSetsHostNetworkFalse(t *testing.T) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(kubeLocalDNSManifest), 4096)
	var daemonSet *unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if obj.GetKind() == "DaemonSet" && obj.GetName() == "node-local-dns" {
			daemonSet = obj
		}
	}
	require.NotNil(t, daemonSet)

	// Presence matters: a missing key is the zero value after decode, which
	// is also false, and that is exactly what leaves hostNetwork: true on
	// clusters that already have the field.
	hostNetwork, found, err := unstructured.NestedBool(daemonSet.Object, "spec", "template", "spec", "hostNetwork")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, hostNetwork)
}

func TestInstallKubeLocalDNSAppliesManifestWithTenantClientProvider(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a"}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Twice()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		newCoreDNSDeployment(),
		newCoreDNSPod(true),
		newKubeDNSService(),
	).Build()
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider, cfg: config.Config{FieldManagerName: "cloud-workflow-engine"}}

	input := InstallKubeLocalDNSInput{ClusterID: cluster.ID}
	require.NoError(t, executeInstallKubeLocalDNSActivity(t, a, input))
	require.NoError(t, executeInstallKubeLocalDNSActivity(t, a, input), "the activity must be safe to retry")

	var serviceAccount corev1.ServiceAccount
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "node-local-dns",
	}, &serviceAccount))
	require.NotNil(t, serviceAccount.AutomountServiceAccountToken)
	require.False(t, *serviceAccount.AutomountServiceAccountToken)

	var upstreamService corev1.Service
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "kube-dns-upstream",
	}, &upstreamService))
	require.Equal(t, map[string]string{"k8s-app": "kube-dns"}, upstreamService.Spec.Selector)

	var configMap corev1.ConfigMap
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "node-local-dns",
	}, &configMap))
	require.Contains(t, configMap.Data["Corefile"], "health :8080")
	require.NotContains(t, configMap.Data["Corefile"], "bind ")
	require.NotContains(t, configMap.Data["Corefile"], "169.254.20.10")

	var daemonSet appsv1.DaemonSet
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "node-local-dns",
	}, &daemonSet))
	require.Contains(t, daemonSet.Spec.Template.Spec.Containers[0].Image, "registry.k8s.io/dns/k8s-dns-node-cache:")
	require.Equal(t, dnsRoleClusterDNS, daemonSet.Spec.Template.Labels[dnsRoleLabelKey])
	require.False(t, daemonSet.Spec.Template.Spec.HostNetwork)
	require.NotNil(t, daemonSet.Spec.Template.Spec.Affinity)
	require.NotNil(t, daemonSet.Spec.Template.Spec.Affinity.NodeAffinity)
	require.NotNil(t, daemonSet.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
	// Node selector terms are ORed, so any extra term could let KWOK nodes back in.
	require.Equal(t, []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "type", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"kwok"}}},
	}}, daemonSet.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms)
	require.Equal(t, []string{
		"-localip", "0.0.0.0",
		"-conf", "/config/Corefile",
		"-upstreamsvc", "kube-dns-upstream",
		"-setupinterface=false",
		"-setupiptables=false",
	}, daemonSet.Spec.Template.Spec.Containers[0].Args)
	require.Empty(t, daemonSet.Spec.Template.Spec.Containers[0].LivenessProbe.HTTPGet.Host)

	containerSecurityContext := daemonSet.Spec.Template.Spec.Containers[0].SecurityContext
	require.NotNil(t, containerSecurityContext.AllowPrivilegeEscalation)
	require.False(t, *containerSecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, containerSecurityContext.RunAsNonRoot)
	require.True(t, *containerSecurityContext.RunAsNonRoot)
	require.NotNil(t, containerSecurityContext.RunAsUser)
	require.Equal(t, int64(65532), *containerSecurityContext.RunAsUser)
	require.ElementsMatch(t, []corev1.Capability{"ALL"}, containerSecurityContext.Capabilities.Drop)
	require.ElementsMatch(t, []corev1.Capability{"NET_BIND_SERVICE"}, containerSecurityContext.Capabilities.Add)

	var metricsService corev1.Service
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "node-local-dns",
	}, &metricsService))
	require.Equal(t, corev1.ClusterIPNone, metricsService.Spec.ClusterIP)

	var kubeDNSMetrics corev1.Service
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "kube-dns-metrics",
	}, &kubeDNSMetrics))
	require.Equal(t, map[string]string{"k8s-app": "kube-dns"}, kubeDNSMetrics.Spec.Selector)
	require.Equal(t, "9153", kubeDNSMetrics.Annotations[prometheusPortAnnotation])

	var coreDNS appsv1.Deployment
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      coreDNSName,
	}, &coreDNS))
	require.Equal(t, dnsRoleClusterDNS, coreDNS.Spec.Template.Labels[dnsRoleLabelKey])

	var coreDNSPod corev1.Pod
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "coredns-0",
	}, &coreDNSPod))
	require.Equal(t, dnsRoleClusterDNS, coreDNSPod.Labels[dnsRoleLabelKey])

	var kubeDNS corev1.Service
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      kubeDNSServiceName,
	}, &kubeDNS))
	require.Equal(t, map[string]string{dnsRoleLabelKey: dnsRoleClusterDNS}, kubeDNS.Spec.Selector)
	require.Equal(t, preferLocalAnnotationVal, kubeDNS.Annotations[preferLocalAnnotationKey])
	require.NotContains(t, kubeDNS.Annotations, prometheusPortAnnotation)
	require.NotContains(t, kubeDNS.Annotations, prometheusScrapeAnnot)
	for _, port := range kubeDNS.Spec.Ports {
		require.NotEqual(t, metricsPortName, port.Name)
	}

	require.Equal(t, 2, provider.calls)
	require.Equal(t, cluster, provider.cluster)
	database.AssertExpectations(t)
}

func TestInstallKubeLocalDNSLabelsCoreDNSDaemonSet(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a"}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		newCoreDNSDaemonSet(),
		newCoreDNSPod(true),
		newKubeDNSService(),
	).Build()
	a := &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{tenantClient: tenantClient},
		cfg:                  config.Config{FieldManagerName: "cloud-workflow-engine"},
	}

	require.NoError(t, executeInstallKubeLocalDNSActivity(t, a, InstallKubeLocalDNSInput{ClusterID: cluster.ID}))

	var coreDNS appsv1.DaemonSet
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      coreDNSName,
	}, &coreDNS))
	require.Equal(t, dnsRoleClusterDNS, coreDNS.Spec.Template.Labels[dnsRoleLabelKey])
	database.AssertExpectations(t)
}

func TestInstallKubeLocalDNSDefersSelectorUntilClusterDNSPodIsReady(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a"}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	// The only CoreDNS pod is not Ready yet, so the activity must not cut
	// kube-dns over to the dns-role=cluster-dns selector on this run.
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		newCoreDNSDeployment(),
		newCoreDNSPod(false),
		newKubeDNSService(),
	).Build()
	a := &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{tenantClient: tenantClient},
		cfg:                  config.Config{FieldManagerName: "cloud-workflow-engine"},
	}

	// The activity should fail rather than silently leaving kube-dns
	// pointed at cluster-dns pods before any of them can serve traffic.
	err := executeInstallKubeLocalDNSActivity(t, a, InstallKubeLocalDNSInput{ClusterID: cluster.ID})
	require.ErrorContains(t, err, "waiting for a Ready pod")

	// Selector must stay on the original kube-dns backend
	var kubeDNS corev1.Service
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      kubeDNSServiceName,
	}, &kubeDNS))
	require.Equal(t, map[string]string{k8sAppLabelKey: kubeDNSAppLabel}, kubeDNS.Spec.Selector)
	require.Equal(t, preferLocalAnnotationVal, kubeDNS.Annotations[preferLocalAnnotationKey])
	require.NotContains(t, kubeDNS.Annotations, prometheusPortAnnotation)

	// The CoreDNS pod is labeled dns-role=cluster-dns regardless of
	// readiness, so a later retry can see it Ready and finish the cutover.
	var coreDNSPod corev1.Pod
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      "coredns-0",
	}, &coreDNSPod))
	require.Equal(t, dnsRoleClusterDNS, coreDNSPod.Labels[dnsRoleLabelKey])
	database.AssertExpectations(t)
}

func TestInstallKubeLocalDNSRewritesSelectorWhenNodeLocalDNSIsReady(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a"}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		newCoreDNSDeployment(),
		newCoreDNSPod(false),
		newNodeLocalDNSPod("node-local-dns-0", true),
		newKubeDNSService(),
	).Build()
	a := &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{tenantClient: tenantClient},
		cfg:                  config.Config{FieldManagerName: "cloud-workflow-engine"},
	}

	require.NoError(t, executeInstallKubeLocalDNSActivity(t, a, InstallKubeLocalDNSInput{ClusterID: cluster.ID}))

	var kubeDNS corev1.Service
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{
		Namespace: metav1.NamespaceSystem,
		Name:      kubeDNSServiceName,
	}, &kubeDNS))
	require.Equal(t, map[string]string{dnsRoleLabelKey: dnsRoleClusterDNS}, kubeDNS.Spec.Selector)
	database.AssertExpectations(t)
}

func TestInstallKubeLocalDNSReturnsTenantClientProviderError(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a"}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()

	providerErr := errors.New("tenant API unavailable")
	a := &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{err: providerErr},
		cfg:                  config.Config{FieldManagerName: "cloud-workflow-engine"},
	}

	err := executeInstallKubeLocalDNSActivity(t, a, InstallKubeLocalDNSInput{ClusterID: cluster.ID})
	require.ErrorContains(t, err, "failed to build tenant client for cluster")
	require.ErrorContains(t, err, providerErr.Error())
	database.AssertExpectations(t)
}

func executeInstallKubeLocalDNSActivity(t *testing.T, a *Activities, input InstallKubeLocalDNSInput) error {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.InstallKubeLocalDNS)

	_, err := env.ExecuteActivity(a.InstallKubeLocalDNS, input)
	return err
}

func newCoreDNSDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      coreDNSName,
			Namespace: metav1.NamespaceSystem,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"k8s-app": "kube-dns"},
				},
			},
		},
	}
}

func newCoreDNSDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      coreDNSName,
			Namespace: metav1.NamespaceSystem,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"k8s-app": "kube-dns"},
				},
			},
		},
	}
}

func newCoreDNSPod(ready bool) *corev1.Pod {
	return newDNSPod("coredns-0", map[string]string{k8sAppLabelKey: kubeDNSAppLabel}, ready)
}

func newNodeLocalDNSPod(name string, ready bool) *corev1.Pod {
	return newDNSPod(name, map[string]string{
		k8sAppLabelKey:  "node-local-dns",
		dnsRoleLabelKey: dnsRoleClusterDNS,
	}, ready)
}

func newDNSPod(name string, labels map[string]string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceSystem,
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: status,
			}},
		},
	}
}

func newKubeDNSService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeDNSServiceName,
			Namespace: metav1.NamespaceSystem,
			Annotations: map[string]string{
				prometheusPortAnnotation: "9153",
				prometheusScrapeAnnot:    "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"k8s-app": "kube-dns"},
			Ports: []corev1.ServicePort{
				{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt(53)},
				{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(53)},
				{Name: metricsPortName, Port: 9153, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(9153)},
			},
		},
	}
}
