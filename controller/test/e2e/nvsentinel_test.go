// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

// NVSentinel (https://github.com/NVIDIA/nvsentinel) on Mokka's simulated GPUs.
// The versions match hack/mokka-demo/nvsentinel/run.sh; change them together.
const (
	nvsentinelChart      = "oci://ghcr.io/nvidia/nvsentinel"
	nvsentinelVersion    = "v1.21.0"
	nvsentinelNamespace  = "nvsentinel"
	certManagerChart     = "oci://quay.io/jetstack/charts/cert-manager"
	certManagerVersion   = "v1.19.1"
	certManagerNamespace = "cert-manager"
	// nvsentinelWait bounds the datastore, which comes up in stages: the Percona
	// operator, the replica set, its certificates and a collection-setup Job.
	nvsentinelWait = 15 * time.Minute
	// faultWait bounds each step of the fault: the cordon, the drain, the uncordon.
	faultWait = 10 * time.Minute
	// hotGPUTemp is above every Mokka profile's slowdown threshold (h100 87 C,
	// gb300 90 C). At the threshold the margin is 0, which the watch accepts.
	hotGPUTemp = "142"
	sampleApp  = "nvsentinel-sample"
)

//go:embed testdata/nvsentinel-values.yaml
var nvsentinelValues []byte

// capdNVSentinelEntry is the "NVKE with backend" entry for NVSentinel. Only its
// own scheduled lane selects the "nvsentinel" label: the datastore and fault
// cycle take too long for the pull-request and full CAPD runs.
var capdNVSentinelEntry = Entry("CAPD NVSentinel", Ordered, ContinueOnFailure, Label("nvsentinel"), testSpec{
	buildSpec: capdSpec,
	shape: clusterShape{
		controlPlaneConfig: &nvke.ControlPlaneConfigSpec{DesiredCount: 1},
		workerPools: []nvke.NodePoolSpec{
			{Name: "mokka-h100", NodeNamePrefix: "mh", DesiredNodeCount: 2, NodeLabels: map[string]string{mokkaLabelKey: "h100"}},
		},
	},
	options:  nvke.Options{Provision: 10 * time.Minute, Delete: 5 * time.Minute, Poll: 5 * time.Second},
	validate: validations{provisioningOnly: true, mokkaGPU: true, nvsentinel: true},
})

// validateNVSentinel installs NVSentinel on the nodes validateMokkaGPU gave
// simulated GPUs, overheats one GPU, and asserts NVSentinel cordons its node,
// drains a GPU workload to another node, and uncordons it once the GPU cools.
func validateNVSentinel(test *tenantTest) {
	var (
		c          client.Client
		clientset  kubernetes.Interface
		restConfig *rest.Config
	)
	connect := func(ctx context.Context) {
		GinkgoHelper()
		if c != nil {
			return
		}
		restConfig = expectSucceed(test.tenantClients.AdminRESTConfig(ctx, test.cluster.Name))
		clientset = expectSucceed(kubernetes.NewForConfig(restConfig))
		c = expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
	}
	ready := func(ctx context.Context, namespace string, timeout time.Duration) {
		GinkgoHelper()
		Eventually(func(ctx context.Context) error {
			var pods corev1.PodList
			if err := c.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
				return err
			}
			if len(pods.Items) == 0 {
				return fmt.Errorf("no pods in %s yet", namespace)
			}
			var jobs batchv1.JobList
			if err := c.List(ctx, &jobs, client.InNamespace(namespace)); err != nil {
				return err
			}
			return joinProblems(namespace+" pods", podProblems(pods.Items, jobs.Items))
		}).WithContext(ctx).WithTimeout(timeout).WithPolling(sitePoll).Should(Succeed())
	}

	It("turns on GPU Operator's standalone DCGM, which NVSentinel polls", func(ctx SpecContext) {
		connect(ctx)
		Expect(helmOnTenant(ctx, test, "upgrade", "gpu-operator", "gpu-operator", "--repo", gpuOperatorRepo, "--version", gpuOperatorVersion,
			"--namespace", gpuOperatorNamespace, "--reuse-values", "--set", "dcgm.enabled=true",
		)).To(Succeed())
		Eventually(func(ctx context.Context) error {
			return c.Get(ctx, client.ObjectKey{Namespace: gpuOperatorNamespace, Name: "nvidia-dcgm"}, &corev1.Service{})
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(sitePoll).Should(Succeed())
	}, NodeTimeout(helmRun+mokkaWait+time.Minute))

	It("installs cert-manager and NVSentinel, whose pods become Ready", func(ctx SpecContext) {
		connect(ctx)
		Expect(helmOnTenant(ctx, test, "upgrade", "--install", "cert-manager", certManagerChart, "--version", certManagerVersion,
			"--namespace", certManagerNamespace, "--create-namespace", "--set", "crds.enabled=true",
		)).To(Succeed())
		ready(ctx, certManagerNamespace, mokkaWait)

		values := filepath.Join(GinkgoT().TempDir(), "values.yaml")
		Expect(os.WriteFile(values, nvsentinelValues, 0o600)).To(Succeed())
		// Helm creates the chart's Percona CRDs first, but the API server can serve them a
		// moment late, failing the install before it creates anything.
		Eventually(func(ctx context.Context) error {
			return helmOnTenant(ctx, test, "upgrade", "--install", "nvsentinel", nvsentinelChart, "--version", nvsentinelVersion,
				"--namespace", nvsentinelNamespace, "--create-namespace", "--values", values,
			)
		}).WithContext(ctx).WithTimeout(helmRun).WithPolling(sitePoll).Should(Succeed())
		ready(ctx, nvsentinelNamespace, nvsentinelWait)
	}, NodeTimeout(3*helmRun+mokkaWait+nvsentinelWait+time.Minute))

	It("cordons and drains a node whose GPU overheats, and uncordons it once the GPU cools", func(ctx SpecContext) {
		connect(ctx)
		workload := nvsentinelWorkload()
		Expect(c.Create(ctx, workload)).To(Succeed())
		deleteOnExit(ctx, c, workload)
		var target string
		Eventually(func(ctx context.Context) (string, error) {
			var err error
			target, err = sampleNode(ctx, c)
			return target, err
		}).WithContext(ctx).WithTimeout(siteCreateTimeout).WithPolling(sitePoll).ShouldNot(BeEmpty())

		var pods corev1.PodList
		Expect(c.List(ctx, &pods, client.InNamespace(mokkaNamespace))).To(Succeed())
		i := slices.IndexFunc(pods.Items, func(pod corev1.Pod) bool { return pod.Spec.NodeName == target })
		Expect(i).NotTo(BeNumerically("<", 0), "no Mokka pod on %s", target)
		mock := &pods.Items[i]
		// Cool the GPU however the spec ends, so a kept or reused cluster has no node quarantined.
		DeferCleanup(func() {
			if CurrentSpecReport().Failed() && keepOnFailure() {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteCleanup)
			defer cancel()
			if _, err := kube.Exec(cleanupCtx, restConfig, clientset, mock, "nvml-mock-ctl", "reset", "--gpu", "0"); err != nil {
				GinkgoLogr.Error(err, "cleanup", "node", target)
			}
		})
		cordoned := func(ctx context.Context) (bool, error) {
			var node corev1.Node
			err := c.Get(ctx, client.ObjectKey{Name: target}, &node)
			return node.Spec.Unschedulable, err
		}

		Expect(kube.Exec(ctx, restConfig, clientset, mock, "nvml-mock-ctl", "temp", "--gpu", "0", hotGPUTemp)).Error().NotTo(HaveOccurred())
		Eventually(cordoned).WithContext(ctx).WithTimeout(faultWait).WithPolling(sitePoll).Should(BeTrue(), "NVSentinel did not cordon %s", target)
		Eventually(sampleNode).WithContext(ctx).WithArguments(c).WithTimeout(faultWait).WithPolling(sitePoll).
			Should(And(Not(BeEmpty()), Not(Equal(target))), "the workload was not drained off %s", target)

		Expect(kube.Exec(ctx, restConfig, clientset, mock, "nvml-mock-ctl", "reset", "--gpu", "0")).Error().NotTo(HaveOccurred())
		Eventually(cordoned).WithContext(ctx).WithTimeout(faultWait).WithPolling(sitePoll).Should(BeFalse(), "NVSentinel did not uncordon %s", target)
	}, NodeTimeout(siteCreateTimeout+3*faultWait+time.Minute))
}

// nvsentinelWorkload is a GPU workload for NVSentinel's drainer to move. A
// Deployment brings a drained pod back on another GPU node.
func nvsentinelWorkload() *appsv1.Deployment {
	labels := map[string]string{"app": sampleApp}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: sampleApp, Namespace: corev1.NamespaceDefault},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"nvidia.com/gpu.present": "true"},
					Containers: []corev1.Container{{
						Name:      "sleeper",
						Image:     "registry.k8s.io/pause:3.10",
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{gpuResource: resource.MustParse("1")}},
					}},
				},
			},
		},
	}
}

// sampleNode is the node the sample workload runs on, or "" while it runs nowhere.
func sampleNode(ctx context.Context, c client.Client) (string, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(corev1.NamespaceDefault), client.MatchingLabels{"app": sampleApp}); err != nil {
		return "", fmt.Errorf("listing %s pods: %w", sampleApp, err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
			return pod.Spec.NodeName, nil
		}
	}
	return "", nil
}
