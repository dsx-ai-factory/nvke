// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/site"
)

// The validation image's tenant checks: the site checks that hold on any
// Kubernetes cluster, run against one tenant, plus what the controller's
// record of that tenant says it should have.
//
// The suite reaches the tenant as a ServiceAccount the runner provisions in
// it, NVKE's tenant automation path: a read-only ClusterRole, and create and
// delete in one tenant namespace for the specs labeled creates. It never
// reads the kubeconfig the controller publishes, which with OIDC off is an
// admin credential.
const (
	// tenantClusterEnv names the tenant, as the controller API lists it.
	// Required when tenantValidateEnv is true.
	tenantClusterEnv = "NVKE_TENANT_CLUSTER"
	// tenantTokenSecretEnv names a Secret in the suite's own namespace on
	// the site holding the tenant ServiceAccount's credential under
	// tenantTokenKey and tenantCAKey, and optionally the API server URL
	// under tenantServerKey. Unset is nvke-validate-<cluster>.
	tenantTokenSecretEnv = "NVKE_TENANT_TOKEN_SECRET"
	// tenantNamespaceEnv is the tenant namespace the ServiceAccount may
	// create in, where the probe pods run. Its pods are left out of the pod
	// check.
	tenantNamespaceEnv = "NVKE_TENANT_NAMESPACE"
	// tenantKubernetesVersionEnv overrides the version the controller
	// records for the tenant's control plane.
	tenantKubernetesVersionEnv = "NVKE_TENANT_KUBERNETES_VERSION"
	// tenantNamespacesEnv replaces the namespaces that must each run a
	// workload, comma-separated. Unset is kube-system.
	tenantNamespacesEnv = "NVKE_TENANT_NAMESPACES"
	// tenantProbeImageEnv is the probe pods' image, which needs sh,
	// nslookup and curl.
	tenantProbeImageEnv = "NVKE_TENANT_PROBE_IMAGE"
	// tenantProbePullSecretsEnv lists, comma-separated, the Secrets in the
	// tenant namespace that the probe pods pull their image with.
	tenantProbePullSecretsEnv = "NVKE_TENANT_PROBE_PULL_SECRETS"
	// tenantGPUNodesEnv is how many nodes must advertise GPUs. Unset skips
	// the GPU checks.
	tenantGPUNodesEnv = "NVKE_TENANT_GPU_NODES"
	// tenantGPUImageEnv is the GPU workload's image, which needs sh. The
	// NVIDIA container runtime mounts nvidia-smi into it.
	tenantGPUImageEnv = "NVKE_TENANT_GPU_IMAGE"
)

const (
	tenantTokenKey  = "token"
	tenantCAKey     = "ca.crt"
	tenantServerKey = "server"
	// tenantAPIServerPort is where kubeadm binds a tenant's API server.
	tenantAPIServerPort    = "6443"
	defaultTenantNamespace = "nvke-validate"
	defaultTenantGPUImage  = "nvcr.io/nvidia/cuda:12.9.1-base-ubuntu24.04"
	gpuResource            = corev1.ResourceName("nvidia.com/gpu")
	// tenantConnect bounds finding the tenant and reading its credential.
	tenantConnect = 2 * time.Minute
	gpuContainer  = "gpu"
)

// tenantValidateSettings is the image's tenant validation configuration.
type tenantValidateSettings struct {
	enabled bool
	cluster string
	// tokenSecret is in runnerNamespace, the suite's own on the site.
	tokenSecret       string
	runnerNamespace   string
	kubernetesVersion string
	// adminCredential marks a tenant reached as its admin, as a lane does,
	// rather than as the ServiceAccount the README grants.
	adminCredential bool
	gpuNodes        *int
	gpuImage        string
	checks          siteChecks
	expect          siteExpectations
}

func tenantValidateFromEnv(getenv func(string) string, namespaceFile string) (tenantValidateSettings, error) {
	enabled, err := parseRequireEnv(tenantValidateEnv, getenv(tenantValidateEnv))
	if err != nil {
		return tenantValidateSettings{}, err
	}
	cluster := strings.TrimSpace(getenv(tenantClusterEnv))
	if enabled && cluster == "" {
		return tenantValidateSettings{}, fmt.Errorf("%s is true but %s names no tenant", tenantValidateEnv, tenantClusterEnv)
	}

	tokenSecret := strings.TrimSpace(getenv(tenantTokenSecretEnv))
	if tokenSecret == "" && cluster != "" {
		tokenSecret = "nvke-validate-" + cluster
	}

	runnerNamespace := "default"
	if own, readErr := os.ReadFile(namespaceFile); readErr == nil && strings.TrimSpace(string(own)) != "" {
		runnerNamespace = strings.TrimSpace(string(own))
	}

	namespace := strings.TrimSpace(getenv(tenantNamespaceEnv))
	if namespace == "" {
		namespace = defaultTenantNamespace
	}

	namespaces := []siteNamespace{{name: kubeSystemNamespace, source: "kubeadm"}}
	if raw := strings.TrimSpace(getenv(tenantNamespacesEnv)); raw != "" {
		names := splitList(raw, ",")
		namespaces = make([]siteNamespace, 0, len(names))
		for _, name := range names {
			namespaces = append(namespaces, siteNamespace{name: name, source: tenantNamespacesEnv})
		}
		if len(namespaces) == 0 {
			return tenantValidateSettings{}, fmt.Errorf("%s=%q names no namespace", tenantNamespacesEnv, raw)
		}
	}

	image := probeImage{ref: cmp.Or(strings.TrimSpace(getenv(tenantProbeImageEnv)), kube.KubectlImage)}
	for _, name := range splitList(getenv(tenantProbePullSecretsEnv), ",") {
		image.pullSecrets = append(image.pullSecrets, corev1.LocalObjectReference{Name: name})
	}

	var gpuNodes *int
	if raw := strings.TrimSpace(getenv(tenantGPUNodesEnv)); raw != "" {
		count, countErr := strconv.Atoi(raw)
		if countErr != nil || count < 0 {
			return tenantValidateSettings{}, fmt.Errorf("%s=%q is not a count of nodes", tenantGPUNodesEnv, raw)
		}
		gpuNodes = &count
	}

	gpuImage := strings.TrimSpace(getenv(tenantGPUImageEnv))
	if gpuImage == "" {
		gpuImage = defaultTenantGPUImage
	}

	expect, err := siteExpectationsFromEnv(getenv, tenantPrefix)
	if err != nil {
		return tenantValidateSettings{}, err
	}
	if _, err := parseRequireEnv(keepOnFailureEnv, getenv(keepOnFailureEnv)); err != nil {
		return tenantValidateSettings{}, err
	}
	_, certManager := expect.components[componentCertManager]

	return tenantValidateSettings{
		enabled:           enabled,
		cluster:           cluster,
		tokenSecret:       tokenSecret,
		runnerNamespace:   runnerNamespace,
		kubernetesVersion: strings.TrimSpace(getenv(tenantKubernetesVersionEnv)),
		gpuNodes:          gpuNodes,
		gpuImage:          gpuImage,
		checks: siteChecks{
			namespaces:       namespaces,
			probeNamespace:   namespace,
			probeImage:       image,
			excusedNamespace: namespace,
			tenant:           true,
			certManager:      certManager,
		},
		expect: expect,
	}, nil
}

// tenantTarget is the tenant as found: the controller's record of it, the
// controller API that holds the record, and a config that reaches it.
type tenantTarget struct {
	record     api.Cluster
	controller *nvke.Client
	restConfig *rest.Config
}

// describe lists the settings the run resolved, for the report. It names the
// credential's Secret, never its contents.
func (s tenantValidateSettings) describe() []string {
	lines := []string{
		tenantClusterEnv + "=" + s.cluster,
		"credential Secret: " + s.runnerNamespace + "/" + s.tokenSecret,
	}
	if s.kubernetesVersion != "" {
		lines = append(lines, tenantKubernetesVersionEnv+"="+s.kubernetesVersion)
	}
	if s.gpuNodes != nil {
		lines = append(lines, fmt.Sprintf("%s=%d", tenantGPUNodesEnv, *s.gpuNodes), "GPU image: "+s.gpuImage)
	}
	lines = append(lines, s.checks.describe()...)
	return append(lines, s.expect.describe()...)
}

// describe lists the tenant as found, for the report: what the controller
// records and where the suite reaches it.
func (t *tenantTarget) describe() []string {
	return []string{
		"cluster ID: " + t.record.ID.String(),
		"API server: " + t.restConfig.Host,
		"recorded Kubernetes version: " + t.record.ControlPlaneKubernetesVersion,
		fmt.Sprintf("recorded etcd encryption: %t", t.record.EtcdEncrypted),
		"recorded control-plane VIP: " + ptr.Deref(t.record.ControlPlaneVIP, "none"),
	}
}

// connectTenant finds the tenant by name through the controller API and
// builds its config from the runner's Secret.
func connectTenant(ctx context.Context, settings tenantValidateSettings) (*tenantTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, tenantConnect)
	defer cancel()
	siteConfig, err := kube.RESTConfigWithInClusterFallback("", "")
	if err != nil {
		return nil, err
	}
	controller, err := nvke.NewClient(ctx, siteConfig, nvke.Options{Poll: sitePoll})
	if err != nil {
		return nil, err
	}
	clusters, err := controller.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	record, err := clusterNamed(clusters, settings.cluster)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(siteConfig)
	if err != nil {
		return nil, err
	}
	secret, err := clientset.CoreV1().Secrets(settings.runnerNamespace).Get(ctx, settings.tokenSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the tenant credential %s/%s (%s): %w", settings.runnerNamespace, settings.tokenSecret, tenantTokenSecretEnv, err)
	}
	restConfig, err := tenantRESTConfig(record, secret)
	if err != nil {
		return nil, err
	}
	return &tenantTarget{record: record, controller: controller, restConfig: restConfig}, nil
}

func clusterNamed(clusters []api.Cluster, name string) (api.Cluster, error) {
	var found []api.Cluster
	for _, cluster := range clusters {
		if cluster.Name == name {
			found = append(found, cluster)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return api.Cluster{}, fmt.Errorf("the controller lists no cluster named %q (%s)", name, tenantClusterEnv)
	default:
		return api.Cluster{}, fmt.Errorf("the controller lists %d clusters named %q", len(found), name)
	}
}

// tenantRESTConfig is a config that presents the Secret's token to the
// tenant's API server, trusting the Secret's CA. The server is the Secret's
// where it names one, else the control-plane VIP the controller records: a
// tenant without a VIP serves at a control-plane node's address, which only
// the runner knows.
func tenantRESTConfig(record api.Cluster, secret *corev1.Secret) (*rest.Config, error) {
	where := secret.Namespace + "/" + secret.Name
	token := strings.TrimSpace(string(secret.Data[tenantTokenKey]))
	if token == "" {
		return nil, fmt.Errorf("Secret %s has no %q", where, tenantTokenKey)
	}
	ca := secret.Data[tenantCAKey]
	if len(ca) == 0 {
		return nil, fmt.Errorf("Secret %s has no %q", where, tenantCAKey)
	}
	server := strings.TrimSpace(string(secret.Data[tenantServerKey]))
	if server == "" {
		vip := strings.TrimSpace(ptr.Deref(record.ControlPlaneVIP, ""))
		if vip == "" {
			return nil, fmt.Errorf("cluster %s has no control-plane VIP, so Secret %s must name its API server under %q", record.Name, where, tenantServerKey)
		}
		server = "https://" + net.JoinHostPort(vip, tenantAPIServerPort)
	}
	return &rest.Config{
		Host:            server,
		BearerToken:     token,
		TLSClientConfig: rest.TLSClientConfig{CAData: ca},
	}, nil
}

// tenantExpectations adds what the controller records for the tenant to
// what the run expects: encryption at rest where the record says so, and
// kube-vip where it records a VIP.
func tenantExpectations(expect siteExpectations, record api.Cluster) siteExpectations {
	expect.etcdEncrypted = expect.etcdEncrypted || record.EtcdEncrypted
	if strings.TrimSpace(ptr.Deref(record.ControlPlaneVIP, "")) != "" {
		if _, listed := expect.components[componentKubeVIP]; !listed {
			components := map[string]string{componentKubeVIP: siteComponentNamespaces[componentKubeVIP]}
			for name, namespace := range expect.components {
				components[name] = namespace
			}
			expect.components = components
		}
	}
	return expect
}

// Like the site validation, every check runs whatever the others found.
// tenantValidateEnv selects these specs in the image, and gates them
// everywhere else, as siteValidateEnv does the site's.
var _ = Describe("tenant validation", Ordered, ContinueOnFailure, Label(tenantValidateLabel), func() {
	settings, err := tenantValidateFromEnv(os.Getenv, serviceAccountNamespaceFile)
	var (
		target     *tenantTarget
		connectErr error
		connected  bool
	)
	resolve := func() *tenantTarget {
		Expect(err).NotTo(HaveOccurred(), "tenant validation configuration")
		if !settings.enabled {
			Skip(tenantValidateEnv + " is not true")
		}
		if !connected {
			connected = true
			target, connectErr = connectTenant(context.Background(), settings)
			if connectErr == nil {
				AddReportEntry("tenant validation target", strings.Join(target.describe(), "\n"))
			}
		}
		Expect(connectErr).NotTo(HaveOccurred(), "reaching tenant %s", settings.cluster)
		return target
	}
	BeforeAll(func() {
		if err == nil && settings.enabled {
			AddReportEntry("tenant validation inputs", strings.Join(settings.describe(), "\n"))
		}
	})
	validateTenant(resolve, settings)
})

// validateTenant runs every tenant check against the tenant resolve finds:
// the image's, named by the run, or the one a lane has just built.
func validateTenant(resolve func() *tenantTarget, settings tenantValidateSettings) {
	checks := settings.checks
	checks.restConfig = func() (*rest.Config, error) { return resolve().restConfig, nil }
	checks.declaredTaints = func(ctx context.Context) (map[string][]corev1.Taint, error) {
		target := resolve()
		pools, err := target.controller.ListNodePools(ctx, target.record.ID)
		if err != nil {
			return nil, err
		}
		declared := map[string][]corev1.Taint{}
		for _, pool := range pools {
			members, err := target.controller.ListNodePoolNodes(ctx, pool.ID)
			if err != nil {
				return nil, err
			}
			addPoolTaints(declared, pool, members)
		}
		return declared, nil
	}

	validateSitePostDeploy(func() (site.Cluster, site.Spec, bool) {
		version := settings.kubernetesVersion
		if version == "" {
			version = resolve().record.ControlPlaneKubernetesVersion
		}
		return site.Cluster{}, site.Spec{KubernetesVersion: version}, true
	}, checks)
	validateSiteBehavior(func() siteValidateSettings {
		return tenantBehaviorSettings(settings, checks, resolve().record)
	})
	validateTenantRecord(resolve, settings)
}

// tenantBehaviorSettings are the shared checks' settings for a tenant. The
// suite's identity there is the ServiceAccount the runner provisioned, so it
// is held to read access wherever the suite runs, unless a lane reaches the
// tenant as its admin.
func tenantBehaviorSettings(settings tenantValidateSettings, checks siteChecks, record api.Cluster) siteValidateSettings {
	return siteValidateSettings{
		enabled:       true,
		checkIdentity: !settings.adminCredential,
		checks:        checks,
		expect:        tenantExpectations(settings.expect, record),
	}
}

// laneTenantSettings are the tenant checks a lane runs on the tenant it built:
// kube-system's workloads, probe pods in default, and fail fast, since every
// minute a failed check spends is one the delete needs. Nothing on a lane
// installs a GPU driver or device plugin, so the GPU checks skip.
var laneTenantSettings = tenantValidateSettings{
	enabled:         true,
	adminCredential: true,
	gpuImage:        defaultTenantGPUImage,
	checks: siteChecks{
		namespaces:     []siteNamespace{{name: kubeSystemNamespace, source: "kubeadm"}},
		failFast:       true,
		probeNamespace: "default",
		probeImage:     probeImage{ref: kube.KubectlImage},
		tenant:         true,
	},
	expect: siteExpectations{prefix: tenantPrefix},
}

// laneTenantTarget is the tenant test built, reached as its admin through
// the lane's own controller client and relay. It skips once failed reports
// an earlier failure, for the same reason the settings fail fast.
func laneTenantTarget(test *tenantTest, failed func() bool) func() *tenantTarget {
	var target *tenantTarget
	return func() *tenantTarget {
		if failed() {
			Skip("an earlier check on this tenant failed")
		}
		if target == nil {
			ctx, cancel := context.WithTimeout(context.Background(), tenantConnect)
			defer cancel()
			restConfig := expectSucceed(test.tenantClients.AdminRESTConfig(ctx, test.cluster.Name))
			target = &tenantTarget{record: *test.cluster, controller: test.nvke, restConfig: restConfig}
			AddReportEntry("tenant validation target", strings.Join(target.describe(), "\n"))
		}
		return target
	}
}

// validateTenantRecord asserts that the tenant is what the controller records
// it to be, and that its GPUs work.
func validateTenantRecord(resolve func() *tenantTarget, settings tenantValidateSettings) {
	var (
		target    *tenantTarget
		clientset kubernetes.Interface
		c         client.Client
	)
	connect := func() {
		target = resolve()
		if c == nil {
			clientset = expectSucceed(kubernetes.NewForConfig(target.restConfig))
			c = expectSucceed(siteBehaviorClient(target.restConfig))
		}
	}

	It("has the nodes the controller records for it, each joined, Ready and running the node agent", func(ctx SpecContext) {
		connect()
		Eventually(func(ctx context.Context) error {
			recorded, err := target.controller.ListClusterNodes(ctx, target.record.ID)
			if err != nil {
				return err
			}
			var nodes corev1.NodeList
			if err := c.List(ctx, &nodes); err != nil {
				return err
			}
			return joinProblems("nodes", recordedNodeProblems(recorded, nodes.Items))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("tenant", "readiness"), NodeTimeout(siteCheck+time.Minute))

	It("carries each node pool's labels and taints on its nodes", func(ctx SpecContext) {
		connect()
		pools := expectSucceed(target.controller.ListNodePools(ctx, target.record.ID))
		var nodes corev1.NodeList
		Expect(c.List(ctx, &nodes)).To(Succeed())
		problems := make([]string, 0, len(pools))
		for _, pool := range pools {
			members := expectSucceed(target.controller.ListNodePoolNodes(ctx, pool.ID))
			problems = append(problems, nodePoolMetadataProblems(pool, members, nodes.Items)...)
		}
		Expect(joinProblems("node pool nodes", problems)).To(Succeed())
	}, Label("tenant"), NodeTimeout(2*time.Minute))

	It("reserves each node pool's memory on its nodes", func(ctx SpecContext) {
		connect()
		pools := expectSucceed(target.controller.ListNodePools(ctx, target.record.ID))
		var nodes corev1.NodeList
		Expect(c.List(ctx, &nodes)).To(Succeed())
		checked := 0
		var problems []string
		for _, pool := range pools {
			want, ok, err := poolReservedMemory(pool)
			Expect(err).NotTo(HaveOccurred())
			if !ok {
				continue
			}
			checked++
			members := expectSucceed(target.controller.ListNodePoolNodes(ctx, pool.ID))
			problems = append(problems, reservedMemoryProblems(poolMembers(members, nodes.Items), want)...)
		}
		if checked == 0 {
			Skip("no node pool reserves memory")
		}
		Expect(joinProblems("nodes", problems)).To(Succeed())
	}, Label("tenant"), NodeTimeout(2*time.Minute))

	It("runs NodeLocal DNS on every Ready node", func(ctx SpecContext) {
		connect()
		Eventually(func(ctx context.Context) error {
			var nodes corev1.NodeList
			if err := c.List(ctx, &nodes); err != nil {
				return err
			}
			ready := 0
			for _, node := range nodes.Items {
				if nodeReady(node) {
					ready++
				}
			}
			return assertNodeLocalDNSAvailable(ctx, c, ready)
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("tenant", "readiness"), NodeTimeout(siteCheck+time.Minute))

	It("has as many nodes advertising GPUs as the run expects", func(ctx SpecContext) {
		connect()
		if settings.gpuNodes == nil {
			Skip(tenantGPUNodesEnv + " is unset")
		}
		Eventually(func(ctx context.Context) error {
			var nodes corev1.NodeList
			if err := c.List(ctx, &nodes); err != nil {
				return err
			}
			return joinProblems("GPU nodes", gpuNodeProblems(nodes.Items, *settings.gpuNodes))
		}).WithContext(ctx).WithTimeout(siteCheck).WithPolling(sitePoll).Should(Succeed())
	}, Label("tenant", "gpu"), NodeTimeout(siteCheck+time.Minute))

	It("runs a GPU workload to completion", func(ctx SpecContext) {
		connect()
		if settings.gpuNodes == nil || *settings.gpuNodes == 0 {
			Skip(tenantGPUNodesEnv + " expects no GPU node")
		}
		image := probeImage{ref: settings.gpuImage, pullSecrets: settings.checks.probeImage.pullSecrets}
		pod := gpuPod(settings.checks.probeNamespace, image, map[string]string{siteRunLabel: utilrand.String(6)})
		_, message, err := kube.RunPodWithLogs(ctx, c, clientset, pod, gpuContainer, siteCreateTimeout)
		deleteOnExit(ctx, c, pod)
		Expect(err).NotTo(HaveOccurred())
		Expect(message).To(ContainSubstring("GPU "), "nvidia-smi -L listed no GPU")
		GinkgoLogr.Info("GPU workload", "node", pod.Spec.NodeName, "result", strings.TrimSpace(message))
	}, Label("tenant", "gpu", "creates"), NodeTimeout(siteCreateTimeout+time.Minute))
}

// recordedNodeProblems requires the controller's nodes and the cluster's to
// be the same set, each in the same role, and every recorded node joined,
// Ready and running a healthy node agent, as the kubelet agrees. A node the
// controller is deleting is left out on both sides.
func recordedNodeProblems(recorded []api.Node, nodes []corev1.Node) []string {
	if len(recorded) == 0 {
		return []string{"the controller records no node for the cluster"}
	}
	byName := map[string]corev1.Node{}
	for _, node := range nodes {
		byName[node.Name] = node
	}
	deleting := map[string]bool{}
	var problems []string
	for _, record := range recorded {
		if record.Conditions.Deleting.Status == api.ConditionStatusTrue || record.Conditions.DeleteRequested.Status == api.ConditionStatusTrue {
			deleting[record.Name] = true
			continue
		}
		node, ok := byName[record.Name]
		if !ok {
			problems = append(problems, record.Name+": recorded by the controller, not in the cluster")
			continue
		}
		_, labeled := node.Labels[controlPlaneTaint]
		if labeled != record.ControlPlane {
			problems = append(problems, fmt.Sprintf("%s: the controller records control plane %t, the node is labeled %t", record.Name, record.ControlPlane, labeled))
		}
		for _, condition := range []struct {
			name      string
			condition api.Condition
		}{
			{"joined", record.Conditions.Joined},
			{"kubernetesReady", record.Conditions.KubernetesReady},
			{"nodeAgentReady", record.Conditions.NodeAgentReady},
		} {
			if condition.condition.Status != api.ConditionStatusTrue {
				problems = append(problems, fmt.Sprintf("%s: controller condition %s is %s: %s", record.Name, condition.name, condition.condition.Status, condition.condition.Message))
			}
		}
		if !nodeReady(node) {
			problems = append(problems, record.Name+": not Ready")
		}
	}
	for _, node := range nodes {
		if deleting[node.Name] {
			continue
		}
		if !slices.ContainsFunc(recorded, func(record api.Node) bool { return record.Name == node.Name }) {
			problems = append(problems, node.Name+": in the cluster, not recorded by the controller")
		}
	}
	slices.Sort(problems)
	return problems
}

// poolMembers returns the cluster's nodes the controller records in a pool.
func poolMembers(members []api.Node, nodes []corev1.Node) []corev1.Node {
	var found []corev1.Node
	for _, node := range nodes {
		if slices.ContainsFunc(members, func(member api.Node) bool { return member.Name == node.Name }) {
			found = append(found, node)
		}
	}
	return found
}

// addPoolTaints records the taints pool declares against each of its members.
func addPoolTaints(declared map[string][]corev1.Taint, pool api.NodePool, members []api.Node) {
	for _, member := range members {
		for _, taint := range pool.NodeTaints {
			declared[member.Name] = append(declared[member.Name], corev1.Taint{Key: taint.Key, Value: taint.Value, Effect: corev1.TaintEffect(taint.Effect)})
		}
	}
}

// nodePoolMetadataProblems requires every node the controller records in pool
// to carry the pool's labels and taints.
func nodePoolMetadataProblems(pool api.NodePool, members []api.Node, nodes []corev1.Node) []string {
	found := poolMembers(members, nodes)
	var problems []string
	for _, member := range members {
		if !slices.ContainsFunc(found, func(node corev1.Node) bool { return node.Name == member.Name }) {
			problems = append(problems, fmt.Sprintf("pool %s: node %s is not in the cluster", pool.Name, member.Name))
		}
	}
	for _, node := range found {
		for key, value := range pool.NodeLabels {
			if got, ok := node.Labels[key]; !ok || got != value {
				problems = append(problems, fmt.Sprintf("pool %s: node %s: label %s=%q, want %q", pool.Name, node.Name, key, got, value))
			}
		}
		for _, taint := range pool.NodeTaints {
			has := slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool {
				return t.Key == taint.Key && t.Value == taint.Value && string(t.Effect) == string(taint.Effect)
			})
			if !has {
				problems = append(problems, fmt.Sprintf("pool %s: node %s: no taint %s=%s:%s", pool.Name, node.Name, taint.Key, taint.Value, taint.Effect))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

// poolReservedMemory is the memory a pool's nodes hold back from pods: its
// system and kube reservations together. ok is false where it sets neither.
func poolReservedMemory(pool api.NodePool) (resource.Quantity, bool, error) {
	total := resource.Quantity{}
	ok := false
	for _, raw := range []*string{pool.SystemReservedMemory, pool.KubeReservedMemory} {
		if raw == nil || strings.TrimSpace(*raw) == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(strings.TrimSpace(*raw))
		if err != nil {
			return resource.Quantity{}, false, fmt.Errorf("pool %s reserves %q: %w", pool.Name, *raw, err)
		}
		total.Add(quantity)
		ok = true
	}
	return total, ok, nil
}

// gpuNodeProblems requires want nodes to advertise allocatable GPUs. A node
// labeled as having GPUs that advertises none has a device plugin that is
// not working, and counts against the cluster whatever the total.
func gpuNodeProblems(nodes []corev1.Node, want int) []string {
	var (
		problems   []string
		advertised int
	)
	for _, node := range nodes {
		quantity, ok := node.Status.Allocatable[gpuResource]
		if ok && !quantity.IsZero() {
			advertised++
			continue
		}
		if node.Labels["nvidia.com/gpu.present"] == "true" {
			problems = append(problems, node.Name+": labeled nvidia.com/gpu.present but advertises no allocatable "+string(gpuResource))
		}
	}
	if advertised != want {
		problems = append(problems, fmt.Sprintf("%d nodes advertise %s, want %d", advertised, gpuResource, want))
	}
	return problems
}

// gpuPod lists the GPU it is given. It tolerates every taint, since GPU nodes
// are commonly tainted to keep other work off them, and runs as the probe
// pods do, since it shares their namespace.
func gpuPod(namespace string, image probeImage, labels map[string]string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "nvke-validate-gpu-", Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    gpuContainer,
				Command: []string{"sh", "-ec", "nvidia-smi -L | tee /dev/termination-log"},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{gpuResource: resource.MustParse("1")},
				},
			}},
		},
	}
	image.apply(&pod.Spec)
	return pod
}
