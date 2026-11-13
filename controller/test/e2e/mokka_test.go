// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

// Mokka (nvml-mock) simulates an NVIDIA driver on a node. The versions match
// hack/mokka-demo/demo.sh's defaults; change them together.
const (
	// mokkaLabelKey's value on a worker pool is the Mokka profile its nodes present.
	mokkaLabelKey        = "mokka.example/simulated-gpu"
	gpuProductLabel      = "nvidia.com/gpu.product"
	mokkaChart           = "oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock"
	mokkaChartVersion    = "0.6.0"
	mokkaNamespace       = "mokka"
	gpuOperatorRepo      = "https://helm.ngc.nvidia.com/nvidia"
	gpuOperatorVersion   = "v26.3.3"
	gpuOperatorNamespace = "gpu-operator"
	// mokkaWait bounds each wait for a chart's workloads; CI needs under two minutes.
	mokkaWait = 5 * time.Minute
	// helmRun bounds one helm call, which does not wait for the workloads.
	helmRun = 3 * time.Minute
)

//go:embed testdata/mokka-gpu-operator-values.yaml
var mokkaGPUOperatorValues []byte

var clusterPolicyGVK = schema.GroupVersionKind{Group: "nvidia.com", Version: "v1", Kind: "ClusterPolicy"}

// mokkaProfile is what a node running one Mokka profile advertises.
type mokkaProfile struct {
	product string // GPU Feature Discovery's nvidia.com/gpu.product
	gpus    int64
}

// mokkaProfiles are the profiles an entry may label a pool with.
var mokkaProfiles = map[string]mokkaProfile{
	"gb300": {product: "NVIDIA-GB300-NVL", gpus: 4},
	"h100":  {product: "NVIDIA-H100-80GB-HBM3", gpus: 8},
}

// validateMokkaGPU installs Mokka and GPU Operator on the labeled worker pools
// and asserts each node advertises its profile's GPU model, passes the validator,
// and that a pod selecting each model lands on that model's nodes.
// The releases go with the cluster.
func validateMokkaGPU(test *tenantTest) {
	var (
		c         client.Client
		clientset kubernetes.Interface
		want      int
		profiles  []string
	)
	connect := func(ctx context.Context) {
		GinkgoHelper()
		if c != nil {
			return
		}
		labeled := 0
		for _, pool := range test.spec.Workers {
			if profile, ok := pool.NodeLabels[mokkaLabelKey]; ok {
				Expect(mokkaProfiles).To(HaveKey(profile), "pool %s: unknown Mokka profile %q", pool.Name, profile)
				labeled += pool.DesiredNodeCount
				if !slices.Contains(profiles, profile) {
					profiles = append(profiles, profile)
				}
			}
		}
		Expect(labeled).To(BeNumerically(">", 0), "the entry must label a worker pool %s=<profile>", mokkaLabelKey)
		want = labeled
		clientset = expectSucceed(kubernetes.NewForConfig(expectSucceed(test.tenantClients.AdminRESTConfig(ctx, test.cluster.Name))))
		c = expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
	}

	It("installs Mokka on the labeled nodes", func(ctx SpecContext) {
		connect(ctx)
		for _, profile := range profiles {
			Expect(helmOnTenant(ctx, test, "upgrade", "--install", "nvml-mock-"+profile, mokkaChart,
				"--version", mokkaChartVersion, "--namespace", mokkaNamespace, "--create-namespace",
				"--set", "gpu.profile="+profile,
				"--set-string", "nodeSelector."+strings.ReplaceAll(mokkaLabelKey, ".", `\.`)+"="+profile,
			)).To(Succeed())
		}

		Eventually(func(ctx context.Context) error {
			nodes, err := mokkaNodes(ctx, c)
			if err != nil {
				return err
			}
			var pods corev1.PodList
			if err := c.List(ctx, &pods, client.InNamespace(mokkaNamespace)); err != nil {
				return err
			}
			return joinProblems("Mokka", mokkaPodProblems(nodes, pods.Items, want))
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(sitePoll).Should(Succeed())
	}, NodeTimeout(time.Duration(len(mokkaProfiles))*helmRun+mokkaWait+time.Minute))

	It("installs GPU Operator and its ClusterPolicy becomes ready", func(ctx SpecContext) {
		connect(ctx)
		values := filepath.Join(GinkgoT().TempDir(), "values.yaml")
		Expect(os.WriteFile(values, mokkaGPUOperatorValues, 0o600)).To(Succeed())
		Expect(helmOnTenant(ctx, test, "upgrade", "--install", "gpu-operator", "gpu-operator",
			"--repo", gpuOperatorRepo, "--version", gpuOperatorVersion,
			"--namespace", gpuOperatorNamespace, "--create-namespace", "--values", values,
		)).To(Succeed())

		Eventually(func(ctx context.Context) error {
			policy := &unstructured.Unstructured{}
			policy.SetGroupVersionKind(clusterPolicyGVK)
			if err := c.Get(ctx, client.ObjectKey{Name: "cluster-policy"}, policy); err != nil {
				return err
			}
			state, _, _ := unstructured.NestedString(policy.Object, "status", "state")
			if state != "ready" {
				return fmt.Errorf("ClusterPolicy state is %q, want ready", state)
			}
			return nil
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(sitePoll).Should(Succeed())
	}, NodeTimeout(helmRun+mokkaWait+time.Minute))

	It("advertises each profile's GPU model and count on the labeled nodes", func(ctx SpecContext) {
		connect(ctx)
		Eventually(func(ctx context.Context) error {
			var nodes corev1.NodeList
			if err := c.List(ctx, &nodes); err != nil {
				return err
			}
			labeled, err := mokkaNodes(ctx, c)
			if err != nil {
				return err
			}
			return joinProblems("GPU nodes", append(gpuNodeProblems(nodes.Items, want), mokkaModelProblems(labeled)...))
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(sitePoll).Should(Succeed())
	}, NodeTimeout(mokkaWait+time.Minute))

	It("passes the GPU Operator validator on every labeled node", func(ctx SpecContext) {
		connect(ctx)
		Eventually(func(ctx context.Context) error {
			nodes, err := mokkaNodes(ctx, c)
			if err != nil {
				return err
			}
			var pods corev1.PodList
			if err := c.List(ctx, &pods, client.InNamespace(gpuOperatorNamespace), client.MatchingLabels{"app": "nvidia-operator-validator"}); err != nil {
				return err
			}
			return joinProblems("GPU Operator validator", validatorProblems(nodes, pods.Items))
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(sitePoll).Should(Succeed())
	}, NodeTimeout(mokkaWait+time.Minute))

	It("runs a pod per GPU model on that model's nodes, seeing the one GPU it requests", func(ctx SpecContext) {
		connect(ctx)
		for _, profile := range profiles {
			model := mokkaProfiles[profile].product
			pod := gpuPod(corev1.NamespaceDefault, probeImage{ref: defaultTenantGPUImage}, nil)
			pod.Spec.NodeSelector = map[string]string{gpuProductLabel: model}
			finished, message, err := kube.RunPodWithLogs(ctx, c, clientset, pod, gpuContainer, siteCreateTimeout)
			deleteOnExit(ctx, c, pod)
			Expect(err).NotTo(HaveOccurred())
			var node corev1.Node
			Expect(c.Get(ctx, client.ObjectKey{Name: finished.Spec.NodeName}, &node)).To(Succeed())
			Expect(node.Labels).To(HaveKeyWithValue(mokkaLabelKey, profile), "a pod selecting %s ran on %s", model, node.Name)
			var gpus []string
			for _, line := range strings.Split(strings.TrimSpace(message), "\n") {
				if strings.HasPrefix(line, "GPU ") {
					gpus = append(gpus, line)
				}
			}
			Expect(gpus).To(HaveLen(1), "nvidia-smi -L in a pod limited to one GPU listed:\n%s", message)
			// GPU Feature Discovery writes the model nvidia-smi reports with dashes for spaces.
			Expect(gpus[0]).To(ContainSubstring(strings.ReplaceAll(model, "-", " ")))
			GinkgoLogr.Info("Mokka GPU workload", "node", node.Name, "result", gpus[0])
		}
	}, NodeTimeout(time.Duration(len(mokkaProfiles))*siteCreateTimeout+time.Minute))
}

// helmOnTenant runs helm against the test's tenant as its admin, with a freshly
// minted kubeconfig and helm's cache and config kept out of the runner's home.
func helmOnTenant(ctx context.Context, test *tenantTest, args ...string) error {
	dir := GinkgoT().TempDir()
	kubeconfig, err := test.tenantClients.AdminKubeconfig(ctx, test.cluster.Name)
	if err != nil {
		return fmt.Errorf("minting the tenant kubeconfig: %w", err)
	}
	path := filepath.Join(dir, "kubeconfig")
	if err = os.WriteFile(path, kubeconfig, 0o600); err != nil {
		return fmt.Errorf("writing the tenant kubeconfig: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, helmRun)
	defer cancel()
	command := exec.CommandContext(ctx, "helm", args...)
	command.Env = append(os.Environ(),
		"KUBECONFIG="+path,
		"HELM_CACHE_HOME="+filepath.Join(dir, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(dir, "config"),
		"HELM_DATA_HOME="+filepath.Join(dir, "data"),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	return nil
}

// mokkaNodes lists the nodes labeled for Mokka, with any profile.
func mokkaNodes(ctx context.Context, c client.Client) ([]corev1.Node, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.HasLabels{mokkaLabelKey}); err != nil {
		return nil, fmt.Errorf("listing nodes labeled %s: %w", mokkaLabelKey, err)
	}
	return nodes.Items, nil
}

// mokkaPodProblems requires want labeled nodes, each running a Ready Mokka pod.
func mokkaPodProblems(nodes []corev1.Node, pods []corev1.Pod, want int) []string {
	var problems []string
	if len(nodes) != want {
		problems = append(problems, fmt.Sprintf("%d nodes are labeled %s, want %d", len(nodes), mokkaLabelKey, want))
	}
	for _, node := range nodes {
		if !slices.ContainsFunc(pods, func(pod corev1.Pod) bool { return pod.Spec.NodeName == node.Name && podReady(&pod) }) {
			problems = append(problems, node.Name+": no Ready Mokka pod")
		}
	}
	return problems
}

// mokkaModelProblems requires each of the nodes labeled for Mokka to advertise
// its profile's GPU model and count.
func mokkaModelProblems(nodes []corev1.Node) []string {
	var problems []string
	for _, node := range nodes {
		profile := node.Labels[mokkaLabelKey]
		want := mokkaProfiles[profile]
		count := node.Status.Allocatable[gpuResource]
		if node.Labels[gpuProductLabel] != want.product || count.Value() != want.gpus {
			problems = append(problems, fmt.Sprintf("%s (%s): %q x%d, want %q x%d",
				node.Name, profile, node.Labels[gpuProductLabel], count.Value(), want.product, want.gpus))
		}
	}
	return problems
}

// validatorProblems requires a GPU Operator validator pod on every node whose
// steps, its init containers, all exited 0. A pod being deleted does not count.
func validatorProblems(nodes []corev1.Node, pods []corev1.Pod) []string {
	var problems []string
	for _, node := range nodes {
		i := slices.IndexFunc(pods, func(pod corev1.Pod) bool { return pod.Spec.NodeName == node.Name && pod.DeletionTimestamp == nil })
		if i < 0 {
			problems = append(problems, node.Name+": no validator pod")
			continue
		}
		steps := pods[i].Status.InitContainerStatuses
		if len(steps) == 0 {
			problems = append(problems, node.Name+": the validator has not started its steps")
		}
		for _, step := range steps {
			switch terminated := step.State.Terminated; {
			case terminated == nil:
				problems = append(problems, fmt.Sprintf("%s: validator step %s has not finished", node.Name, step.Name))
			case terminated.ExitCode != 0:
				problems = append(problems, fmt.Sprintf("%s: validator step %s exited %d: %s", node.Name, step.Name, terminated.ExitCode, terminated.Reason))
			}
		}
	}
	return problems
}
