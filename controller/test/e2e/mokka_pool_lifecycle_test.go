// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

func validateMokkaPoolLifecycle(test *tenantTest) {
	It("adds, scales and removes an H100 pool while a GB300 GPU workload keeps running", func(ctx SpecContext) {
		c := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
		clientset := expectSucceed(kubernetes.NewForConfig(expectSucceed(test.tenantClients.AdminRESTConfig(ctx, test.cluster.Name))))
		primary := expectWorkerPool(ctx, test)
		primaryIDs := expectNodeIDs(ctx, test, primary.ID)
		Expect(primaryIDs).To(HaveLen(1))
		primaryNames := expectNodeNames(ctx, test, primary.ID)

		By("Starting a persistent GPU workload on the original GB300 pool")
		probe := gpuPod(corev1.NamespaceDefault, probeImage{ref: defaultTenantGPUImage}, nil)
		probe.Spec.NodeSelector = map[string]string{gpuProductLabel: mokkaProfiles["gb300"].product}
		probe.Spec.Containers[0].Command = []string{"sh", "-ec", "nvidia-smi -L; while true; do nvidia-smi -L >/dev/null; date +%s; sleep 1; done"}
		Expect(c.Create(ctx, probe)).To(Succeed())
		deleteOnExit(ctx, c, probe)
		Eventually(func(ctx context.Context) bool {
			if err := c.Get(ctx, client.ObjectKeyFromObject(probe), probe); err != nil {
				return false
			}
			return podReady(probe)
		}).WithContext(ctx).WithTimeout(mokkaWait).WithPolling(sitePoll).Should(BeTrue())
		originalProbe := probe.DeepCopy()
		Expect(primaryNames).To(ContainElement(probe.Spec.NodeName))

		check := func(stage string, extra *api.NodePool, count int) {
			GinkgoHelper()
			By("Checking GPU identity, capacity and workload continuity after " + stage)
			pools := map[string][]string{"gb300": primaryNames}
			Eventually(func(ctx context.Context) error {
				if extra != nil && count > 0 {
					members, err := test.nvke.ListNodePoolNodes(ctx, extra.ID)
					if err != nil {
						return err
					}
					if len(members) != count {
						return fmt.Errorf("H100 pool has %d node records, want %d", len(members), count)
					}
					names := make([]string, 0, len(members))
					for _, node := range members {
						names = append(names, node.Name)
					}
					pools["h100"] = names
				}
				var nodes corev1.NodeList
				if err := c.List(ctx, &nodes); err != nil {
					return err
				}
				problems := mokkaPoolProblems(nodes.Items, pools)
				labeled, err := mokkaNodes(ctx, c)
				if err != nil {
					return err
				}
				var validators corev1.PodList
				if err := c.List(ctx, &validators, client.InNamespace(gpuOperatorNamespace), client.MatchingLabels{"app": "nvidia-operator-validator"}); err != nil {
					return err
				}
				return joinProblems("Mokka pool lifecycle", append(problems, validatorProblems(labeled, validators.Items)...))
			}).WithContext(ctx).WithTimeout(test.timeouts.Provision).WithPolling(sitePoll).Should(Succeed())
			Expect(expectNodeIDs(ctx, test, primary.ID)).To(Equal(primaryIDs), "the original pool must retain its nodes")
			for profile := range pools {
				validateMokkaWorkload(ctx, c, clientset, profile)
			}
			Expect(c.Get(ctx, client.ObjectKeyFromObject(probe), probe)).To(Succeed())
			Expect(mokkaContinuityProblem(*originalProbe, *probe)).To(Succeed())
			readHeartbeats := func() (int64, int64, error) {
				logs, err := clientset.CoreV1().Pods(probe.Namespace).GetLogs(probe.Name, &corev1.PodLogOptions{Container: gpuContainer}).DoRaw(ctx)
				if err != nil {
					return 0, 0, err
				}
				return mokkaHeartbeats(string(logs))
			}
			before, _, err := readHeartbeats()
			Expect(err).NotTo(HaveOccurred())
			var latest, gap int64
			Eventually(func() error {
				var err error
				latest, gap, err = readHeartbeats()
				if err != nil {
					return err
				}
				if latest <= before {
					return fmt.Errorf("GPU workload has not emitted a new heartbeat after %d", before)
				}
				return nil
			}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
			GinkgoLogr.Info("Mokka pool lifecycle heartbeat", "stage", stage, "latest", latest, "maxGapSeconds", gap)
		}
		check("initial bring-up", nil, 0)

		By("Installing the H100 simulator before adding nodes of that model")
		Expect(installMokkaProfile(ctx, test, "h100")).To(Succeed())
		req := expectSucceed(workerNodePoolRequest(test, "mokka-h100-added", "mh"))
		req.NodeLabels = map[string]string{mokkaLabelKey: "h100"}
		extra := expectSucceed(test.nvke.CreateNodePool(ctx, req))
		Expect(extra.NodeLabels).To(Equal(req.NodeLabels))
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, extra.ID, 1)).To(Succeed())
		check("adding H100", extra, 1)

		By("Scaling the H100 pool from one node to two")
		expectSucceed(test.nvke.UpdateNodePoolDesiredCount(ctx, extra.ID, 2))
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, extra.ID, 2)).To(Succeed())
		check("scaling H100 up", extra, 2)

		By("Lowering the H100 desired count and explicitly deleting one node, as NVKE requires")
		expectSucceed(test.nvke.UpdateNodePoolDesiredCount(ctx, extra.ID, 1))
		nodes := expectSucceed(test.nvke.ListNodePoolNodes(ctx, extra.ID))
		Expect(nodes).To(HaveLen(2))
		Expect(test.nvke.DeleteNode(ctx, nodes[0].ID)).To(Succeed())
		Expect(test.nvke.WaitForProvisionedNodeCount(ctx, extra.ID, 1)).To(Succeed())
		check("scaling H100 down", extra, 1)

		By("Deleting the H100 pool and waiting for its GPU nodes to disappear")
		Expect(test.nvke.DeleteNodePool(ctx, extra.ID)).To(Succeed())
		Expect(test.nvke.WaitForNodePoolDeleted(ctx, extra.ID)).To(Succeed())
		Expect(expectSucceed(test.nvke.ListNodePoolNodes(ctx, extra.ID))).To(BeEmpty())
		check("removing H100", nil, 0)
	}, NodeTimeout(45*time.Minute))
}

// mokkaPoolProblems compares tenant nodes to the NVKE pool membership, rather
// than trusting the profile label to identify which model a node should have.
func mokkaPoolProblems(nodes []corev1.Node, pools map[string][]string) []string {
	var problems []string
	want := 0
	for profile, names := range pools {
		want += len(names)
		for _, name := range names {
			var found *corev1.Node
			for i := range nodes {
				if nodes[i].Name == name {
					found = &nodes[i]
					break
				}
			}
			if found == nil {
				problems = append(problems, name+": missing tenant node")
				continue
			}
			if found.Labels[mokkaLabelKey] != profile {
				problems = append(problems, fmt.Sprintf("%s: profile %q, want %q", name, found.Labels[mokkaLabelKey], profile))
			}
			problems = append(problems, mokkaModelProblems([]corev1.Node{*found})...)
			if ready, detail := kubernetesNodeReady(found); !ready {
				problems = append(problems, name+": "+detail)
			}
		}
	}
	if len(nodes) != want+1 {
		problems = append(problems, fmt.Sprintf("%d tenant nodes, want %d workers plus one control plane", len(nodes), want))
	}
	// This also detects stale GPU nodes after scale-down or pool deletion.
	return append(problems, gpuNodeProblems(nodes, want)...)
}

func mokkaContinuityProblem(before, after corev1.Pod) error {
	if before.UID != after.UID || before.Spec.NodeName != after.Spec.NodeName || !podReady(&after) || after.DeletionTimestamp != nil {
		return fmt.Errorf("original GPU workload was replaced, moved, deleted or is not Ready")
	}
	for _, status := range after.Status.ContainerStatuses {
		if status.RestartCount != 0 {
			return fmt.Errorf("GPU workload container %s restarted %d times", status.Name, status.RestartCount)
		}
	}
	return nil
}

// mokkaHeartbeats reports the largest observed gap without imposing an
// unagreed availability SLO on this simulation. Pod identity/restarts and
// forward progress are separate assertions.
func mokkaHeartbeats(logs string) (int64, int64, error) {
	var latest, maxGap int64
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if strings.HasPrefix(line, "GPU ") {
			continue
		}
		stamp, parseErr := strconv.ParseInt(line, 10, 64)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("invalid GPU workload heartbeat %q: %w", line, parseErr)
		}
		if latest != 0 {
			if stamp < latest {
				return 0, 0, fmt.Errorf("GPU workload heartbeat moved backwards: %d to %d", latest, stamp)
			}
			maxGap = max(maxGap, stamp-latest)
		}
		latest = stamp
	}
	if latest == 0 {
		return 0, 0, fmt.Errorf("GPU workload produced no heartbeats")
	}
	return latest, maxGap, nil
}
