//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gofail

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/e2e/framework"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

// TestRebootVerificationTimeoutEscalates exercises real activity dispatch and
// ScheduleToClose timeouts. Only agent scheduling and host recovery are simulated.
func TestRebootVerificationTimeoutEscalates(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	server := framework.StartTemporalDevServer(t, ctx)
	dsn := framework.StartPostgres(t)
	kubeconfig := framework.WriteKubeconfig(t)
	contents, err := os.ReadFile(kubeconfig)
	require.NoError(t, err)
	agent := framework.StartNodeAgentServer(t)
	agent.SetKubeconfig(string(contents))
	api := rebootAPIClient(t, kubeconfig)
	// The worker reads CAPI Machines from the namespace its config names, so the
	// Machine below has to be seeded there rather than in a fixed one.
	managementNamespace := framework.ManagementNamespace(t)
	require.NoError(t, api.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: managementNamespace}}))
	taskQueue := framework.TaskQueueName("reboot-timeout", t.Name())
	worker := framework.StartControllerWorkerBinary(t, framework.ControllerWorkerBinaryConfig{
		BinaryPath: framework.ControllerWorkerBinary(t), DBConnectionString: dsn, KubeconfigPath: kubeconfig,
		NodeAgent: config.NodeAgentConfig{Port: agent.Port(t)}, TemporalHostPort: server.FrontendHostPort(),
		Namespace: "default", TaskQueue: taskQueue,
		Env: []string{"NODE_REBOOT_INITIAL_TIMEOUT=45s", "NODE_REBOOT_POLLING_INTERVAL=10s", "NODE_REBOOT_SOFT_REBOOT_ENABLED=true"},
	})
	t.Cleanup(func() { worker.Stop(t) })
	worker.WaitReady(t)

	for _, scenario := range []struct {
		name        string
		err         error
		recoverSoft bool
	}{
		{name: "accepted-but-stalled"},
		{name: "unknown-outcome", err: status.Error(codes.Unavailable, "injected response loss")},
		{name: "delayed-soft-recovery", recoverSoft: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			agent.SetRebootError(scenario.err)
			beforeCalls := agent.RebootCalls()
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("soft reboot requests observed: %d\n%s", agent.RebootCalls()-beforeCalls, worker.Output())
				}
			})
			cluster := framework.SeedTeardownScenario(t, dsn, scenario.name)
			workerNode := framework.SeedRebootWorker(t, dsn, cluster)
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: workerNode.Name}, Spec: corev1.NodeSpec{Unschedulable: true}}
			require.NoError(t, api.Create(ctx, node))
			node.Status.NodeInfo.BootID = "old-boot"
			require.NoError(t, api.Status().Update(ctx, node))
			machine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: managementNamespace},
				Spec: clusterv1.MachineSpec{
					ClusterName: "cwe-gofail-" + cluster.Name,
					Bootstrap:   clusterv1.Bootstrap{DataSecretName: ptr.To("unused-bootstrap")},
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "NICoMachine", Name: node.Name,
					},
				},
			}
			require.NoError(t, api.Create(ctx, machine))
			node.Labels = map[string]string{keys.Reboot.String(): "e2e-stalled-soft-reboot"}
			require.NoError(t, api.Update(ctx, node))
			run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID: constants.RebootNodeWorkflowPrefix + workerNode.ID.String(), TaskQueue: taskQueue,
				WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
			}, workflows.RebootNode, workflows.RebootNodeInput{
				NodeID: workerNode.ID, Reason: "e2e-stalled-soft-reboot", InitialTimeout: 45 * time.Second,
				MaxTimeout: time.Minute, PollingInterval: 10 * time.Second, SoftRebootEnabled: true,
			})
			require.NoError(t, err)
			_, err = agent.WaitForReboot(ctx)
			require.NoError(t, err, "controller never requested a soft reboot:\n%s", worker.Output())
			require.NoError(t, waitForActivityScheduled(ctx, server.Client(), run, "WaitForNodeReboot"))
			waitStarted, pollingInterval := softVerificationOptions(t, ctx, server.Client(), run)

			if scenario.recoverSoft {
				require.Eventually(t, func() bool { return time.Since(waitStarted) >= 12*time.Second }, 15*time.Second, time.Second)
				setRebootBootID(t, ctx, api, node, "soft-boot")
			} else {
				// Keep boot state unchanged through the real verification window.
				// Any hard request before its deadline fails the regression.
				for time.Since(waitStarted) < 44*time.Second {
					require.NoError(t, api.Get(ctx, k8sclient.ObjectKeyFromObject(machine), machine))
					require.Empty(t, machine.Annotations[keys.MachineReboot.String()], "premature hard reboot")
					time.Sleep(time.Second)
				}
				require.Eventually(t, func() bool {
					if err := api.Get(ctx, k8sclient.ObjectKeyFromObject(machine), machine); err != nil {
						return false
					}
					return machine.Annotations[keys.MachineReboot.String()] != ""
				}, 30*time.Second, time.Second, "hard fallback was never requested:\n%s", worker.Output())
				hardAt, err := time.Parse(time.RFC3339, machine.Annotations[keys.MachineReboot.String()])
				require.NoError(t, err)
				require.GreaterOrEqual(t, hardAt.Sub(waitStarted), 44*time.Second, "hard request preceded the 45-second deadline")
				setRebootBootID(t, ctx, api, node, "hard-boot")
			}
			require.NoError(t, run.Get(ctx, nil), "reboot workflow did not recover:\n%s", worker.Output())
			require.Equal(t, beforeCalls+1, agent.RebootCalls(), "verification timeout must not repeat the soft request")
			require.Equal(t, 10*time.Second, pollingInterval)
			require.NoError(t, api.Get(ctx, k8sclient.ObjectKeyFromObject(node), node))
			require.True(t, node.Spec.Unschedulable, "reboot must leave the worker cordoned")
			require.Equal(t, string(constants.RebootStatusCompleted), node.Labels[keys.RebootStatus.String()])
			require.Empty(t, node.Labels[keys.Reboot.String()])
			if scenario.recoverSoft {
				hardScheduled, err := activityScheduled(ctx, server.Client(), run, "ApplyCAPIMachineRebootAnnotation")
				require.NoError(t, err)
				require.False(t, hardScheduled, "delayed soft recovery must avoid hard reboot")
			}
			t.Logf("%s completed with one soft request; worker stayed cordoned", scenario.name)
		})
	}
}

func rebootAPIClient(t testing.TB, kubeconfig string) k8sclient.Client {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	api, err := k8sclient.New(cfg, k8sclient.Options{Scheme: scheme})
	require.NoError(t, err)
	return api
}

func setRebootBootID(t testing.TB, ctx context.Context, api k8sclient.Client, node *corev1.Node, bootID string) {
	t.Helper()
	require.NoError(t, api.Get(ctx, k8sclient.ObjectKeyFromObject(node), node))
	node.Status.NodeInfo.BootID = bootID
	require.NoError(t, api.Status().Update(ctx, node))
}

func softVerificationOptions(t testing.TB, ctx context.Context, temporalClient client.Client, run client.WorkflowRun) (time.Time, time.Duration) {
	t.Helper()
	history := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for history.HasNext() {
		event, err := history.Next()
		require.NoError(t, err)
		attrs := event.GetActivityTaskScheduledEventAttributes()
		if attrs == nil || attrs.GetActivityType().GetName() != "WaitForNodeReboot" {
			continue
		}
		require.Equal(t, 45*time.Second, attrs.GetScheduleToCloseTimeout().AsDuration())
		var input activities.WaitForNodeRebootInput
		require.NoError(t, converter.GetDefaultDataConverter().FromPayloads(attrs.Input, &input))
		return event.EventTime.AsTime(), input.PollingInterval
	}
	require.FailNow(t, "soft verification activity is missing")
	return time.Time{}, 0
}
