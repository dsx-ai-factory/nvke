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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

// Mokka (nvml-mock) simulates what an NVIDIA driver exposes on a node, so GPU
// Operator and the pods it serves behave as they would on a GPU. The versions
// and profile match hack/mokka-demo/demo.sh's defaults; change them together.
const (
	// mokkaLabelKey selects the nodes Mokka runs on. An entry sets it on a
	// worker pool's node labels.
	mokkaLabelKey        = "mokka.example/simulated-gpu"
	mokkaChart           = "oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock"
	mokkaChartVersion    = "0.6.0"
	mokkaGPUProfile      = "gb300"
	mokkaNamespace       = "mokka"
	gpuOperatorRepo      = "https://helm.ngc.nvidia.com/nvidia"
	gpuOperatorVersion   = "v26.3.3"
	gpuOperatorNamespace = "gpu-operator"
	// mokkaWait bounds each wait for what a chart installs to come up. Most
	// of it is image pulls onto a node that has none of them.
	mokkaWait = 15 * time.Minute
	mokkaPoll = 10 * time.Second
	// helmRun bounds one helm call, which returns once the release is
	// applied: the specs wait for its workloads themselves.
	helmRun = 3 * time.Minute
)

// mokkaGPUOperatorValues turns off what Mokka replaces: the driver and the
// container toolkit. hack/mokka-demo/demo.sh installs with the same file.
//
//go:embed testdata/mokka-gpu-operator-values.yaml
var mokkaGPUOperatorValues []byte

// clusterPolicyGVK is GPU Operator's top-level resource, which reports
// "ready" once every operand it manages is.
var clusterPolicyGVK = schema.GroupVersionKind{Group: "nvidia.com", Version: "v1", Kind: "ClusterPolicy"}

// validateMokkaGPU gives the entry's labeled worker pools simulated GPUs and
// asserts the GPU stack works on them: GPU Operator becomes ready, exactly
// those nodes advertise GPUs, its validator passes on each, and a pod that
// asks for one GPU sees one.
//
// The releases are not uninstalled: they go with the cluster.
func validateMokkaGPU(test *tenantTest) {
	var (
		c         client.Client
		clientset kubernetes.Interface
		want      int
	)
	connect := func(ctx context.Context) {
		GinkgoHelper()
		if c != nil {
			return
		}
		for _, pool := range test.spec.Workers {
			if pool.NodeLabels[mokkaLabelKey] == "true" {
				want += pool.DesiredNodeCount
			}
		}
		Expect(want).To(BeNumerically(">", 0), "the entry must label a worker pool %s=true", mokkaLabelKey)
		restConfig := expectSucceed(test.tenantClients.AdminRESTConfig(ctx, test.cluster.Name))
		clientset = expectSucceed(kubernetes.NewForConfig(restConfig))
		c = expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
	}

	It("installs Mokka on the labeled nodes", func(ctx SpecContext) {
		connect(ctx)
		// A missing helm is the runner's fault, not the cluster's, but a skip
		// would leave the row green with nothing proven.
		_, err := exec.LookPath("helm")
		Expect(err).NotTo(HaveOccurred(), "the Mokka specs install charts with helm, and the runner has none")

		Expect(helmOnTenant(ctx, test, "upgrade", "--install", "nvml-mock", mokkaChart,
			"--version", mokkaChartVersion, "--namespace", mokkaNamespace, "--create-namespace",
			"--set", "gpu.profile="+mokkaGPUProfile,
			"--set-string", "nodeSelector."+strings.ReplaceAll(mokkaLabelKey, ".", `\.`)+"=true",
		)).To(Succeed())

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
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(mokkaPoll).Should(Succeed())
	}, NodeTimeout(helmRun+mokkaWait+time.Minute))

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
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(mokkaPoll).Should(Succeed())
	}, NodeTimeout(helmRun+mokkaWait+time.Minute))

	It("advertises GPUs on the labeled nodes and no others", func(ctx SpecContext) {
		connect(ctx)
		Eventually(func(ctx context.Context) error {
			var nodes corev1.NodeList
			if err := c.List(ctx, &nodes); err != nil {
				return err
			}
			problems := gpuNodeProblems(nodes.Items, want)
			for _, node := range nodes.Items {
				quantity := node.Status.Allocatable[gpuResource]
				if node.Labels[mokkaLabelKey] != "true" && !quantity.IsZero() {
					problems = append(problems, node.Name+": not labeled "+mokkaLabelKey+" but advertises "+string(gpuResource))
				}
			}
			return joinProblems("GPU nodes", problems)
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(mokkaPoll).Should(Succeed())
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
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(mokkaPoll).Should(Succeed())
	}, NodeTimeout(mokkaWait+time.Minute))

	It("runs a pod that sees exactly the one GPU it requests", func(ctx SpecContext) {
		connect(ctx)
		pod := mokkaGPUPod()
		finished, message, err := kube.RunPodWithLogs(ctx, c, clientset, pod, gpuContainer, siteCreateTimeout)
		deleteOnExit(ctx, c, pod)
		Expect(err).NotTo(HaveOccurred())
		var gpus []string
		for _, line := range strings.Split(strings.TrimSpace(message), "\n") {
			if strings.HasPrefix(line, "GPU ") {
				gpus = append(gpus, line)
			}
		}
		Expect(gpus).To(HaveLen(1), "nvidia-smi -L in a pod limited to one GPU listed:\n%s", message)
		GinkgoLogr.Info("Mokka GPU workload", "node", finished.Spec.NodeName, "result", gpus[0])
	}, NodeTimeout(siteCreateTimeout+time.Minute))
}

// helmOnTenant runs helm against the test's tenant as its admin. Each call
// mints its own kubeconfig, since the token in one is fixed when it is minted,
// and keeps helm's cache and config out of the runner's home.
func helmOnTenant(ctx context.Context, test *tenantTest, args ...string) error {
	dir := GinkgoT().TempDir()
	kubeconfig, err := test.tenantClients.AdminKubeconfig(ctx, test.cluster.Name)
	if err != nil {
		return err
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

// mokkaNodes lists the nodes labeled for Mokka.
func mokkaNodes(ctx context.Context, c client.Client) ([]corev1.Node, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes, client.MatchingLabels{mokkaLabelKey: "true"}); err != nil {
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
		ready := false
		for i := range pods {
			if pods[i].Spec.NodeName == node.Name && podReady(&pods[i]) {
				ready = true
			}
		}
		if !ready {
			problems = append(problems, node.Name+": no Ready Mokka pod")
		}
	}
	return problems
}

// validatorProblems requires a GPU Operator validator pod on every node whose
// steps, its init containers, all exited 0. A step that ended any other way,
// such as ContainerStatusUnknown, is not a pass.
func validatorProblems(nodes []corev1.Node, pods []corev1.Pod) []string {
	var problems []string
	for _, node := range nodes {
		var validator *corev1.Pod
		for i := range pods {
			if pods[i].Spec.NodeName == node.Name {
				validator = &pods[i]
			}
		}
		if validator == nil {
			problems = append(problems, node.Name+": no validator pod")
			continue
		}
		if len(validator.Status.InitContainerStatuses) == 0 {
			problems = append(problems, node.Name+": the validator has not started its steps")
		}
		for _, step := range validator.Status.InitContainerStatuses {
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

// mokkaGPUPod lists the one GPU it requests on a labeled node. Unlike
// gpuPod it runs as the image's own user, root, as the demo does: Mokka's
// injected nvidia-smi is proven there and not as nobody.
func mokkaGPUPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "nvke-mokka-gpu-", Namespace: corev1.NamespaceDefault},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector:  map[string]string{mokkaLabelKey: "true"},
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:            gpuContainer,
				Image:           defaultTenantGPUImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sh", "-ec", "nvidia-smi -L | tee /dev/termination-log"},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{gpuResource: resource.MustParse("1")},
				},
			}},
		},
	}
}
