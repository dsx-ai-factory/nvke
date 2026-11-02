// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

// runTemporalWorkflow executes a registered production workflow using a
// one-shot copy of the deployed Temporal admin-tools Pod template.
func runTemporalWorkflow(
	ctx context.Context,
	managementClient client.Client,
	workflowType string,
	workflowID string,
	inputs []string,
	timeout time.Duration,
) error {
	temporalNamespace := envOrDefault("TEMPORAL_K8S_NAMESPACE", "temporal")
	adminToolsDeployment := envOrDefault("TEMPORAL_ADMIN_TOOLS_DEPLOYMENT", "temporal-admintools")
	adminToolsContainer := envOrDefault("TEMPORAL_ADMIN_TOOLS_CONTAINER", "admin-tools")

	var deployment appsv1.Deployment
	if err := managementClient.Get(ctx, client.ObjectKey{Namespace: temporalNamespace, Name: adminToolsDeployment}, &deployment); err != nil {
		return fmt.Errorf("getting Temporal admin-tools deployment: %w", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: *deployment.Spec.Template.ObjectMeta.DeepCopy(),
		Spec:       *deployment.Spec.Template.Spec.DeepCopy(),
	}
	pod.Name = ""
	pod.GenerateName = "e2e-temporal-workflow-"
	pod.Namespace = temporalNamespace
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever

	operatorContainer := -1
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == adminToolsContainer {
			operatorContainer = i
			break
		}
	}
	if operatorContainer == -1 {
		return fmt.Errorf("Temporal admin-tools deployment has no %q container", adminToolsContainer)
	}

	operator := &pod.Spec.Containers[operatorContainer]
	operator.Command = []string{"temporal"}
	operator.Args = []string{
		"workflow", "execute",
		"--address", envOrDefault("E2E_TEMPORAL_ADDRESS", "temporal-frontend."+temporalNamespace+".svc.cluster.local:7233"),
		"--namespace", envOrDefault("E2E_TEMPORAL_NAMESPACE", "nvke-controller"),
		"--task-queue", envOrDefault("E2E_TEMPORAL_TASK_QUEUE", "nvke-controller-tasks"),
		"--type", workflowType,
		"--workflow-id", workflowID,
		"--execution-timeout", timeout.String(),
		"--run-timeout", timeout.String(),
	}
	for _, input := range inputs {
		operator.Args = append(operator.Args, "--input", input)
	}
	operator.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError

	if _, _, err := kube.RunPod(ctx, managementClient, pod, adminToolsContainer, timeout+time.Minute); err != nil {
		return fmt.Errorf("running Temporal workflow %s (%s): %w", workflowType, workflowID, err)
	}
	return nil
}
