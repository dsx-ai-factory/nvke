// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	// Keep in sync with controller/internal/constants.ProvisionNodeWorkflowPrefix.
	provisionNodeWorkflowPrefix = "provision-node-"
	agentAuditLogPath           = "/var/log/nvke-agent-audit.log"
	agentTraceProbeContainer    = "agent-trace"
	controllerLogWindow         = 2 * time.Hour
	controllerLogLimit          = 16 << 20
)

// validateAgentTracePostCreate observes the trace created when the node
// scheduler starts each ProvisionNode workflow. It proves the same trace ID
// reaches the agent's durable audit file and its journald mirror.
func validateAgentTracePostCreate(test *tenantTest) {
	It("carries each ProvisionNode trace into the agent audit logs", func(ctx context.Context) {
		nodes := expectSucceed(test.nvke.ListClusterNodes(ctx, test.cluster.ID))
		Expect(nodes).NotTo(BeEmpty(), "the provisioned cluster should have nodes")
		tenantClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
		tenantConfig := expectSucceed(test.tenantClients.AdminRESTConfig(ctx, test.cluster.Name))
		tenantClientset := expectSucceed(kubernetes.NewForConfig(tenantConfig))

		for _, node := range nodes {
			workflowID := provisionNodeWorkflowPrefix + node.ID.String()
			var traceID string
			By(fmt.Sprintf("Finding the trace for %s on node %s", workflowID, node.Name))
			Eventually(func(ctx context.Context) error {
				var err error
				traceID, err = controllerWorkflowTraceID(ctx, test.k8sClientset, workflowID)
				return err
			}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

			By(fmt.Sprintf("Finding trace %s in the audit file and journal on node %s", traceID, node.Name))
			_, _, err := kube.RunPodWithLogs(ctx, tenantClient, tenantClientset, agentTraceProbe(node.Name, traceID), agentTraceProbeContainer, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred())
		}
	}, Label("lifecycle", "post-ready", "tracing"), NodeTimeout(10*time.Minute))
}

func controllerWorkflowTraceID(ctx context.Context, clientset kubernetes.Interface, workflowID string) (string, error) {
	namespace := controller.Namespace()
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, controller.Deployment(), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting the controller deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return "", fmt.Errorf("building the controller pod selector: %w", err)
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return "", fmt.Errorf("listing controller pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", errors.New("the controller deployment has no pods")
	}

	var observed []string
	sinceSeconds := int64(controllerLogWindow.Seconds())
	limitBytes := int64(controllerLogLimit)
	for i := range pods.Items {
		for _, previous := range []bool{false, true} {
			logs, readErr := clientset.CoreV1().Pods(namespace).GetLogs(pods.Items[i].Name, &corev1.PodLogOptions{
				Container:    controller.Deployment(),
				Previous:     previous,
				SinceSeconds: &sinceSeconds,
				LimitBytes:   &limitBytes,
			}).DoRaw(ctx)
			if readErr != nil {
				if previous {
					continue
				}
				return "", fmt.Errorf("reading controller pod %s logs: %w", pods.Items[i].Name, readErr)
			}
			observed = append(observed, traceIDsForWorkflow(logs, workflowID)...)
		}
	}
	if len(observed) == 0 {
		return "", fmt.Errorf("controller logs have no trace ID for workflow %s", workflowID)
	}
	traceID := observed[0]
	for _, candidate := range observed[1:] {
		if candidate != traceID {
			return "", fmt.Errorf("controller logs have multiple trace IDs for workflow %s: %s and %s", workflowID, traceID, candidate)
		}
	}
	return traceID, nil
}

func traceIDsForWorkflow(logs []byte, workflowID string) []string {
	var traceIDs []string
	for line := range bytes.SplitSeq(logs, []byte{'\n'}) {
		var record struct {
			WorkflowID string `json:"workflow_id"`
			TraceID    string `json:"trace_id"`
			Event      string `json:"event"`
		}
		if json.Unmarshal(line, &record) != nil || record.WorkflowID != workflowID || record.Event != "workflow.started" || !validTraceID(record.TraceID) {
			continue
		}
		traceIDs = append(traceIDs, record.TraceID)
	}
	return traceIDs
}

func validTraceID(value string) bool {
	if len(value) != 32 || value == strings.Repeat("0", 32) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func agentTraceProbe(nodeName, traceID string) *corev1.Pod {
	privileged := true
	hostPathType := corev1.HostPathDirectory
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-agent-trace-",
			Namespace:    corev1.NamespaceDefault,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			Containers: []corev1.Container{{
				Name:    agentTraceProbeContainer,
				Image:   kube.KubectlImage,
				Command: []string{"/bin/sh", "-ec"},
				Args: []string{fmt.Sprintf(`
needle='"trace_id":"'"${TRACE_ID}"'"'
audit=/host%s
audit_record="$(grep -F "$needle" "$audit" | grep -F '"msg":"grpc request received"' | tail -n 1 || true)"
test -n "$audit_record" || {
  echo "agent audit record not found in $audit; recent audit trace IDs:" >&2
  grep -o '"trace_id":"[0-9a-f]*"' "$audit" | tail -n 10 >&2 || true
  exit 1
}
chroot /host /usr/bin/journalctl -u nvke-agent --no-pager -o cat | grep -Fqx "$audit_record" || {
  echo "agent audit record not found in the nvke-agent journal" >&2
  exit 1
}
printf 'found trace %%s in the agent audit file and journal\n' "$TRACE_ID" > /dev/termination-log
`, agentAuditLogPath)},
				Env: []corev1.EnvVar{{Name: "TRACE_ID", Value: traceID}},
				SecurityContext: &corev1.SecurityContext{
					Privileged: &privileged,
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "host-root",
					MountPath: "/host",
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "host-root",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
					Path: "/",
					Type: &hostPathType,
				}},
			}},
		},
	}
}
