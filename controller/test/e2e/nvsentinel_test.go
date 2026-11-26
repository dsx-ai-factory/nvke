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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	// nvsentinelGuideValues are Mokka's NVSentinel guide values, which
	// testdata/nvsentinel-values.yaml is layered over.
	nvsentinelGuideValues = "https://raw.githubusercontent.com/NVIDIA/k8s-test-infra/797b96a8a57f1285cf18a15d9cbf67cbbb782c14/docs/guides/nv-sentinel/nvsentinel-values.yaml"
	// nvsentinelWait bounds helm's wait for the datastore, which comes up in stages:
	// the Percona operator, the replica set, its certificates and a collection-setup Job.
	nvsentinelWait = 15 * time.Minute
	// faultWait bounds each step of the fault: the cordon, the drain, the uncordon.
	faultWait = 10 * time.Minute
	// hotGPUTemp is above every Mokka profile's slowdown threshold (h100 87 C,
	// gb300 90 C). At the threshold the margin is 0, which the watch accepts.
	hotGPUTemp = "142"
	// sampleApp names the sample workload and its namespace, the only one the
	// override in testdata/nvsentinel-values.yaml lets NVSentinel drain.
	sampleApp = "nvsentinel-sample"
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
	var c client.Client
	connect := func(ctx context.Context) {
		GinkgoHelper()
		if c == nil {
			c = expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
		}
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

	// helm --wait --wait-for-jobs holds each install until its workloads are ready and its Jobs complete.
	It("installs cert-manager and NVSentinel, whose workloads become ready", func(ctx SpecContext) {
		Expect(runOnTenant(ctx, test, mokkaWait+time.Minute, "helm", "upgrade", "--install", "cert-manager", certManagerChart,
			"--version", certManagerVersion, "--namespace", certManagerNamespace, "--create-namespace", "--set", "crds.enabled=true",
			"--wait", "--wait-for-jobs", "--timeout", mokkaWait.String(),
		)).To(Succeed())

		values := filepath.Join(GinkgoT().TempDir(), "values.yaml")
		Expect(os.WriteFile(values, nvsentinelValues, 0o600)).To(Succeed())
		// Helm creates the chart's Percona CRDs first, but the API server can serve them a
		// moment late, failing the install before it creates anything.
		Eventually(func(ctx context.Context) error {
			return runOnTenant(ctx, test, nvsentinelWait+time.Minute, "helm", "upgrade", "--install", "nvsentinel", nvsentinelChart,
				"--version", nvsentinelVersion, "--namespace", nvsentinelNamespace, "--create-namespace",
				"--values", nvsentinelGuideValues, "--values", values, "--wait", "--wait-for-jobs", "--timeout", nvsentinelWait.String(),
			)
		}).WithContext(ctx).WithTimeout(helmRun).WithPolling(sitePoll).Should(Succeed())
	}, NodeTimeout(mokkaWait+helmRun+nvsentinelWait+3*time.Minute))

	It("cordons and drains a node whose GPU overheats, and uncordons it once the GPU cools", func(ctx SpecContext) {
		connect(ctx)
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sampleApp}}
		Expect(c.Create(ctx, namespace)).To(Succeed())
		deleteOnExit(ctx, c, namespace)
		Expect(c.Create(ctx, nvsentinelWorkload())).To(Succeed())
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
		// Mokka's control CLI runs in the pod's default container.
		mockCtl := func(ctx context.Context, args ...string) error {
			return runOnTenant(ctx, test, helmRun, "kubectl", append([]string{"exec", "--namespace", mokkaNamespace, pods.Items[i].Name, "--", "nvml-mock-ctl"}, args...)...)
		}
		// Cool the GPU however the spec ends, so a kept or reused cluster has no node quarantined.
		DeferCleanup(func() {
			if CurrentSpecReport().Failed() && keepOnFailure() {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteCleanup)
			defer cancel()
			if err := mockCtl(cleanupCtx, "reset", "--gpu", "0"); err != nil {
				GinkgoLogr.Error(err, "cleanup", "node", target)
			}
		})
		cordoned := func(ctx context.Context) (bool, error) {
			var node corev1.Node
			err := c.Get(ctx, client.ObjectKey{Name: target}, &node)
			return node.Spec.Unschedulable, err
		}

		Expect(mockCtl(ctx, "temp", "--gpu", "0", hotGPUTemp)).To(Succeed())
		Eventually(cordoned).WithContext(ctx).WithTimeout(faultWait).WithPolling(sitePoll).Should(BeTrue(), "NVSentinel did not cordon %s", target)
		Eventually(sampleNode).WithContext(ctx).WithArguments(c).WithTimeout(faultWait).WithPolling(sitePoll).
			Should(And(Not(BeEmpty()), Not(Equal(target))), "the workload was not drained off %s", target)

		Expect(mockCtl(ctx, "reset", "--gpu", "0")).To(Succeed())
		Eventually(cordoned).WithContext(ctx).WithTimeout(faultWait).WithPolling(sitePoll).Should(BeFalse(), "NVSentinel did not uncordon %s", target)
	}, NodeTimeout(siteCreateTimeout+3*faultWait+time.Minute))
}

// nvsentinelWorkload is a GPU workload for NVSentinel's drainer to move. A
// Deployment brings a drained pod back on another GPU node.
func nvsentinelWorkload() *appsv1.Deployment {
	labels := map[string]string{"app": sampleApp}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: sampleApp, Namespace: sampleApp},
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
	if err := c.List(ctx, &pods, client.InNamespace(sampleApp), client.MatchingLabels{"app": sampleApp}); err != nil {
		return "", fmt.Errorf("listing %s pods: %w", sampleApp, err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
			return pod.Spec.NodeName, nil
		}
	}
	return "", nil
}
