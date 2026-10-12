// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	applyNodeConditionWorkflowType   = "ApplyK8sNodeCondition"
	nodeReconfigureWorkflowType      = "NodeReconfigureKubeletServer"
	e2eNodeConditionType             = "nke.nvidia.com/E2EPostReadyValidation"
	maintenanceTaintKey              = "nke.nvidia.com/maintenance"
	nodeOperationExecutionTimeout    = 5 * time.Minute
	nodeOperationObservationInterval = time.Second
)

func validatePostReadyNodeOperations(test *tenantTest) {
	It("applies the requested condition to a Ready tenant Node", func(ctx context.Context) {
		workerPool := expectWorkerPool(ctx, test)
		worker := expectSucceed(test.nvke.WaitForReadyNodeInPool(ctx, workerPool.ID))
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))

		now := metav1.Now()
		condition := corev1.NodeCondition{
			Type:               corev1.NodeConditionType(e2eNodeConditionType),
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
			Reason:             "E2EPostReadyValidation",
			Message:            "post-ready tenant Node condition validation",
		}
		input := struct {
			NodeID        string               `json:"NodeID"`
			NodeCondition corev1.NodeCondition `json:"NodeCondition"`
		}{NodeID: worker.ID.String(), NodeCondition: condition}
		rawInput := expectSucceed(json.Marshal(input))

		By("Executing the registered ApplyK8sNodeCondition workflow")
		workflowID := fmt.Sprintf("e2e-apply-node-condition-%s-%d", worker.ID, time.Now().UnixNano())
		Expect(runTemporalWorkflow(ctx, test.k8sClient, applyNodeConditionWorkflowType, workflowID, []string{string(rawInput)}, nodeOperationExecutionTimeout)).To(Succeed())

		By("Verifying the requested condition on the Ready tenant Node")
		Eventually(func(ctx context.Context) error {
			return assertE2ENodeCondition(ctx, tenantClient, worker.Name, condition)
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
	}, Label("lifecycle", "post-ready", "node-condition"), NodeTimeout(10*time.Minute))

	It("taints, restarts, and observes a new Lease through the kubelet reconfigure workflow", func(ctx context.Context) {
		workerPool := expectWorkerPool(ctx, test)
		worker := expectSucceed(test.nvke.WaitForReadyNodeInPool(ctx, workerPool.ID))
		tenantClient := expectSucceed(test.tenantClients.GetClient(ctx, test.cluster.Name))
		previousRenewTime := expectSucceed(nodeLeaseRenewTime(ctx, tenantClient, worker.Name))

		Expect(test.cluster.ControlPlaneVIP).NotTo(BeNil(), "CAPD cluster should report its control-plane VIP")
		vip := strings.TrimSpace(*test.cluster.ControlPlaneVIP)
		Expect(vip).NotTo(BeEmpty(), "CAPD cluster should report a non-empty control-plane VIP")
		input := struct {
			Server string `json:"server"`
		}{Server: "https://" + net.JoinHostPort(vip, "6443")}
		rawInput := expectSucceed(json.Marshal(input))
		rawNodeID := expectSucceed(json.Marshal(worker.ID.String()))

		By("Executing the registered NodeReconfigureKubeletServer workflow")
		workflowID := fmt.Sprintf("e2e-reconfigure-kubelet-%s-%d", worker.ID, time.Now().UnixNano())
		workflowDone := make(chan error, 1)
		go func() {
			workflowDone <- runTemporalWorkflow(ctx, test.k8sClient, nodeReconfigureWorkflowType, workflowID, []string{string(rawNodeID), string(rawInput)}, nodeOperationExecutionTimeout)
		}()

		By("Observing the maintenance taint while reconfiguration is in progress")
		Expect(observeMaintenanceTaint(ctx, tenantClient, worker.Name, workflowDone)).To(Succeed())

		By("Waiting for the reconfiguration workflow to complete")
		select {
		case err := <-workflowDone:
			Expect(err).NotTo(HaveOccurred())
		case <-ctx.Done():
			Fail(fmt.Sprintf("waiting for kubelet reconfigure workflow: %v", ctx.Err()))
		}

		By("Verifying the Lease advanced, the taint was removed, and the tenant Node is Ready")
		Eventually(func(ctx context.Context) error {
			return assertKubeletReconfigureCompleted(ctx, tenantClient, worker.Name, previousRenewTime)
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	}, Label("lifecycle", "post-ready", "node-reconfigure"), NodeTimeout(10*time.Minute))
}

func assertE2ENodeCondition(ctx context.Context, tenantClient client.Client, nodeName string, expected corev1.NodeCondition) error {
	var node corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("getting tenant Node %s: %w", nodeName, err)
	}
	if ready, detail := kubernetesNodeReady(&node); !ready {
		return fmt.Errorf("tenant Node %s is not Ready: %s", nodeName, detail)
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type != expected.Type {
			continue
		}
		if condition.Status != expected.Status || condition.Reason != expected.Reason || condition.Message != expected.Message {
			return fmt.Errorf("tenant Node %s condition %s = status %s reason %q message %q, want status %s reason %q message %q",
				nodeName, expected.Type, condition.Status, condition.Reason, condition.Message, expected.Status, expected.Reason, expected.Message)
		}
		return nil
	}
	return fmt.Errorf("tenant Node %s is missing condition %s", nodeName, expected.Type)
}

func nodeLeaseRenewTime(ctx context.Context, tenantClient client.Client, nodeName string) (time.Time, error) {
	var lease coordinationv1.Lease
	if err := tenantClient.Get(ctx, client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: nodeName}, &lease); err != nil {
		return time.Time{}, fmt.Errorf("getting Lease for tenant Node %s: %w", nodeName, err)
	}
	if lease.Spec.RenewTime == nil {
		return time.Time{}, fmt.Errorf("Lease for tenant Node %s has no renewTime", nodeName)
	}
	return lease.Spec.RenewTime.Time, nil
}

func observeMaintenanceTaint(ctx context.Context, tenantClient client.Client, nodeName string, workflowDone <-chan error) error {
	ticker := time.NewTicker(nodeOperationObservationInterval)
	defer ticker.Stop()

	for {
		var node corev1.Node
		if err := tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err == nil {
			for _, taint := range node.Spec.Taints {
				if taint.Key == maintenanceTaintKey && taint.Effect == corev1.TaintEffectNoSchedule {
					return nil
				}
			}
		}

		select {
		case err := <-workflowDone:
			if err != nil {
				return err
			}
			return fmt.Errorf("kubelet reconfigure workflow completed before the maintenance taint was observed")
		case <-ctx.Done():
			return fmt.Errorf("observing maintenance taint on tenant Node %s: %w", nodeName, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertKubeletReconfigureCompleted(ctx context.Context, tenantClient client.Client, nodeName string, previousRenewTime time.Time) error {
	var node corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("getting reconfigured tenant Node %s: %w", nodeName, err)
	}
	if ready, detail := kubernetesNodeReady(&node); !ready {
		return fmt.Errorf("reconfigured tenant Node %s is not Ready: %s", nodeName, detail)
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == maintenanceTaintKey {
			return fmt.Errorf("reconfigured tenant Node %s still has maintenance taint", nodeName)
		}
	}
	renewTime, err := nodeLeaseRenewTime(ctx, tenantClient, nodeName)
	if err != nil {
		return err
	}
	if !renewTime.After(previousRenewTime) {
		return fmt.Errorf("tenant Node %s Lease renewTime = %s, want later than %s", nodeName, renewTime, previousRenewTime)
	}
	return nil
}
