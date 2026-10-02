//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gofail

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/dsx-ai-factory/nke/controller/e2e/framework"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/workflows"
)

func TestGofailRunbookWorkerCrashRetriesAfterHeartbeatTimeout(t *testing.T) {
	const targetRunbook = "upgrade-kubelet-kubeadm"

	testStarted := time.Now()
	t.Log("starting local Temporal, PostgreSQL, agent, and production CWE worker")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	server := framework.StartTemporalDevServer(t, ctx)
	databaseDSN := framework.StartPostgres(t)
	nodeAgent := framework.StartNodeAgentServer(t)
	tenantAPI := framework.StartTenantAPIServer(t)
	nodeAgent.SetKubeconfig(tenantAPI.Kubeconfig())
	kubeconfigPath := framework.WriteKubeconfig(t)
	taskQueue := framework.TaskQueueName("cwe-gofail-runbook", t.Name())
	workerBinary := framework.CWEWorkerBinary(t)
	gofailAddr := framework.FreeLocalhostAddress(t)

	failingWorker := framework.StartCWEWorkerBinary(t, framework.CWEWorkerBinaryConfig{
		BinaryPath:         workerBinary,
		DBConnectionString: databaseDSN,
		KubeconfigPath:     kubeconfigPath,
		NodeAgent:          config.NodeAgentConfig{Port: nodeAgent.Port(t)},
		TemporalHostPort:   server.FrontendHostPort(),
		Namespace:          "default",
		TaskQueue:          taskQueue,
		GofailHTTPAddr:     gofailAddr,
	})
	failingWorker.WaitReady(t)
	t.Logf("instrumented CWE worker is ready; gofail HTTP endpoint is %s", gofailAddr)
	framework.SeedCWEGofailDB(t, databaseDSN)
	framework.SetGofail(t, gofailAddr, framework.CWEGofailFailpointName, `return("`+targetRunbook+`")`)
	t.Logf("activated failpoint %s for runbook %q", framework.CWEGofailFailpointName, targetRunbook)

	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                  taskQueue,
		TaskQueue:           taskQueue,
		WorkflowRunTimeout:  3*time.Minute + 30*time.Second,
		WorkflowTaskTimeout: 5 * time.Second,
	}, workflows.UpgradeControlPlaneAddons, workflows.UpgradeControlPlaneAddonsInput{
		ClusterID:         framework.CWEGofailClusterID,
		KubernetesVersion: "1.31.0",
	})
	require.NoError(t, err)
	t.Logf("started production workflow %s run %s", run.GetID(), run.GetRunID())

	crashCtx, crashCancel := context.WithTimeout(ctx, 10*time.Second)
	defer crashCancel()
	require.NoError(t, failingWorker.WaitForOutput(crashCtx, framework.CWEGofailWorkerCrashLine),
		"gofail crash marker was not emitted:\n%s", failingWorker.Output())
	require.Contains(t, failingWorker.Output(), framework.CWEGofailWorkerCrashLine)
	t.Logf("observed failpoint marker; injected worker output:\n%s", failingWorker.Output())
	exitErr := failingWorker.Kill(t)
	require.Error(t, exitErr, "failing worker should report an error after the e2e framework kills it")
	t.Logf("killed injected worker process: %v", exitErr)

	recoveryWorker := framework.StartCWEWorkerBinary(t, framework.CWEWorkerBinaryConfig{
		BinaryPath:         workerBinary,
		DBConnectionString: databaseDSN,
		KubeconfigPath:     kubeconfigPath,
		NodeAgent:          config.NodeAgentConfig{Port: nodeAgent.Port(t)},
		TemporalHostPort:   server.FrontendHostPort(),
		Namespace:          "default",
		TaskQueue:          taskQueue,
	})
	t.Cleanup(func() {
		recoveryWorker.Stop(t)
	})
	recoveryWorker.WaitReady(t)
	t.Log("replacement CWE worker is ready")

	// The real policy waits for the two-minute heartbeat timeout and then 30 seconds before retrying.
	retryCtx, retryCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer retryCancel()
	t.Log("waiting for Temporal to detect the missing heartbeat and retry after the production backoff")
	var result workflows.UpgradeControlPlaneAddonsResult
	err = run.Get(retryCtx, &result)
	require.NoError(t, err,
		"workflow did not resume promptly after worker loss; failing worker output:\n%s\nrecovery worker output:\n%s",
		failingWorker.Output(), recoveryWorker.Output())
	require.Equal(t, framework.CWEGofailNodeID, result.NodeID)
	t.Logf("workflow recovered on the replacement worker with node %s", result.NodeID)
	tenantAPI.AssertDNSReconciled(t)
	_, err = nodeAgent.WaitForScript(retryCtx, targetRunbook)
	require.NoError(t, err, "recovered workflow did not call the production runbook")
	t.Logf("agent observed recovered runbook %q", targetRunbook)
	t.Logf("replacement worker output:\n%s", recoveryWorker.Output())
	t.Logf("gofail crash-recovery scenario completed in %s", time.Since(testStarted).Round(time.Millisecond))
}
