// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/site"
)

// siteNamespace is a namespace a full-stage site must run workloads in, and
// what puts them there.
type siteNamespace struct {
	name   string
	source string
}

// siteNamespaces is every namespace the site validation requires a workload
// in. kube-system comes from kubeadm. cert-manager and capi-system come from
// the stack's clusterctl init. The rest are the namespaces
// deploy/nico/nvke-deploy.yaml installs into, which
// TestUnit_SiteNamespacesCoverTheDeployPath keeps this list in step with.
//
// provisioned-system is absent on purpose. nvkectl bootstrap site installs
// cluster-api-provider-provisioned into the ephemeral Kind cluster and tears
// that cluster down. The cluster this check reads is the hardware site, and
// its infrastructure controller is CAPNICo in capnico-system.
//
// Required by name because a namespace with nothing in it passes every
// readiness check: an item that installed nothing would otherwise read as
// healthy.
var siteNamespaces = []siteNamespace{
	{name: kubeSystemNamespace, source: "kubeadm"},
	{name: "cert-manager", source: "cert-manager's default namespace, installed with CAPI"},
	{name: "capi-system", source: "clusterctl init, before the deploy path"},
	{name: "capnico-system", source: "deploy path item capnico"},
	{name: "postgres", source: "deploy path item postgresql"},
	{name: "temporal", source: "deploy path item temporal"},
	{name: "spire-server", source: "deploy path item spire"},
	// Not a release namespace: the spire chart's recommended layout puts its
	// agent and CSI driver here, so no deploy path file names it as a release.
	{name: "spire-system", source: "the spire chart's layout for its agent"},
	{name: "nktp", source: "deploy path item nktp"},
	// NVKE_CONTROLLER_NAMESPACE where the controller is not in nke-controller,
	// as for every other check that finds it.
	{name: controller.Namespace(), source: "deploy path item controller"},
}

const (
	// siteSettle bounds the wait for the site's workloads to converge. Short,
	// because the deploy already waited for each item to be ready, so this
	// covers only pods still rolling when the last item returned. Every minute
	// here is one a broken site takes from the lane's Ginkgo timeout, and a
	// run that reaches that timeout can lose the teardown that releases the
	// machine.
	siteSettle = 4 * time.Minute
	// siteCheck bounds the checks that should pass at once on a site whose
	// workloads have settled.
	siteCheck = 2 * time.Minute
	sitePoll  = 10 * time.Second
	// siteStable is how long the pod check must stay clean once it is. A
	// crash-looping container with no readiness probe reads as Ready for a
	// moment after each restart, so one clean poll proves nothing.
	siteStable = 30 * time.Second
	// siteStablePoll samples the pod check through siteStable often enough
	// that a crash-looping container is unlikely to read as Ready at every
	// one of them.
	siteStablePoll = 2 * time.Second
)

// siteChecks is what the site validation checks against and how it runs.
type siteChecks struct {
	// namespaces must each run at least one workload.
	namespaces []siteNamespace
	// failFast skips every check after the first that fails.
	failFast bool
	// probeNamespace and probeImage are where the DNS probe pod runs and what
	// it runs.
	probeNamespace string
	probeImage     probeImage
	// excusedNamespace is left out of the pod check: the runner's own, where
	// an earlier run's pods may be left Failed or not Ready. "" excuses none.
	excusedNamespace string
	// restConfig, when set, reaches the cluster to check in place of the
	// site's kubeconfig or the suite's in-cluster config.
	restConfig func() (*rest.Config, error)
	// tenant marks the cluster as a tenant: the specs that only a site
	// controller can pass skip.
	tenant bool
	// declaredTaints, set for a tenant, maps each node to the taints its node
	// pool declares. The node check lets a tenant's nodes carry those and,
	// on a control-plane node, kubeadm's taint; any other taint that keeps
	// pods off a node fails.
	declaredTaints func(ctx context.Context) (map[string][]corev1.Taint, error)
	// certManager requires cert-manager: its Certificates checked, and an
	// Issuer tried.
	certManager bool
}

// siteLaneChecks are the site lane's: the stack's namespaces, and fail fast
// so that a broken site cannot spend the minutes teardown needs.
var siteLaneChecks = siteChecks{
	namespaces:     siteNamespaces,
	failFast:       true,
	probeNamespace: "default",
	probeImage:     probeImage{ref: kube.KubectlImage},
	certManager:    true,
}

// probeImage is what every probe and server pod runs, and the Secrets its
// pods pull it with. The pods run as their namespace's default service
// account, which holds no pull Secret of its own on most sites.
type probeImage struct {
	ref         string
	pullSecrets []corev1.LocalObjectReference
}

// probePodUser is nobody: the restricted PodSecurity level refuses root, and
// the image needs no account of its own for it.
const probePodUser = 65534

// apply sets the image on every container in spec, pulled only when the node
// lacks it (an untagged image defaults to always), and its pull Secrets. It
// also runs spec as the restricted PodSecurity level requires, since the probe
// namespace is the runner's own, not one the run creates, and may enforce it.
func (image probeImage) apply(spec *corev1.PodSpec) {
	spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      ptr.To[int64](probePodUser),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	for i := range spec.Containers {
		spec.Containers[i].Image = image.ref
		spec.Containers[i].ImagePullPolicy = corev1.PullIfNotPresent
		spec.Containers[i].SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	}
	spec.ImagePullSecrets = slices.Clone(image.pullSecrets)
}

// validateSitePostDeploy asserts on a site controller after the stack is
// installed: the site's software (what the platform and the deploy path put on
// it) and its distro (what Kubernetes itself reports).
//
// resolve returns the site as built and the spec it was built from. It runs
// when a spec does rather than when the tree is built, because the cluster
// exists only once the bootstrap spec has run. It reports false when the
// stack was not deployed, and every spec then skips: ContinueOnFailure
// reaches these after a failed install, and polling a stack that is not there
// spends minutes teardown needs. For the same reason checks.failFast skips
// the checks after the first that fails: each one reports every problem it
// found, so the ones after it add time and little evidence.
func validateSitePostDeploy(resolve func() (site.Cluster, site.Spec, bool), checks siteChecks) {
	var (
		cluster    site.Cluster
		spec       site.Spec
		restConfig *rest.Config
		c          client.Client
		invalid    bool
	)
	connect := func() {
		var deployed bool
		cluster, spec, deployed = resolve()
		if !deployed {
			Skip("the stack was not deployed")
		}
		if checks.failFast {
			if invalid {
				Skip("an earlier site validation failed")
			}
			DeferCleanup(func() { invalid = invalid || CurrentSpecReport().Failed() })
		}
		if c != nil {
			return
		}
		if checks.restConfig != nil {
			restConfig = expectSucceed(checks.restConfig())
		} else {
			restConfig = expectSucceed(kube.RESTConfigWithInClusterFallback(cluster.Kubeconfig, ""))
		}
		c = expectSucceed(siteClient(restConfig))
	}

	It("has every node Ready and schedulable", func(ctx SpecContext) {
		connect()
		Eventually(func(ctx context.Context) error {
			var nodes corev1.NodeList
			if err := c.List(ctx, &nodes); err != nil {
				return err
			}
			var declared map[string][]corev1.Taint
			if checks.declaredTaints != nil {
				var err error
				if declared, err = checks.declaredTaints(ctx); err != nil {
					return err
				}
			}
			return joinProblems("nodes", nodeProblems(nodes.Items, declared))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("readiness", "site-software"), NodeTimeout(siteCheck+time.Minute))

	It("runs every workload the platform and the deploy path install", func(ctx SpecContext) {
		connect()
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
			return joinProblems("workloads", workloadProblems(checks.namespaces, deployments.Items, statefulSets.Items, daemonSets.Items))
		}).WithContext(ctx).WithTimeout(siteSettle).WithPolling(sitePoll).Should(Succeed())
	}, Label("readiness", "site-software"), NodeTimeout(siteSettle+time.Minute))

	It("has no pod failing in any namespace", func(ctx SpecContext) {
		connect()
		pods := func(ctx context.Context) error {
			var (
				pods corev1.PodList
				jobs batchv1.JobList
			)
			if err := c.List(ctx, &pods); err != nil {
				return err
			}
			if err := c.List(ctx, &jobs); err != nil {
				return err
			}
			checked, excused := excusePods(pods.Items, checks.excusedNamespace)
			if excused > 0 {
				GinkgoLogr.Info("pod check leaves out the runner's namespace and earlier validation probes", "namespace", checks.excusedNamespace, "pods", excused)
			}
			return joinProblems("pods", podProblems(checked, jobs.Items))
		}
		Eventually(pods).WithContext(ctx).WithTimeout(siteSettle).WithPolling(sitePoll).Should(Succeed())
		Consistently(pods).WithContext(ctx).WithTimeout(siteStable).WithPolling(siteStablePoll).Should(Succeed())
	}, Label("readiness", "site-software"), NodeTimeout(siteSettle+siteStable+time.Minute))

	It("has issued every cert-manager certificate", func(ctx SpecContext) {
		connect()
		if !checks.certManager {
			Skip("the cluster is not expected to run cert-manager")
		}
		Eventually(func(ctx context.Context) error {
			certificates := &unstructured.UnstructuredList{}
			certificates.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateList"})
			if err := c.List(ctx, certificates); err != nil {
				return err
			}
			return joinProblems("certificates", certificateProblems(certificates.Items))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("readiness", "site-software"), NodeTimeout(siteCheck+time.Minute))

	It("lists clusters through the controller API", func(ctx SpecContext) {
		connect()
		if checks.tenant {
			Skip("a tenant runs no controller")
		}
		var clusters int
		Eventually(func(ctx context.Context) error {
			api, err := nvke.NewClient(ctx, restConfig, nvke.Options{Poll: sitePoll})
			if err != nil {
				return err
			}
			listed, err := api.ListClusters(ctx)
			clusters = len(listed)
			return err
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
		GinkgoLogr.Info("site controller API", "clusters", clusters)
	}, Label("readiness", "site-software"), NodeTimeout(siteCheck+time.Minute))

	It("passes every API server readiness and liveness check, etcd included", func(ctx SpecContext) {
		connect()
		clientset := expectSucceed(kubernetes.NewForConfig(restConfig))
		for _, endpoint := range []string{"/readyz", "/livez"} {
			Eventually(func(ctx context.Context) error {
				body, err := clientset.Discovery().RESTClient().Get().AbsPath(endpoint).Param("verbose", "true").DoRaw(ctx)
				if err != nil {
					return fmt.Errorf("%s: %w: %s", endpoint, err, body)
				}
				return healthCheckProblems(endpoint, string(body))
			}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
		}
	}, Label("readiness", "site-distro"), NodeTimeout(2*siteCheck+time.Minute))

	It("runs the scheduler and controller-manager on every control-plane node", func(ctx SpecContext) {
		connect()
		Eventually(func(ctx context.Context) error {
			var (
				nodes corev1.NodeList
				pods  corev1.PodList
			)
			if err := c.List(ctx, &nodes); err != nil {
				return err
			}
			if err := c.List(ctx, &pods, client.InNamespace(kubeSystemNamespace)); err != nil {
				return err
			}
			return joinProblems("control-plane components", controlPlaneProblems(nodes.Items, pods.Items))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("readiness", "site-distro"), NodeTimeout(siteCheck+time.Minute))

	It("runs CoreDNS", func(ctx SpecContext) {
		connect()
		Eventually(func(ctx context.Context) error {
			var coredns appsv1.Deployment
			if err := c.Get(ctx, client.ObjectKey{Namespace: kubeSystemNamespace, Name: "coredns"}, &coredns); err != nil {
				return err
			}
			if coredns.Status.AvailableReplicas == 0 {
				return fmt.Errorf("Deployment %s/coredns has no available replica", kubeSystemNamespace)
			}
			return nil
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("readiness", "site-distro"), NodeTimeout(siteCheck+time.Minute))

	It("runs the Kubernetes version it was built with", func(ctx SpecContext) {
		connect()
		clientset := expectSucceed(kubernetes.NewForConfig(restConfig))
		server := expectSucceed(clientset.Discovery().ServerVersion())
		var nodes corev1.NodeList
		Expect(c.List(ctx, &nodes)).To(Succeed())
		Expect(joinProblems("versions", versionProblems(spec.KubernetesVersion, server.GitVersion, nodes.Items))).To(Succeed())
	}, Label("readiness", "site-distro"), NodeTimeout(2*time.Minute))

	// The probe pod also proves the CNI: it has to get an address and reach
	// the cluster DNS service to write its message.
	//
	// The tenant suite's probe, despite its name: it resolves a name from a
	// pod and asserts nothing about NodeLocal DNSCache, which this spec does
	// not establish is on the site. A failed attempt keeps its pod for
	// debugging, so a retry can leave one behind in default.
	It("resolves cluster DNS from a pod", func(ctx SpecContext) {
		connect()
		Eventually(runDNSProbe).WithContext(ctx).WithArguments(c, checks.probeNamespace, checks.probeImage).
			WithTimeout(siteSettle).WithPolling(sitePoll).Should(Succeed())
	}, Label("readiness", "site-distro"), NodeTimeout(siteSettle+time.Minute))
}

// siteClient is a client for the site cluster that can also read Jobs, which
// the pod check needs to tell a retried Job's failed attempt from a failure.
func siteClient(restConfig *rest.Config) (client.Client, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return client.New(restConfig, client.Options{Scheme: scheme})
}

func joinProblems(what string, problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%d %s not healthy:\n  %s", len(problems), what, strings.Join(problems, "\n  "))
}

// controlPlaneTaint is the taint kubeadm puts on a control-plane node.
const controlPlaneTaint = "node-role.kubernetes.io/control-plane"

// nodeProblems reports nodes not Ready or cordoned, and nodes carrying a
// taint that keeps pods off them. declared is nil for a site. For a tenant it
// holds the taints each node's pool declares: a tenant's nodes may carry
// those, and its control-plane nodes kubeadm's taint, since its pools take
// the work.
func nodeProblems(nodes []corev1.Node, declared map[string][]corev1.Taint) []string {
	if len(nodes) == 0 {
		return []string{"the cluster has no nodes"}
	}
	var problems []string
	for _, node := range nodes {
		if !nodeReady(node) {
			problems = append(problems, node.Name+": not Ready"+notReadyDetail(node))
		}
		if node.Spec.Unschedulable {
			problems = append(problems, node.Name+": cordoned")
		}
		// A single-node site schedules everything onto its control plane,
		// and nvkectl bootstrap site removes the taint that would stop it.
		// Where another node takes the work, the taint is kubeadm's normal
		// state.
		for _, taint := range node.Spec.Taints {
			if !hardTaint(taint) {
				continue
			}
			if declared != nil {
				_, controlPlane := node.Labels[controlPlaneTaint]
				kubeadm := taint.Key == controlPlaneTaint && taint.Value == "" && taint.Effect == corev1.TaintEffectNoSchedule
				if kubeadm && controlPlane || slices.ContainsFunc(declared[node.Name], func(d corev1.Taint) bool {
					return d.Key == taint.Key && d.Value == taint.Value && d.Effect == taint.Effect
				}) {
					continue
				}
			} else if taint.Key == controlPlaneTaint && schedulableOtherThan(nodes, node.Name) {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: tainted %s:%s", node.Name, taint.Key, taint.Effect))
		}
	}
	return problems
}

// controlPlaneComponents are the kubeadm static pods every control-plane node
// runs besides the API server and etcd, which /readyz already reports on.
var controlPlaneComponents = []string{"kube-scheduler", "kube-controller-manager"}

// controlPlaneProblems requires each control-plane component's static pod,
// named <component>-<node> by the kubelet, Running and Ready on every node
// labeled control-plane. A crashing one already fails the pod check; this
// is what catches one that is missing.
func controlPlaneProblems(nodes []corev1.Node, kubeSystemPods []corev1.Pod) []string {
	pods := map[string]*corev1.Pod{}
	for i := range kubeSystemPods {
		pods[kubeSystemPods[i].Name] = &kubeSystemPods[i]
	}
	var problems []string
	controlPlanes := 0
	for _, node := range nodes {
		if _, ok := node.Labels[controlPlaneTaint]; !ok {
			continue
		}
		controlPlanes++
		for _, component := range controlPlaneComponents {
			pod, ok := pods[component+"-"+node.Name]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s: no %s pod", node.Name, component))
			case pod.Status.Phase != corev1.PodRunning || !podReady(pod):
				problems = append(problems, fmt.Sprintf("%s: %s not Ready (phase %s)", node.Name, component, pod.Status.Phase))
			}
		}
	}
	if controlPlanes == 0 {
		problems = append(problems, "no node is labeled "+controlPlaneTaint)
	}
	return problems
}

func hardTaint(taint corev1.Taint) bool {
	return taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute
}

// schedulableOtherThan reports whether a node other than name is Ready,
// uncordoned and carries no taint that keeps pods off it.
func schedulableOtherThan(nodes []corev1.Node, name string) bool {
	for _, node := range nodes {
		if node.Name == name || node.Spec.Unschedulable || !nodeReady(node) ||
			slices.ContainsFunc(node.Spec.Taints, hardTaint) {
			continue
		}
		return true
	}
	return false
}

// notReadyDetail is the reason a node's Ready condition gives, as a suffix.
func notReadyDetail(node corev1.Node) string {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return " (" + strings.TrimSpace(fmt.Sprintf("%s %s: %s", c.Status, c.Reason, c.Message)) + ")"
		}
	}
	return " (no Ready condition)"
}

func nodeReady(node corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// workloadProblems reports every Deployment, StatefulSet and DaemonSet in the
// cluster that is not fully available, and every required namespace with no
// workload running at least one ready pod.
//
// Cluster-wide rather than per item, so a workload nothing here names, the
// CNI's among them, still has to be healthy. A namespace counts as running
// only on a ready pod: a Deployment scaled to zero, or a DaemonSet that
// selects no node, is healthy but runs nothing.
func workloadProblems(required []siteNamespace, deployments []appsv1.Deployment, statefulSets []appsv1.StatefulSet, daemonSets []appsv1.DaemonSet) []string {
	var problems []string
	populated := map[string]bool{}
	for _, d := range deployments {
		want := replicas(d.Spec.Replicas)
		s := d.Status
		populated[d.Namespace] = populated[d.Namespace] || s.AvailableReplicas > 0
		if s.ObservedGeneration < d.Generation || s.UpdatedReplicas < want || s.AvailableReplicas < want {
			problems = append(problems, fmt.Sprintf("Deployment %s/%s: %d of %d available, %d updated%s",
				d.Namespace, d.Name, s.AvailableReplicas, want, s.UpdatedReplicas, deploymentConditions(d)))
		}
	}
	for _, st := range statefulSets {
		want := replicas(st.Spec.Replicas)
		s := st.Status
		populated[st.Namespace] = populated[st.Namespace] || s.ReadyReplicas > 0
		if s.ObservedGeneration < st.Generation || s.ReadyReplicas < want {
			problems = append(problems, fmt.Sprintf("StatefulSet %s/%s: %d of %d ready",
				st.Namespace, st.Name, s.ReadyReplicas, want))
		}
	}
	for _, ds := range daemonSets {
		s := ds.Status
		populated[ds.Namespace] = populated[ds.Namespace] || s.NumberReady > 0
		if s.ObservedGeneration < ds.Generation || s.NumberReady < s.DesiredNumberScheduled || s.NumberAvailable < s.DesiredNumberScheduled {
			problems = append(problems, fmt.Sprintf("DaemonSet %s/%s: %d of %d ready, %d available",
				ds.Namespace, ds.Name, s.NumberReady, s.DesiredNumberScheduled, s.NumberAvailable))
		}
	}
	for _, ns := range required {
		if !populated[ns.name] {
			problems = append(problems, fmt.Sprintf("namespace %s: no workload running (expected from %s)", ns.name, ns.source))
		}
	}
	slices.Sort(problems)
	return problems
}

// deploymentConditions is what a Deployment's conditions say is wrong, as a
// suffix: Available or Progressing false, or a ReplicaFailure, which is where
// a quota or admission refusal to create its pods shows.
func deploymentConditions(d appsv1.Deployment) string {
	var details []string
	for _, c := range d.Status.Conditions {
		failing := (c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue) ||
			((c.Type == appsv1.DeploymentAvailable || c.Type == appsv1.DeploymentProgressing) && c.Status == corev1.ConditionFalse)
		if failing {
			details = append(details, strings.TrimSpace(fmt.Sprintf("%s %s: %s", c.Type, c.Reason, c.Message)))
		}
	}
	if len(details) == 0 {
		return ""
	}
	return " (" + strings.Join(details, "; ") + ")"
}

// replicas is a workload's desired count, which the API defaults to one when
// unset.
func replicas(desired *int32) int32 {
	if desired == nil {
		return 1
	}
	return *desired
}

// excusePods drops the pods in namespace, none when it is "", and every pod
// labeled validationProbeLabel, returning the rest and how many it dropped.
// The pod check runs before any probe of its own run, so every probe pod it
// sees is an earlier run's.
func excusePods(pods []corev1.Pod, namespace string) ([]corev1.Pod, int) {
	kept := slices.DeleteFunc(slices.Clone(pods), func(pod corev1.Pod) bool {
		return (namespace != "" && pod.Namespace == namespace) || pod.Labels[validationProbeLabel] == "true"
	})
	return kept, len(pods) - len(kept)
}

// stuckReasons are the waiting reasons a container does not leave by itself.
// Checked even on a pod that reads Running and Ready, which a crash-looping
// container with no readiness probe does between restarts.
var stuckReasons = []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError"}

// podProblems reports every pod that is neither Running with its containers
// Ready nor Succeeded, and every container stuck waiting.
//
// Two kinds of Failed pod are excused. One whose Job went on to complete: a
// Job retries by creating a new pod and keeps the failed one, so a schema
// migration that lost one race against its database leaves a Failed pod
// behind on a healthy site. And an evicted one with an owner: the kubelet
// keeps the pod after its controller has replaced it, and whether the
// replacement is healthy is the workload check's to say. An evicted pod
// with no owner was replaced by nothing, so it still counts.
func podProblems(pods []corev1.Pod, jobs []batchv1.Job) []string {
	completed := map[string]bool{}
	for _, job := range jobs {
		if jobComplete(job) {
			completed[job.Namespace+"/"+job.Name] = true
		}
	}
	var problems []string
	for _, pod := range pods {
		if reason := stuckContainer(pod); reason != "" {
			problems = append(problems, fmt.Sprintf("%s/%s: %s%s", pod.Namespace, pod.Name, reason, restartDetail(pod)))
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			continue
		case corev1.PodRunning:
			if podReady(&pod) {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s/%s: Running, not Ready%s", pod.Namespace, pod.Name, notReadyContainers(pod)))
		case corev1.PodFailed:
			if owner := jobOwner(pod); owner != "" && completed[pod.Namespace+"/"+owner] {
				continue
			}
			if pod.Status.Reason == "Evicted" && metav1.GetControllerOf(&pod) != nil {
				continue
			}
			problems = append(problems, strings.TrimSpace(fmt.Sprintf("%s/%s: Failed %s", pod.Namespace, pod.Name, pod.Status.Reason))+podDetails(&pod))
		case corev1.PodPending, corev1.PodUnknown:
			problems = append(problems, fmt.Sprintf("%s/%s: %s%s", pod.Namespace, pod.Name, pod.Status.Phase, podDetails(&pod)))
		default:
			problems = append(problems, fmt.Sprintf("%s/%s: phase %q", pod.Namespace, pod.Name, pod.Status.Phase))
		}
	}
	slices.Sort(problems)
	return problems
}

// restartDetail is how often a pod's containers restarted and how each last
// exited, as a suffix, or "" where none did.
func restartDetail(pod corev1.Pod) string {
	var details []string
	for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if status.RestartCount == 0 {
			continue
		}
		detail := fmt.Sprintf("container %s restarted %d times", status.Name, status.RestartCount)
		if last := status.LastTerminationState.Terminated; last != nil {
			detail += fmt.Sprintf(", last exit %d %s", last.ExitCode, last.Reason)
		}
		details = append(details, strings.TrimSpace(detail))
	}
	if len(details) == 0 {
		return ""
	}
	return " (" + strings.Join(details, "; ") + ")"
}

// notReadyContainers names a Running pod's containers that are not Ready, as
// a suffix, with what else its containers report.
func notReadyContainers(pod corev1.Pod) string {
	var names []string
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			names = append(names, status.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return " (containers " + strings.Join(names, ", ") + ")" + restartDetail(pod)
}

// stuckContainer names the first container, init containers included, that is
// waiting for one of stuckReasons, or returns "".
func stuckContainer(pod corev1.Pod) string {
	for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if waiting := status.State.Waiting; waiting != nil && slices.Contains(stuckReasons, waiting.Reason) {
			return fmt.Sprintf("container %s %s", status.Name, waiting.Reason)
		}
	}
	return ""
}

func jobComplete(job batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobOwner(pod corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" {
			return ref.Name
		}
	}
	return ""
}

// certificateProblems reports every cert-manager Certificate that is not
// Ready. At least one is required: the SPIRE item blocks on the OIDC
// provider's upstream CA, so a site with none never issued it.
func certificateProblems(certificates []unstructured.Unstructured) []string {
	if len(certificates) == 0 {
		return []string{"the cluster has no cert-manager Certificates"}
	}
	var problems []string
	for _, cert := range certificates {
		conditions, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
		ready := false
		for _, raw := range conditions {
			condition, _ := raw.(map[string]any)
			if condition["type"] == "Ready" && condition["status"] == "True" {
				ready = true
			}
		}
		if !ready {
			problems = append(problems, fmt.Sprintf("Certificate %s/%s: not Ready", cert.GetNamespace(), cert.GetName()))
		}
	}
	slices.Sort(problems)
	return problems
}

// healthCheckProblems reads a verbose /readyz or /livez body. Every check
// must pass, and on /readyz etcd must be among them: an API server reports
// healthy on whatever checks it runs, so a body without etcd's says nothing
// about etcd. /livez is exempt because it does not carry etcd on every
// Kubernetes version.
func healthCheckProblems(endpoint, body string) error {
	var failed []string
	etcd := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "[-]"):
			failed = append(failed, line)
		case line == "[+]etcd ok":
			etcd = true
		}
	}
	if endpoint == "/readyz" && !etcd {
		failed = append(failed, "no passing etcd check")
	}
	return joinProblems(endpoint+" checks", failed)
}

// versionProblems compares the API server and every kubelet against the
// version the site was built with, by major, minor and patch. A build suffix
// such as -vendor.1 or +abc is a packaging detail, not a different Kubernetes,
// so it does not count as a mismatch.
func versionProblems(want, server string, nodes []corev1.Node) []string {
	if want == "" {
		return []string{"no expected Kubernetes version is set (" + siteKubernetesVersionEnv + ")"}
	}
	wanted, err := utilversion.ParseGeneric(want)
	if err != nil {
		return []string{fmt.Sprintf("the site's Kubernetes version %q does not parse: %v", want, err)}
	}
	var problems []string
	check := func(what, got string) {
		reported, err := utilversion.ParseGeneric(got)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: version %q does not parse", what, got))
			return
		}
		if reported.Major() != wanted.Major() || reported.Minor() != wanted.Minor() || reported.Patch() != wanted.Patch() {
			problems = append(problems, fmt.Sprintf("%s: %s, want %s", what, got, want))
		}
	}
	check("API server", server)
	for _, node := range nodes {
		check(node.Name+": kubelet", node.Status.NodeInfo.KubeletVersion)
	}
	return problems
}
