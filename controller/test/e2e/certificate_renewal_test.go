// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

const (
	renewK8sCertsWorkflowType            = "RenewK8sCertsV2"
	forceCertificateRenewalThresholdDays = 30
)

// validatePostReadyCertificateRenewal first runs a non-forced V2 cycle, which
// must select no node for fresh certificates, then forces a uniquely identified
// V2 cycle so fresh certificates cannot take the unchanged path. Ready nodes and
// changed static-pod container IDs demonstrate restart and recovery, not CA
// rotation or independent verification of certificate contents.
func validatePostReadyCertificateRenewal(test *tenantTest) {
	It("renews every control-plane certificate and returns the cluster to Ready", func(ctx context.Context) {
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		expectedPools := expectedNodeCounts(test.spec)
		expectedControlPlanes := controlPlaneNodeCount(test.spec)
		Expect(expectedControlPlanes).To(BeNumerically(">", 0), "certificate renewal requires control-plane nodes")

		By("Waiting for every static control-plane component to be Ready before renewal")
		var originalContainerIDs map[string]string
		Eventually(func(ctx context.Context) error {
			var err error
			originalContainerIDs, err = staticControlPlaneContainerIDs(ctx, tenantClient, expectedControlPlanes)
			return err
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())

		runRenewal := func(force bool) string {
			input := struct {
				ClusterID     string `json:"clusterID"`
				ThresholdDays int    `json:"thresholdDays"`
				Force         bool   `json:"force"`
			}{
				ClusterID:     test.cluster.ID.String(),
				ThresholdDays: forceCertificateRenewalThresholdDays,
				Force:         force,
			}
			rawInput := expectSucceed(json.Marshal(input))
			workflowID := fmt.Sprintf("e2e-renew-k8s-certs-%s-%d", test.cluster.ID, time.Now().UnixNano())
			Expect(runTemporalWorkflow(
				ctx,
				test.k8sClient,
				renewK8sCertsWorkflowType,
				workflowID,
				[]string{string(rawInput)},
				20*time.Minute,
			)).To(Succeed())
			return workflowID
		}

		// Joined control-plane nodes have no super-admin.conf, so this exercises
		// expiry selection on every node of a multi-node control plane.
		By("Executing a non-forced certificate-renewal cycle, which leaves fresh certificates unchanged")
		nonForcedWorkflowID := runRenewal(false)
		renewal := expectSucceed(test.nvke.GetCluster(ctx, test.cluster.ID)).Conditions.CertificateRenewal
		Expect(renewal).NotTo(BeNil(), "the non-forced cycle must record a certificate renewal condition")
		Expect(renewal.CycleID).To(Equal(nonForcedWorkflowID))
		Expect(renewal.State).To(Equal(api.CertificateRenewalStatusStateSucceeded))
		Expect(renewal.Outcome).To(HaveValue(Equal(api.CertificateRenewalStatusOutcomeUnchanged)),
			"a non-forced cycle with fresh certificates must report outcome unchanged")
		Expect(staticControlPlaneContainerIDs(ctx, tenantClient, expectedControlPlanes)).To(Equal(originalContainerIDs),
			"a non-forced cycle with fresh certificates must not restart static control-plane components")

		By("Executing a forced certificate-renewal cycle after cluster readiness")
		runRenewal(true)

		By("Verifying the controller and the tenant API report every node Ready after renewal")
		Eventually(func(ctx context.Context) error {
			if err := test.assertDesiredNodesReady(ctx); err != nil {
				return err
			}
			_, err := validateNodesReadyInCluster(ctx, test, tenantClient, expectedPools)
			return err
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())

		By("Verifying every static control-plane component restarted and is Ready after renewal")
		Eventually(func(ctx context.Context) error {
			currentContainerIDs, err := staticControlPlaneContainerIDs(ctx, tenantClient, expectedControlPlanes)
			if err != nil {
				return err
			}
			for componentNode, originalContainerID := range originalContainerIDs {
				currentContainerID, ok := currentContainerIDs[componentNode]
				if !ok {
					return fmt.Errorf("static control-plane component %s is missing after renewal", componentNode)
				}
				if currentContainerID == originalContainerID {
					return fmt.Errorf("static control-plane component %s did not restart", componentNode)
				}
			}
			return nil
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	}, Label("lifecycle", "post-ready", "cert-renewal"), NodeTimeout(25*time.Minute))
}

func staticControlPlaneContainerIDs(ctx context.Context, tenantClient client.Client, expected int) (map[string]string, error) {
	containerIDs := make(map[string]string, expected*4)
	for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd"} {
		var pods corev1.PodList
		if err := tenantClient.List(
			ctx,
			&pods,
			client.InNamespace(kubeSystemNamespace),
			client.MatchingLabels{"component": component},
		); err != nil {
			return nil, fmt.Errorf("listing %s static pods: %w", component, err)
		}
		if len(pods.Items) != expected {
			return nil, fmt.Errorf("found %d %s static pods, want %d", len(pods.Items), component, expected)
		}
		for i := range pods.Items {
			if !podReady(&pods.Items[i]) {
				return nil, fmt.Errorf("%s static pod %s is not Ready", component, pods.Items[i].Name)
			}
			if len(pods.Items[i].Status.ContainerStatuses) != 1 || pods.Items[i].Status.ContainerStatuses[0].ContainerID == "" {
				return nil, fmt.Errorf("%s static pod %s has no running container ID", component, pods.Items[i].Name)
			}
			containerIDs[component+"/"+pods.Items[i].Spec.NodeName] = pods.Items[i].Status.ContainerStatuses[0].ContainerID
		}
	}
	return containerIDs, nil
}
