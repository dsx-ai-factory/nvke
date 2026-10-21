// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

// What a site is expected to have beyond what every NVKE site has. Each is
// an input because sites differ in it; a check whose input is unset skips.
const (
	// siteComponentsEnv lists the optional components the site runs,
	// comma-separated, each as name or name=namespace. Names are the keys of
	// siteComponentNamespaces.
	siteComponentsEnv = "NVKE_SITE_COMPONENTS"
	// siteStorageClassesEnv lists StorageClasses that must exist.
	siteStorageClassesEnv = "NVKE_SITE_STORAGE_CLASSES"
	// sitePVCStorageClassEnv is the class the PVC check binds from. Unset
	// uses the default class, and skips where there is none.
	sitePVCStorageClassEnv = "NVKE_SITE_PVC_STORAGE_CLASS"
	// siteWorkloadVersionsEnv lists namespace/workload=version, comma-separated:
	// a Deployment, StatefulSet or DaemonSet one of whose containers must run
	// an image tagged version. The CNI's version is given this way.
	siteWorkloadVersionsEnv = "NVKE_SITE_WORKLOAD_VERSIONS"
	// siteRequiredWorkloadsEnv lists namespace/workload, comma-separated: a
	// Deployment, StatefulSet or DaemonSet that must exist, fully available.
	// It names what a site's SPIRE, CNI and telemetry stack must run.
	siteRequiredWorkloadsEnv = "NVKE_SITE_REQUIRED_WORKLOADS"
	// siteAPIServerFlagsEnv lists flags every API server must run with, as
	// name=value separated by semicolons, because values hold commas.
	siteAPIServerFlagsEnv = "NVKE_SITE_APISERVER_FLAGS"
	// siteEtcdEncryptedEnv, when true, requires every API server to encrypt
	// at rest.
	siteEtcdEncryptedEnv = "NVKE_SITE_ETCD_ENCRYPTED"
	// siteReservedMemoryEnv is the least memory every node must hold back
	// from pods, as a quantity such as 2Gi.
	siteReservedMemoryEnv = "NVKE_SITE_RESERVED_MEMORY"
)

const (
	componentArgoCD   = "argocd"
	componentCNPG     = "cnpg"
	componentTeleport = "teleport"
	componentKubeVIP  = "kube-vip"
)

// siteComponentNamespaces are the optional components and the namespace each
// is looked for in unless the run names another.
var siteComponentNamespaces = map[string]string{
	componentArgoCD:   "argocd",
	componentCNPG:     "cnpg-system",
	componentTeleport: "teleport-agent",
	componentKubeVIP:  kubeSystemNamespace,
}

// argoCDWorkloads are parts of a workload name every Argo CD install has,
// whatever its release prefix.
var argoCDWorkloads = []string{"application-controller", "repo-server", "argocd-server"}

// kubeVIPLease is the lease kube-vip's control-plane leader holds.
const kubeVIPLease = "plndr-cp-lock"

// siteRunLabel marks every object one run creates, so that runs sharing a
// namespace select only their own pods.
const siteRunLabel = "e2e.nvidia.com/site-run"

type workloadVersion struct {
	namespace, name, version string
}

type workloadRef struct {
	namespace, name string
}

type siteExpectations struct {
	components       map[string]string
	storageClasses   []string
	pvcStorageClass  string
	workloadVersions []workloadVersion
	workloads        []workloadRef
	apiServerFlags   map[string]string
	etcdEncrypted    bool
	reservedMemory   *resource.Quantity
}

func siteExpectationsFromEnv(getenv func(string) string) (siteExpectations, error) {
	var expect siteExpectations

	for _, entry := range splitList(getenv(siteComponentsEnv), ",") {
		name, namespace, _ := strings.Cut(entry, "=")
		name, namespace = strings.TrimSpace(name), strings.TrimSpace(namespace)
		defaultNamespace, known := siteComponentNamespaces[name]
		if !known {
			return siteExpectations{}, fmt.Errorf("%s: unknown component %q (known: %s)", siteComponentsEnv, name, strings.Join(slices.Sorted(maps.Keys(siteComponentNamespaces)), ", "))
		}
		if namespace == "" {
			namespace = defaultNamespace
		}
		if expect.components == nil {
			expect.components = map[string]string{}
		}
		expect.components[name] = namespace
	}

	expect.storageClasses = splitList(getenv(siteStorageClassesEnv), ",")
	expect.pvcStorageClass = strings.TrimSpace(getenv(sitePVCStorageClassEnv))

	for _, entry := range splitList(getenv(siteWorkloadVersionsEnv), ",") {
		workload, version, _ := strings.Cut(entry, "=")
		namespace, name, _ := strings.Cut(strings.TrimSpace(workload), "/")
		version = strings.TrimSpace(version)
		if namespace == "" || name == "" || version == "" {
			return siteExpectations{}, fmt.Errorf("%s: %q is not namespace/workload=version", siteWorkloadVersionsEnv, entry)
		}
		expect.workloadVersions = append(expect.workloadVersions, workloadVersion{namespace: namespace, name: name, version: version})
	}

	for _, entry := range splitList(getenv(siteRequiredWorkloadsEnv), ",") {
		namespace, name, _ := strings.Cut(entry, "/")
		namespace, name = strings.TrimSpace(namespace), strings.TrimSpace(name)
		if namespace == "" || name == "" || strings.Contains(name, "/") {
			return siteExpectations{}, fmt.Errorf("%s: %q is not namespace/workload", siteRequiredWorkloadsEnv, entry)
		}
		expect.workloads = append(expect.workloads, workloadRef{namespace: namespace, name: name})
	}

	for _, entry := range splitList(getenv(siteAPIServerFlagsEnv), ";") {
		name, value, ok := strings.Cut(entry, "=")
		name = strings.TrimPrefix(strings.TrimSpace(name), "--")
		if !ok || name == "" {
			return siteExpectations{}, fmt.Errorf("%s: %q is not name=value", siteAPIServerFlagsEnv, entry)
		}
		if expect.apiServerFlags == nil {
			expect.apiServerFlags = map[string]string{}
		}
		expect.apiServerFlags[name] = strings.TrimSpace(value)
	}

	encrypted, err := parseRequireEnv(siteEtcdEncryptedEnv, getenv(siteEtcdEncryptedEnv))
	if err != nil {
		return siteExpectations{}, err
	}
	expect.etcdEncrypted = encrypted

	if raw := strings.TrimSpace(getenv(siteReservedMemoryEnv)); raw != "" {
		quantity, err := resource.ParseQuantity(raw)
		if err != nil {
			return siteExpectations{}, fmt.Errorf("%s=%q: %w", siteReservedMemoryEnv, raw, err)
		}
		expect.reservedMemory = &quantity
	}
	return expect, nil
}

// splitList splits raw on sep, trimming each entry and dropping empty ones.
func splitList(raw, sep string) []string {
	var entries []string
	for entry := range strings.SplitSeq(raw, sep) {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

const (
	siteCreateTimeout = 5 * time.Minute
	siteCleanup       = time.Minute
)

// validateSiteBehavior asserts what the post-deploy checks leave out: the
// optional components a run says the site has, the settings of its control
// plane, and that what workloads rely on works, by trying it.
//
// The specs labeled creates make short-lived objects in the probe namespace
// and delete them, whether they pass or fail; a runner that may create
// nothing filters them out with !creates.
func validateSiteBehavior(resolve func() siteValidateSettings) {
	var (
		settings   siteValidateSettings
		restConfig *rest.Config
		clientset  kubernetes.Interface
		c          client.Client
	)
	connect := func() siteExpectations {
		settings = resolve()
		if c == nil {
			restConfig = expectSucceed(kube.RESTConfigWithInClusterFallback("", ""))
			clientset = expectSucceed(kubernetes.NewForConfig(restConfig))
			c = expectSucceed(siteBehaviorClient(restConfig))
		}
		return settings.expect
	}
	component := func(name string) string {
		namespace, ok := connect().components[name]
		if !ok {
			Skip(fmt.Sprintf("%s does not list %s", siteComponentsEnv, name))
		}
		return namespace
	}
	// work is where the creates specs make their objects and the label that
	// marks them as this run's.
	work := func() (string, string, map[string]string) {
		connect()
		run := utilrand.String(6)
		return settings.checks.probeNamespace, settings.checks.probeImage, map[string]string{siteRunLabel: run}
	}

	It("runs Argo CD", func(ctx SpecContext) {
		namespace := component(componentArgoCD)
		Eventually(func(ctx context.Context) error {
			return componentWorkloadProblems(ctx, c, componentArgoCD, namespace, argoCDWorkloads)
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("site-software", "component"), NodeTimeout(siteCheck+time.Minute))

	It("has every Argo CD Application Synced and Healthy", func(ctx SpecContext) {
		component(componentArgoCD)
		Eventually(func(ctx context.Context) error {
			applications := &unstructured.UnstructuredList{}
			applications.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "ApplicationList"})
			if err := c.List(ctx, applications); err != nil {
				return err
			}
			return joinProblems("Argo CD Applications", argoApplicationProblems(applications.Items))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("site-software", "component"), NodeTimeout(siteCheck+time.Minute))

	It("runs the CloudNativePG operator with every database cluster healthy", func(ctx SpecContext) {
		namespace := component(componentCNPG)
		Eventually(func(ctx context.Context) error {
			if err := componentWorkloadProblems(ctx, c, componentCNPG, namespace, nil); err != nil {
				return err
			}
			clusters := &unstructured.UnstructuredList{}
			clusters.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "ClusterList"})
			if err := c.List(ctx, clusters); err != nil {
				return err
			}
			return joinProblems("CloudNativePG clusters", cnpgClusterProblems(clusters.Items))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("site-software", "component"), NodeTimeout(siteCheck+time.Minute))

	It("runs the Teleport agent", func(ctx SpecContext) {
		namespace := component(componentTeleport)
		Eventually(func(ctx context.Context) error {
			return componentWorkloadProblems(ctx, c, componentTeleport, namespace, nil)
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("site-software", "component"), NodeTimeout(siteCheck+time.Minute))

	It("runs kube-vip on every control-plane node", func(ctx SpecContext) {
		namespace := component(componentKubeVIP)
		Eventually(func(ctx context.Context) error {
			var (
				nodes corev1.NodeList
				pods  corev1.PodList
			)
			if err := c.List(ctx, &nodes); err != nil {
				return err
			}
			if err := c.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
				return err
			}
			return joinProblems("kube-vip pods", kubeVIPProblems(nodes.Items, pods.Items))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("site-software", "component"), NodeTimeout(siteCheck+time.Minute))

	It("runs every workload the run requires", func(ctx SpecContext) {
		expect := connect()
		if len(expect.workloads) == 0 {
			Skip(siteRequiredWorkloadsEnv + " is unset")
		}
		Eventually(func(ctx context.Context) error {
			var (
				deployments  appsv1.DeploymentList
				statefulSets appsv1.StatefulSetList
				daemonSets   appsv1.DaemonSetList
			)
			for _, list := range []client.ObjectList{&deployments, &statefulSets, &daemonSets} {
				if err := c.List(ctx, list); err != nil {
					return err
				}
			}
			return joinProblems("required workloads", requiredWorkloadProblems(expect.workloads, deployments.Items, statefulSets.Items, daemonSets.Items))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("site-software"), NodeTimeout(siteCheck+time.Minute))

	It("runs the workload versions the run expects", func(ctx SpecContext) {
		expect := connect()
		if len(expect.workloadVersions) == 0 {
			Skip(siteWorkloadVersionsEnv + " is unset")
		}
		var (
			deployments  appsv1.DeploymentList
			statefulSets appsv1.StatefulSetList
			daemonSets   appsv1.DaemonSetList
		)
		for _, list := range []client.ObjectList{&deployments, &statefulSets, &daemonSets} {
			Expect(c.List(ctx, list)).To(Succeed())
		}
		Expect(joinProblems("workload versions", workloadVersionProblems(expect.workloadVersions, deployments.Items, statefulSets.Items, daemonSets.Items))).To(Succeed())
	}, Label("site-distro"), NodeTimeout(2*time.Minute))

	It("runs every API server with the flags kubeadm and the run set", func(ctx SpecContext) {
		expect := connect()
		configuration := expectSucceed(kubeadmClusterConfiguration(ctx, c))
		kubeadmFlags := expectSucceed(kubeadmAPIServerExtraArgs(configuration))
		var pods corev1.PodList
		Expect(c.List(ctx, &pods, client.InNamespace(kubeSystemNamespace), client.MatchingLabels{"component": "kube-apiserver"})).To(Succeed())
		Expect(joinProblems("API server flags", apiServerFlagProblems(kubeadmFlags, expect.apiServerFlags, pods.Items))).To(Succeed())
	}, Label("site-distro"), NodeTimeout(2*time.Minute))

	It("encrypts Secrets at rest", func(ctx SpecContext) {
		if !connect().etcdEncrypted {
			Skip(siteEtcdEncryptedEnv + " is not true")
		}
		var pods corev1.PodList
		Expect(c.List(ctx, &pods, client.InNamespace(kubeSystemNamespace), client.MatchingLabels{"component": "kube-apiserver"})).To(Succeed())
		Expect(joinProblems("API servers", encryptionProblems(pods.Items))).To(Succeed())
	}, Label("site-distro"), NodeTimeout(2*time.Minute))

	It("runs etcd with the quota kubeadm sets", func(ctx SpecContext) {
		connect()
		configuration := expectSucceed(kubeadmClusterConfiguration(ctx, c))
		quota, local := expectSucceed2(kubeadmEtcdQuota(configuration))
		if !local {
			Skip("kubeadm runs no local etcd")
		}
		var pods corev1.PodList
		Expect(c.List(ctx, &pods, client.InNamespace(kubeSystemNamespace), client.MatchingLabels{"component": "etcd"})).To(Succeed())
		Expect(joinProblems("etcd members", etcdQuotaProblems(quota, pods.Items))).To(Succeed())
	}, Label("site-distro"), NodeTimeout(2*time.Minute))

	It("reserves the memory the run expects on every node", func(ctx SpecContext) {
		want := connect().reservedMemory
		if want == nil {
			Skip(siteReservedMemoryEnv + " is unset")
		}
		var nodes corev1.NodeList
		Expect(c.List(ctx, &nodes)).To(Succeed())
		Expect(joinProblems("nodes", reservedMemoryProblems(nodes.Items, *want))).To(Succeed())
	}, Label("site-distro"), NodeTimeout(2*time.Minute))

	It("has every StorageClass the run expects", func(ctx SpecContext) {
		expect := connect()
		if len(expect.storageClasses) == 0 {
			Skip(siteStorageClassesEnv + " is unset")
		}
		var classes storagev1.StorageClassList
		Expect(c.List(ctx, &classes)).To(Succeed())
		Expect(joinProblems("StorageClasses", storageClassProblems(expect.storageClasses, classes.Items))).To(Succeed())
	}, Label("site-distro"), NodeTimeout(2*time.Minute))

	It("denies what default RBAC does not grant", func(ctx SpecContext) {
		connect()
		var problems []string
		for _, attributes := range rbacDenied {
			review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: ptr.To(attributes)}}
			result := expectSucceed(clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{}))
			if result.Status.Allowed {
				problems = append(problems, "the suite's own identity may "+describeAttributes(attributes)+": it should hold read access only")
			}
		}
		anonymous := expectSucceed(kubernetes.NewForConfig(rest.AnonymousClientConfig(restConfig)))
		_, err := anonymous.CoreV1().Secrets(kubeSystemNamespace).List(ctx, metav1.ListOptions{Limit: 1})
		if problem := anonymousProblem(err); problem != "" {
			problems = append(problems, problem)
		}
		Expect(joinProblems("RBAC checks", problems)).To(Succeed())
	}, Label("site-distro"), NodeTimeout(2*time.Minute))

	It("has a ready endpoint behind every admission webhook", func(ctx SpecContext) {
		connect()
		Eventually(func(ctx context.Context) error {
			var (
				validating admissionregistrationv1.ValidatingWebhookConfigurationList
				mutating   admissionregistrationv1.MutatingWebhookConfigurationList
				endpoints  discoveryv1.EndpointSliceList
			)
			for _, list := range []client.ObjectList{&validating, &mutating, &endpoints} {
				if err := c.List(ctx, list); err != nil {
					return err
				}
			}
			return joinProblems("admission webhooks", webhookProblems(webhookServices(validating.Items, mutating.Items), endpoints.Items))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("site-distro"), NodeTimeout(siteCheck+time.Minute))

	It("reaches the API server at the control-plane endpoint from another node", func(ctx SpecContext) {
		namespace, image, labels := work()
		configuration := expectSucceed(kubeadmClusterConfiguration(ctx, c))
		endpoint := expectSucceed(controlPlaneEndpoint(configuration))
		if endpoint == "" {
			Skip("kubeadm-config sets no controlPlaneEndpoint")
		}
		holder := ""
		var lease coordinationv1.Lease
		switch err := c.Get(ctx, client.ObjectKey{Namespace: kubeSystemNamespace, Name: kubeVIPLease}, &lease); {
		case err == nil:
			holder = ptr.Deref(lease.Spec.HolderIdentity, "")
		case !apierrors.IsNotFound(err):
			Expect(err).NotTo(HaveOccurred())
		}
		var nodes corev1.NodeList
		Expect(c.List(ctx, &nodes)).To(Succeed())
		candidates := slices.DeleteFunc(probeNodes(nodes.Items), func(n corev1.Node) bool { return n.Name == holder })
		if len(candidates) == 0 {
			Skip(fmt.Sprintf("no node but the endpoint's holder %q can run the probe", holder))
		}
		GinkgoLogr.Info("control-plane endpoint probe", "endpoint", endpoint, "holder", holder, "from", candidates[0].Name)
		pod := endpointProbePod(namespace, image, labels, candidates[0], endpoint)
		_, message, err := kube.RunPodWithLogs(ctx, c, clientset, pod, podCIDRProbeContainer, siteCreateTimeout)
		deleteOnExit(ctx, c, pod)
		Expect(err).NotTo(HaveOccurred())
		GinkgoLogr.Info("control-plane endpoint probe", "result", message)
	}, Label("site-distro", "creates"), NodeTimeout(siteCreateTimeout+time.Minute))

	It("carries traffic between pods and through a Service, across nodes where there are two", func(ctx SpecContext) {
		namespace, image, labels := work()
		var nodes corev1.NodeList
		Expect(c.List(ctx, &nodes)).To(Succeed())
		targets := probeNodes(nodes.Items)
		Expect(targets).NotTo(BeEmpty(), "no Ready, schedulable node to run pods on")
		if len(targets) > 2 {
			targets = targets[:2]
		}

		servers := make([]*corev1.Pod, 0, len(targets))
		for _, node := range targets {
			server := siteServerPod(namespace, image, labels, "traffic", node)
			Expect(c.Create(ctx, server)).To(Succeed())
			deleteOnExit(ctx, c, server)
			servers = append(servers, waitForPodReady(ctx, c, client.ObjectKeyFromObject(server)))
		}
		service := siteService(namespace, labels, "traffic", false)
		Expect(c.Create(ctx, service)).To(Succeed())
		deleteOnExit(ctx, c, service)

		for i, node := range targets {
			urls := []string{
				podCIDRURL(servers[i].Status.PodIP, podCIDRServerPort),
				podCIDRURL(servers[(i+1)%len(servers)].Status.PodIP, podCIDRServerPort),
				podCIDRURL(service.Name, podCIDRServerPort),
				podCIDRURL(service.Spec.ClusterIP, podCIDRServerPort),
			}
			probe := siteProbePod(namespace, image, labels, "traffic", node, urls)
			_, message, err := kube.RunPodWithLogs(ctx, c, clientset, probe, podCIDRProbeContainer, siteCreateTimeout)
			deleteOnExit(ctx, c, probe)
			Expect(err).NotTo(HaveOccurred(), "traffic from %s", node.Name)
			GinkgoLogr.Info("pod traffic", "from", node.Name, "result", message)
		}
	}, Label("site-distro", "creates"), NodeTimeout(4*siteCreateTimeout))

	It("enforces a NetworkPolicy", func(ctx SpecContext) {
		namespace, image, labels := work()
		var nodes corev1.NodeList
		Expect(c.List(ctx, &nodes)).To(Succeed())
		targets := probeNodes(nodes.Items)
		Expect(targets).NotTo(BeEmpty(), "no Ready, schedulable node to run pods on")
		node := targets[0]

		target := siteServerPod(namespace, image, labels, ovnPolicyTargetRole, node)
		control := siteServerPod(namespace, image, labels, ovnPolicyControlRole, node)
		for _, pod := range []*corev1.Pod{target, control} {
			Expect(c.Create(ctx, pod)).To(Succeed())
			deleteOnExit(ctx, c, pod)
		}
		targetURL := podCIDRURL(waitForPodReady(ctx, c, client.ObjectKeyFromObject(target)).Status.PodIP, podCIDRServerPort)
		controlURL := podCIDRURL(waitForPodReady(ctx, c, client.ObjectKeyFromObject(control)).Status.PodIP, podCIDRServerPort)
		probe := func(ctx context.Context, role string, reachable bool) error {
			pod := siteProbePod(namespace, image, labels, role, node, nil)
			pod.Spec.Containers[0].Command = []string{"sh", "-c", ovnPolicyProbeScript(reachable), "probe", targetURL, controlURL}
			_, _, err := kube.RunPodWithLogs(ctx, c, clientset, pod, podCIDRProbeContainer, ovnPolicyProbeTimeout)
			deleteOnExit(ctx, c, pod)
			return err
		}

		By("reaching the target before any policy selects it")
		Expect(probe(ctx, ovnPolicyDeniedSourceRole, true)).To(Succeed())

		policy := siteIngressPolicy(namespace, labels)
		Expect(c.Create(ctx, policy)).To(Succeed())
		deleteOnExit(ctx, c, policy)

		By("denying a source the policy does not allow, while the control pod stays reachable")
		Eventually(probe).WithContext(ctx).WithArguments(ovnPolicyDeniedSourceRole, false).
			WithTimeout(ovnPolicyConvergenceLimit).WithPolling(5 * time.Second).Should(Succeed())
		By("admitting the source it allows")
		Expect(probe(ctx, ovnPolicyAllowedSourceRole, true)).To(Succeed())
	}, Label("site-distro", "creates"), NodeTimeout(4*siteCreateTimeout))

	It("resolves and reaches a StatefulSet pod by its headless Service name", func(ctx SpecContext) {
		namespace, image, labels := work()
		var nodes corev1.NodeList
		Expect(c.List(ctx, &nodes)).To(Succeed())
		targets := probeNodes(nodes.Items)
		Expect(targets).NotTo(BeEmpty(), "no Ready, schedulable node to run pods on")

		service := siteService(namespace, labels, "headless", true)
		Expect(c.Create(ctx, service)).To(Succeed())
		deleteOnExit(ctx, c, service)
		statefulSet := siteStatefulSet(namespace, image, labels, service.Name, targets[0])
		Expect(c.Create(ctx, statefulSet)).To(Succeed())
		deleteOnExit(ctx, c, statefulSet)
		Eventually(func(ctx context.Context) error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(statefulSet), statefulSet); err != nil {
				return err
			}
			if statefulSet.Status.ReadyReplicas < 1 {
				return fmt.Errorf("StatefulSet %s/%s has no ready replica", namespace, statefulSet.Name)
			}
			return nil
		}).WithContext(ctx).WithTimeout(siteCreateTimeout).WithPolling(2 * time.Second).Should(Succeed())

		probe := siteProbePod(namespace, image, labels, "headless", targets[0], []string{podCIDRURL(statefulSet.Name+"-0."+service.Name, podCIDRServerPort)})
		_, _, err := kube.RunPodWithLogs(ctx, c, clientset, probe, podCIDRProbeContainer, siteCreateTimeout)
		deleteOnExit(ctx, c, probe)
		Expect(err).NotTo(HaveOccurred())
	}, Label("site-distro", "creates"), NodeTimeout(3*siteCreateTimeout))

	It("binds a PersistentVolumeClaim and mounts it in a pod", func(ctx SpecContext) {
		namespace, image, labels := work()
		var classes storagev1.StorageClassList
		Expect(c.List(ctx, &classes)).To(Succeed())
		class := pvcStorageClass(settings.expect.pvcStorageClass, classes.Items)
		if class == "" {
			Skip(sitePVCStorageClassEnv + " is unset and the site has no default StorageClass")
		}
		claim := sitePVC(namespace, labels, class)
		Expect(c.Create(ctx, claim)).To(Succeed())
		deleteOnExit(ctx, c, claim)
		pod := siteVolumePod(namespace, image, labels, claim.Name)
		_, message, err := kube.RunPodWithLogs(ctx, c, clientset, pod, podCIDRProbeContainer, siteCreateTimeout)
		deleteOnExit(ctx, c, pod)
		Expect(err).NotTo(HaveOccurred(), "StorageClass %s", class)
		Expect(c.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(Succeed())
		Expect(claim.Status.Phase).To(Equal(corev1.ClaimBound), "StorageClass %s", class)
		GinkgoLogr.Info("persistent volume", "class", class, "volume", claim.Spec.VolumeName, "result", message)
	}, Label("site-distro", "creates"), NodeTimeout(siteCreateTimeout+time.Minute))

	It("issues a certificate from a new cert-manager Issuer", func(ctx SpecContext) {
		namespace, _, labels := work()
		name := "nvke-validate-" + labels[siteRunLabel]
		issuer, certificate := siteCertificate(namespace, name, labels)
		Expect(c.Create(ctx, issuer)).To(Succeed())
		deleteOnExit(ctx, c, issuer)
		Expect(c.Create(ctx, certificate)).To(Succeed())
		deleteOnExit(ctx, c, certificate, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}})
		Eventually(func(ctx context.Context) error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(certificate), certificate); err != nil {
				return err
			}
			return joinProblems("certificates", certificateProblems([]unstructured.Unstructured{*certificate}))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(2 * time.Second).Should(Succeed())
		var secret corev1.Secret
		Expect(c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret)).To(Succeed())
		Expect(secret.Data).To(HaveKeyWithValue(corev1.TLSCertKey, Not(BeEmpty())))
	}, Label("site-software", "creates"), NodeTimeout(siteCheck+time.Minute))
}

// siteBehaviorClient is siteClient with the APIs these specs read.
func siteBehaviorClient(restConfig *rest.Config) (client.Client, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme, storagev1.AddToScheme,
		admissionregistrationv1.AddToScheme, discoveryv1.AddToScheme, coordinationv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return client.New(restConfig, client.Options{Scheme: scheme})
}

// deleteOnExit deletes objects when the spec ends, passed or failed, in the
// foreground so that a StatefulSet's pods go with it.
func deleteOnExit(ctx context.Context, c client.Client, objects ...client.Object) {
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteCleanup)
		defer cancel()
		for _, object := range objects {
			if err := c.Delete(cleanupCtx, object, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				GinkgoLogr.Error(err, "cleanup", "kind", fmt.Sprintf("%T", object), "name", object.GetName())
			}
		}
	})
}

// componentWorkloadProblems requires a component's namespace to run at least
// one ready pod, every workload in it to be available, and a workload whose
// name holds each of required.
func componentWorkloadProblems(ctx context.Context, c client.Client, component, namespace string, required []string) error {
	var (
		deployments  appsv1.DeploymentList
		statefulSets appsv1.StatefulSetList
		daemonSets   appsv1.DaemonSetList
	)
	for _, list := range []client.ObjectList{&deployments, &statefulSets, &daemonSets} {
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return err
		}
	}
	problems := workloadProblems([]siteNamespace{{name: namespace, source: siteComponentsEnv + " " + component}}, deployments.Items, statefulSets.Items, daemonSets.Items)
	var names []string
	for _, d := range deployments.Items {
		names = append(names, d.Name)
	}
	for _, s := range statefulSets.Items {
		names = append(names, s.Name)
	}
	for _, d := range daemonSets.Items {
		names = append(names, d.Name)
	}
	problems = append(problems, missingWorkloads(namespace, names, required)...)
	return joinProblems(component+" workloads", problems)
}

func missingWorkloads(namespace string, names, required []string) []string {
	var problems []string
	for _, part := range required {
		if !slices.ContainsFunc(names, func(name string) bool { return strings.Contains(name, part) }) {
			problems = append(problems, fmt.Sprintf("namespace %s: no workload named *%s*", namespace, part))
		}
	}
	return problems
}

// argoApplicationProblems requires at least one Application, and every one
// Synced and Healthy.
func argoApplicationProblems(applications []unstructured.Unstructured) []string {
	if len(applications) == 0 {
		return []string{"the cluster has no Argo CD Applications"}
	}
	var problems []string
	for _, app := range applications {
		sync, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
		health, _, _ := unstructured.NestedString(app.Object, "status", "health", "status")
		if sync != "Synced" || health != "Healthy" {
			problems = append(problems, fmt.Sprintf("Application %s/%s: sync %q, health %q", app.GetNamespace(), app.GetName(), sync, health))
		}
	}
	slices.Sort(problems)
	return problems
}

// cnpgHealthy is the phase CloudNativePG reports for a cluster with nothing
// to do.
const cnpgHealthy = "Cluster in healthy state"

// cnpgClusterProblems requires every CloudNativePG cluster healthy with all
// its instances ready. A site with none passes: the operator check is the
// component's, and what databases it runs is the site's business.
func cnpgClusterProblems(clusters []unstructured.Unstructured) []string {
	var problems []string
	for _, cluster := range clusters {
		phase, _, _ := unstructured.NestedString(cluster.Object, "status", "phase")
		want, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
		ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
		if phase != cnpgHealthy || ready < want {
			problems = append(problems, fmt.Sprintf("Cluster %s/%s: %d of %d instances ready, phase %q", cluster.GetNamespace(), cluster.GetName(), ready, want, phase))
		}
	}
	slices.Sort(problems)
	return problems
}

// kubeVIPProblems requires a Running, Ready kube-vip pod on every
// control-plane node, as a static pod or a DaemonSet's.
func kubeVIPProblems(nodes []corev1.Node, pods []corev1.Pod) []string {
	var problems []string
	controlPlanes := 0
	for _, node := range nodes {
		if _, ok := node.Labels[controlPlaneTaint]; !ok {
			continue
		}
		controlPlanes++
		running := slices.ContainsFunc(pods, func(pod corev1.Pod) bool {
			return pod.Spec.NodeName == node.Name && strings.HasPrefix(pod.Name, componentKubeVIP) &&
				pod.Status.Phase == corev1.PodRunning && podReady(&pod)
		})
		if !running {
			problems = append(problems, node.Name+": no Ready kube-vip pod")
		}
	}
	if controlPlanes == 0 {
		problems = append(problems, "no node is labeled "+controlPlaneTaint)
	}
	return problems
}

// workloadVersionProblems requires each named workload to exist and run a
// container whose image tag is the version, with or without a leading v.
func workloadVersionProblems(want []workloadVersion, deployments []appsv1.Deployment, statefulSets []appsv1.StatefulSet, daemonSets []appsv1.DaemonSet) []string {
	templates := map[string]corev1.PodSpec{}
	for _, d := range deployments {
		templates[d.Namespace+"/"+d.Name] = d.Spec.Template.Spec
	}
	for _, s := range statefulSets {
		templates[s.Namespace+"/"+s.Name] = s.Spec.Template.Spec
	}
	for _, d := range daemonSets {
		templates[d.Namespace+"/"+d.Name] = d.Spec.Template.Spec
	}
	var problems []string
	for _, w := range want {
		key := w.namespace + "/" + w.name
		spec, ok := templates[key]
		if !ok {
			problems = append(problems, key+": no Deployment, StatefulSet or DaemonSet")
			continue
		}
		var images []string
		matched := false
		for _, container := range spec.Containers {
			images = append(images, container.Image)
			matched = matched || strings.TrimPrefix(imageTag(container.Image), "v") == strings.TrimPrefix(w.version, "v")
		}
		if !matched {
			problems = append(problems, fmt.Sprintf("%s: no container runs %s (images %s)", key, w.version, strings.Join(images, ", ")))
		}
	}
	return problems
}

// requiredWorkloadProblems requires each named workload to exist and run at
// least one ready pod. Whether it runs all it wants is the cluster-wide
// workload check's to say; this one says what must be there at all.
func requiredWorkloadProblems(want []workloadRef, deployments []appsv1.Deployment, statefulSets []appsv1.StatefulSet, daemonSets []appsv1.DaemonSet) []string {
	ready := map[string]int32{}
	found := map[string]bool{}
	for _, d := range deployments {
		key := d.Namespace + "/" + d.Name
		found[key], ready[key] = true, d.Status.AvailableReplicas
	}
	for _, s := range statefulSets {
		key := s.Namespace + "/" + s.Name
		found[key], ready[key] = true, s.Status.ReadyReplicas
	}
	for _, d := range daemonSets {
		key := d.Namespace + "/" + d.Name
		found[key], ready[key] = true, d.Status.NumberReady
	}
	var problems []string
	for _, w := range want {
		key := w.namespace + "/" + w.name
		switch {
		case !found[key]:
			problems = append(problems, key+": no Deployment, StatefulSet or DaemonSet")
		case ready[key] == 0:
			problems = append(problems, key+": no ready pod")
		}
	}
	return problems
}

// imageTag is a reference's tag, without its digest; "" when it has none.
func imageTag(image string) string {
	image, _, _ = strings.Cut(image, "@")
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[colon+1:]
	}
	return ""
}

func kubeadmClusterConfiguration(ctx context.Context, c client.Client) (string, error) {
	var configMap corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: kubeadmConfigMapNamespace, Name: kubeadmConfigMapName}, &configMap); err != nil {
		return "", fmt.Errorf("getting kubeadm ConfigMap: %w", err)
	}
	configuration := configMap.Data[kubeadmConfigDataKey]
	if strings.TrimSpace(configuration) == "" {
		return "", fmt.Errorf("kubeadm ConfigMap is missing non-empty %q data", kubeadmConfigDataKey)
	}
	return configuration, nil
}

// decodeKubeadmConfiguration decodes a ClusterConfiguration into a generic
// map, because extraArgs is a map in v1beta3 and a list in v1beta4.
func decodeKubeadmConfiguration(configuration string) (map[string]any, error) {
	configJSON, err := yaml.YAMLToJSON([]byte(configuration))
	if err != nil {
		return nil, fmt.Errorf("converting kubeadm ClusterConfiguration to JSON: %w", err)
	}
	var config map[string]any
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return nil, fmt.Errorf("decoding kubeadm ClusterConfiguration: %w", err)
	}
	return config, nil
}

// kubeadmExtraArgs flattens an extraArgs value of either version.
func kubeadmExtraArgs(raw any, where string) (map[string]string, error) {
	args := map[string]string{}
	switch extraArgs := raw.(type) {
	case nil:
	case map[string]any:
		for name, value := range extraArgs {
			if rendered, ok := kubeadmArgValue(value); ok {
				args[name] = rendered
			}
		}
	case []any:
		for index, item := range extraArgs {
			entry, ok := item.(map[string]any)
			name, _ := entry["name"].(string)
			if !ok || name == "" {
				return nil, fmt.Errorf("kubeadm %s.extraArgs[%d] has no name", where, index)
			}
			if rendered, ok := kubeadmArgValue(entry["value"]); ok {
				args[name] = rendered
			}
		}
	default:
		return nil, fmt.Errorf("kubeadm %s.extraArgs has unsupported type %T", where, extraArgs)
	}
	return args, nil
}

// kubeadmAPIServerExtraArgs is the API server's extraArgs; none set is an
// empty map.
func kubeadmAPIServerExtraArgs(configuration string) (map[string]string, error) {
	config, err := decodeKubeadmConfiguration(configuration)
	if err != nil {
		return nil, err
	}
	section, _ := config["apiServer"].(map[string]any)
	return kubeadmExtraArgs(section["extraArgs"], "apiServer")
}

// kubeadmEtcdQuota is local etcd's quota-backend-bytes, "" when kubeadm sets
// none, and false when kubeadm runs no local etcd.
func kubeadmEtcdQuota(configuration string) (string, bool, error) {
	config, err := decodeKubeadmConfiguration(configuration)
	if err != nil {
		return "", false, err
	}
	etcd, _ := config["etcd"].(map[string]any)
	if _, external := etcd["external"]; external {
		return "", false, nil
	}
	section, _ := etcd["local"].(map[string]any)
	args, err := kubeadmExtraArgs(section["extraArgs"], "etcd.local")
	if err != nil {
		return "", true, err
	}
	return args["quota-backend-bytes"], true, nil
}

// controlPlaneEndpoint is kubeadm's controlPlaneEndpoint as host:port, with
// kubeadm's default port where it names none; "" when unset.
func controlPlaneEndpoint(configuration string) (string, error) {
	config, err := decodeKubeadmConfiguration(configuration)
	if err != nil {
		return "", err
	}
	endpoint, _ := config["controlPlaneEndpoint"].(string)
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", nil
	}
	if _, _, err := net.SplitHostPort(endpoint); err != nil {
		return net.JoinHostPort(strings.Trim(endpoint, "[]"), "6443"), nil
	}
	return endpoint, nil
}

// apiServerFlagProblems requires at least one API server, and each to run
// every flag kubeadm and the run set, with their values.
func apiServerFlagProblems(kubeadmFlags, runFlags map[string]string, pods []corev1.Pod) []string {
	if len(pods) == 0 {
		return []string{"no kube-apiserver pod in " + kubeSystemNamespace}
	}
	want := map[string]string{}
	sources := map[string]string{}
	for name, value := range kubeadmFlags {
		want[name], sources[name] = value, "kubeadm-config"
	}
	var problems []string
	for name, value := range runFlags {
		if kubeadm, ok := want[name]; ok && kubeadm != value {
			problems = append(problems, fmt.Sprintf("--%s: kubeadm-config sets %q, %s wants %q", name, kubeadm, siteAPIServerFlagsEnv, value))
			continue
		}
		want[name], sources[name] = value, siteAPIServerFlagsEnv
	}
	for _, pod := range pods {
		for _, name := range slices.Sorted(maps.Keys(want)) {
			got, found := containerFlag(pod.Spec.Containers, "kube-apiserver", name)
			switch {
			case !found:
				problems = append(problems, fmt.Sprintf("%s: no --%s (set by %s)", pod.Name, name, sources[name]))
			case got != want[name]:
				problems = append(problems, fmt.Sprintf("%s: --%s=%s, want %s (set by %s)", pod.Name, name, got, want[name], sources[name]))
			}
		}
	}
	return problems
}

// containerFlag is a flag's value in a container's command and args, and
// whether the flag is there at all: kubeadm renders an empty value as
// --name=, which containerFlagValue reads as missing.
func containerFlag(containers []corev1.Container, containerName, flagName string) (string, bool) {
	for _, container := range containers {
		if container.Name != containerName {
			continue
		}
		args := slices.Concat(container.Command, container.Args)
		for i, arg := range args {
			if value, ok := strings.CutPrefix(arg, "--"+flagName+"="); ok {
				return value, true
			}
			if arg == "--"+flagName && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				return args[i+1], true
			}
		}
	}
	return "", false
}

func encryptionProblems(pods []corev1.Pod) []string {
	if len(pods) == 0 {
		return []string{"no kube-apiserver pod in " + kubeSystemNamespace}
	}
	var problems []string
	for _, pod := range pods {
		if _, err := containerFlagValue(pod.Spec.Containers, "kube-apiserver", encryptionProviderConfigFlag); err != nil {
			problems = append(problems, fmt.Sprintf("%s: no --%s, so Secrets are stored in plain text", pod.Name, encryptionProviderConfigFlag))
		}
	}
	return problems
}

// etcdQuotaProblems requires every etcd member to run kubeadm's quota, and
// none to set one where kubeadm sets none.
func etcdQuotaProblems(want string, pods []corev1.Pod) []string {
	if len(pods) == 0 {
		return []string{"no etcd pod in " + kubeSystemNamespace}
	}
	var problems []string
	for _, pod := range pods {
		got, err := containerFlagValue(pod.Spec.Containers, "etcd", "quota-backend-bytes")
		if err != nil {
			got = ""
		}
		if got != want {
			problems = append(problems, fmt.Sprintf("%s: --quota-backend-bytes=%q, kubeadm-config sets %q", pod.Name, got, want))
		}
	}
	return problems
}

func reservedMemoryProblems(nodes []corev1.Node, want resource.Quantity) []string {
	if len(nodes) == 0 {
		return []string{"the cluster has no nodes"}
	}
	var problems []string
	for _, node := range nodes {
		capacity, hasCapacity := node.Status.Capacity[corev1.ResourceMemory]
		allocatable, hasAllocatable := node.Status.Allocatable[corev1.ResourceMemory]
		if !hasCapacity || !hasAllocatable {
			problems = append(problems, node.Name+": reports no memory capacity or allocatable")
			continue
		}
		reserved := capacity.DeepCopy()
		reserved.Sub(allocatable)
		if reserved.Cmp(want) < 0 {
			problems = append(problems, fmt.Sprintf("%s: reserves %s, want at least %s", node.Name, reserved.String(), want.String()))
		}
	}
	return problems
}

func storageClassProblems(want []string, classes []storagev1.StorageClass) []string {
	var problems []string
	for _, name := range want {
		if !slices.ContainsFunc(classes, func(class storagev1.StorageClass) bool { return class.Name == name }) {
			problems = append(problems, "no StorageClass "+name)
		}
	}
	return problems
}

const defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// pvcStorageClass is the class named, else the site's default, else "".
func pvcStorageClass(named string, classes []storagev1.StorageClass) string {
	if named != "" {
		return named
	}
	for _, class := range classes {
		if class.Annotations[defaultStorageClassAnnotation] == "true" {
			return class.Name
		}
	}
	return ""
}

// rbacDenied are requests a validation identity must not be allowed: it
// reads, and a site where it can do these has granted it more than that.
var rbacDenied = []authorizationv1.ResourceAttributes{
	{Verb: "list", Resource: "secrets"},
	{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"},
	{Verb: "*", Group: "*", Resource: "*"},
}

func describeAttributes(a authorizationv1.ResourceAttributes) string {
	what := a.Resource
	if a.Group != "" {
		what += "." + a.Group
	}
	return a.Verb + " " + what + " in every namespace"
}

// anonymousProblem reads an anonymous request for kube-system's Secrets:
// it must be refused.
func anonymousProblem(err error) string {
	switch {
	case err == nil:
		return "an anonymous request listed kube-system's Secrets"
	case apierrors.IsUnauthorized(err), apierrors.IsForbidden(err):
		return ""
	default:
		return fmt.Sprintf("an anonymous request for kube-system's Secrets failed other than as refused: %v", err)
	}
}

// webhookService is a Service an admission webhook calls.
type webhookService struct {
	webhook, namespace, name string
}

func webhookServices(validating []admissionregistrationv1.ValidatingWebhookConfiguration, mutating []admissionregistrationv1.MutatingWebhookConfiguration) []webhookService {
	var services []webhookService
	add := func(configuration, webhook string, config admissionregistrationv1.WebhookClientConfig) {
		if config.Service != nil {
			services = append(services, webhookService{webhook: configuration + "/" + webhook, namespace: config.Service.Namespace, name: config.Service.Name})
		}
	}
	for _, configuration := range validating {
		for _, webhook := range configuration.Webhooks {
			add("ValidatingWebhookConfiguration "+configuration.Name, webhook.Name, webhook.ClientConfig)
		}
	}
	for _, configuration := range mutating {
		for _, webhook := range configuration.Webhooks {
			add("MutatingWebhookConfiguration "+configuration.Name, webhook.Name, webhook.ClientConfig)
		}
	}
	return services
}

// webhookProblems requires every webhook Service to have a ready endpoint.
// One without fails every request it intercepts, or lets them all through
// unchecked, by its failure policy.
func webhookProblems(services []webhookService, endpointSlices []discoveryv1.EndpointSlice) []string {
	var problems []string
	for _, service := range services {
		ready := slices.ContainsFunc(endpointSlices, func(slice discoveryv1.EndpointSlice) bool {
			return slice.Namespace == service.namespace && slice.Labels[discoveryv1.LabelServiceName] == service.name &&
				slices.ContainsFunc(slice.Endpoints, func(e discoveryv1.Endpoint) bool { return ptr.Deref(e.Conditions.Ready, false) })
		})
		if !ready {
			problems = append(problems, fmt.Sprintf("%s: Service %s/%s has no ready endpoint", service.webhook, service.namespace, service.name))
		}
	}
	slices.Sort(problems)
	return problems
}

// probeNodes are the nodes a probe pod can be placed on: Ready, uncordoned,
// with a hostname label and no hard taint but the control plane's, which the
// probes tolerate.
func probeNodes(nodes []corev1.Node) []corev1.Node {
	var usable []corev1.Node
	for _, node := range nodes {
		if !nodeConditionTrue(node, corev1.NodeReady) || node.Spec.Unschedulable || node.Labels[corev1.LabelHostname] == "" {
			continue
		}
		if slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool {
			return hardTaint(t) && t.Key != controlPlaneTaint && t.Key != "node-role.kubernetes.io/master"
		}) {
			continue
		}
		usable = append(usable, node)
	}
	return usable
}

func withRunLabels(labels map[string]string, role string) map[string]string {
	all := map[string]string{ovnPolicyRoleLabel: role}
	maps.Copy(all, labels)
	return all
}

func siteServerPod(namespace, image string, labels map[string]string, role string, node corev1.Node) *corev1.Pod {
	pod := podCIDRServerPod(role, node.Labels[corev1.LabelHostname], false, podCIDRServerPort)
	pod.GenerateName = "nvke-validate-" + role + "-"
	pod.Namespace = namespace
	pod.Labels = withRunLabels(labels, role)
	pod.Spec.Containers[0].Image = image
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(podCIDRServerPort)},
	}}
	return pod
}

func siteProbePod(namespace, image string, labels map[string]string, role string, node corev1.Node, targets []string) *corev1.Pod {
	pod := podCIDRProbePod(role, node.Labels[corev1.LabelHostname], false, targets)
	pod.GenerateName = "nvke-validate-" + role + "-probe-"
	pod.Namespace = namespace
	pod.Labels = withRunLabels(labels, role)
	pod.Spec.Containers[0].Image = image
	return pod
}

// siteService selects the run's pods of role; headless gives it no
// ClusterIP, so that DNS names each pod.
func siteService(namespace string, labels map[string]string, role string, headless bool) *corev1.Service {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "nvke-validate-" + role + "-", Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: withRunLabels(labels, role),
			Ports:    []corev1.ServicePort{{Port: podCIDRServerPort, TargetPort: intstr.FromInt32(podCIDRServerPort)}},
		},
	}
	if headless {
		service.Spec.ClusterIP = corev1.ClusterIPNone
	}
	return service
}

func siteStatefulSet(namespace, image string, labels map[string]string, service string, node corev1.Node) *appsv1.StatefulSet {
	server := siteServerPod(namespace, image, labels, "headless", node)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To[int32](1),
			ServiceName: service,
			Selector:    &metav1.LabelSelector{MatchLabels: server.Labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: server.Labels},
				Spec: corev1.PodSpec{
					NodeSelector: server.Spec.NodeSelector,
					Tolerations:  server.Spec.Tolerations,
					Containers:   server.Spec.Containers,
				},
			},
		},
	}
}

func sitePVC(namespace string, labels map[string]string, class string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "nvke-validate-", Namespace: namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To(class),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("64Mi"),
			}},
		},
	}
}

const siteVolumeScript = `set -eu
echo nvke-validate > /data/probe
sync
[ "$(cat /data/probe)" = nvke-validate ]
printf 'wrote and read back /data/probe' > /dev/termination-log
`

func siteVolumePod(namespace, image string, labels map[string]string, claim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "nvke-validate-volume-", Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   podCIDRTolerations(),
			Containers: []corev1.Container{{
				Name:         podCIDRProbeContainer,
				Image:        image,
				Command:      []string{"sh", "-c", siteVolumeScript},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			}}},
		},
	}
}

// siteIngressPolicy admits to the run's target pods only the run's
// allowed-source pods. Its control pods stay unselected, so a denial that
// also loses them is the network failing, not the policy working.
func siteIngressPolicy(namespace string, labels map[string]string) *networkingv1.NetworkPolicy {
	protocol := corev1.ProtocolTCP
	port := intstr.FromInt32(podCIDRServerPort)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "nvke-validate-", Namespace: namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: withRunLabels(labels, ovnPolicyTargetRole)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: withRunLabels(labels, ovnPolicyAllowedSourceRole)}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}},
			}},
		},
	}
}

// endpointProbeScript fetches /readyz at the control-plane endpoint given as
// $1, verifying the serving certificate against the cluster CA.
const endpointProbeScript = `set -eu
sa=/var/run/secrets/kubernetes.io/serviceaccount
body="$(curl -fsS --connect-timeout 5 --max-time 15 --cacert "$sa/ca.crt" \
  -H "Authorization: Bearer $(cat "$sa/token")" "https://$1/readyz")"
printf '/readyz at %s: %s' "$1" "$body" > /dev/termination-log
`

func endpointProbePod(namespace, image string, labels map[string]string, node corev1.Node, endpoint string) *corev1.Pod {
	pod := siteProbePod(namespace, image, labels, "endpoint", node, nil)
	pod.Spec.Containers[0].Command = []string{"sh", "-c", endpointProbeScript, "probe", endpoint}
	return pod
}

// siteCertificate is a self-signed Issuer and a Certificate from it, both
// named name; the Certificate's Secret is named name too.
func siteCertificate(namespace, name string, labels map[string]string) (*unstructured.Unstructured, *unstructured.Unstructured) {
	issuer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Issuer",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"selfSigned": map[string]any{}},
	}}
	certificate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"secretName": name,
			"commonName": name,
			"dnsNames":   []any{name + "." + namespace + ".svc"},
			"duration":   "1h",
			"issuerRef":  map[string]any{"name": name, "kind": "Issuer"},
		},
	}}
	for _, object := range []*unstructured.Unstructured{issuer, certificate} {
		object.SetLabels(labels)
	}
	return issuer, certificate
}
