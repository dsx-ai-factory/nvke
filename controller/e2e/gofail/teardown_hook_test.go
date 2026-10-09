//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gofail

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"

	"github.com/dsx-ai-factory/nke/controller/e2e/framework"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/temporal/workflows"
)

// drainActivityName is the first activity TeardownNode reaches after the
// beforeRemoveNode hooks, so its presence in history is the signal that the
// workflow got past a hook that never succeeds.
const drainActivityName = "DrainKubeNode"

// hookActivityName is the activity that runs a teardown hook runbook.
const hookActivityName = "ExecuteClusterRunbook"

// forceUnreachableHookAttemptBudget mirrors forceUnreachableHookMaxAttempts in
// the workflows package, which is unexported.
const forceUnreachableHookAttemptBudget int32 = 3

// teardownRunTimeout must stay well clear of the hook's retry chain (attempts at
// roughly 0s, 1m, 3m, 7m under the production 1m/5m backoff). Temporal will not
// schedule a retry that cannot start before the run timeout, so a tight timeout
// silently truncates the unbounded retry the normal path is supposed to show —
// making a blocked teardown look like it gave up on its own.
const teardownRunTimeout = 30 * time.Minute

// TestGofailTeardownHookBlocksUnlessForceUnreachable pins the teardown-hook
// blocking behavior against a real Temporal server and the production worker binary.
//
// A beforeRemoveNode hook that can never succeed (modeled with the
// CWERunbookExecutionError failpoint, standing in for evict-cnpg-node waiting
// on PVCs whose pod will never terminate) must:
//
//   - keep blocking a normal teardown, because the hooks protect data; and
//   - be abandoned on a force-delete, so the operator's force-unreachable
//     request is not swallowed by an unbounded retry loop.
//
// The unit tests assert this against the Temporal test environment, which
// simulates timers. This exercises the real retry budget, real activity
// dispatch, and the real worker process.
func TestGofailTeardownHookBlocksUnlessForceUnreachable(t *testing.T) {
	testStarted := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	server := framework.StartTemporalDevServer(t, ctx)
	databaseDSN := framework.StartPostgres(t)
	nodeAgent := framework.StartNodeAgentServer(t)
	kubeconfigPath := framework.WriteKubeconfig(t)
	taskQueue := framework.TaskQueueName("cwe-gofail-teardown-hook", t.Name())
	gofailAddr := framework.FreeLocalhostAddress(t)

	worker := framework.StartControllerWorkerBinary(t, framework.ControllerWorkerBinaryConfig{
		BinaryPath:         framework.ControllerWorkerBinary(t),
		DBConnectionString: databaseDSN,
		KubeconfigPath:     kubeconfigPath,
		NodeAgent:          config.NodeAgentConfig{Port: nodeAgent.Port(t)},
		TemporalHostPort:   server.FrontendHostPort(),
		Namespace:          "default",
		TaskQueue:          taskQueue,
		GofailHTTPAddr:     gofailAddr,
	})
	t.Cleanup(func() { worker.Stop(t) })
	worker.WaitReady(t)

	// Fail the hook runbook outright rather than hanging it: the behavior under
	// test is the retry budget, and a hang would cost a two-minute heartbeat
	// timeout per attempt.
	framework.SetGofail(t, gofailAddr, framework.ControllerGofailExecutionErrorFailpoint,
		`return("`+framework.TeardownHookRunbook+`")`)
	t.Logf("failpoint %s will fail runbook %q on every attempt",
		framework.ControllerGofailExecutionErrorFailpoint, framework.TeardownHookRunbook)

	blocked := framework.SeedTeardownScenario(t, databaseDSN, "blocked")
	forced := framework.SeedTeardownScenario(t, databaseDSN, "forced")

	// Run both teardowns concurrently against the one worker so the wall clock is
	// the slower scenario rather than their sum.
	blockedRun := startTeardown(ctx, t, server.Client(), taskQueue, blocked, false)
	forcedRun := startTeardown(ctx, t, server.Client(), taskQueue, forced, true)

	t.Log("waiting for the force-unreachable teardown to abandon the hook and reach drain")
	forcedCtx, forcedCancel := context.WithTimeout(ctx, 6*time.Minute)
	defer forcedCancel()
	require.NoError(t,
		waitForActivityScheduled(forcedCtx, server.Client(), forcedRun, drainActivityName),
		"force-unreachable teardown never got past the failing hook; worker output:\n%s", worker.Output())
	t.Logf("force-unreachable teardown reached %s after %s", drainActivityName, time.Since(testStarted).Round(time.Second))

	// The blocked run has now had at least as long as the forced one, which was
	// enough for the full bounded retry budget. If it were going to give up it
	// would have by now.
	scheduled, err := activityScheduled(ctx, server.Client(), blockedRun, drainActivityName)
	require.NoError(t, err)
	require.False(t, scheduled,
		"normal teardown skipped a failing beforeRemoveNode hook; the hooks protect data and must keep blocking")

	describe, err := server.Client().DescribeWorkflowExecution(ctx, blockedRun.GetID(), blockedRun.GetRunID())
	require.NoError(t, err)
	status := describe.GetWorkflowExecutionInfo().GetStatus()

	blockedAttempts, err := hookActivityAttempts(ctx, server.Client(), blockedRun)
	require.NoError(t, err)
	forcedAttempts, err := hookActivityAttempts(ctx, server.Client(), forcedRun)
	require.NoError(t, err)
	// A run that is still retrying reports 0: Temporal keeps the in-flight
	// attempt transient until the activity reaches a terminal state.
	t.Logf("hook attempts in durable history: blocked=%d (status %s), forced=%d",
		blockedAttempts, status, forcedAttempts)

	// The force path must give up on a bounded budget; that bound is what makes
	// the tolerate branch reachable instead of the workflow spinning.
	require.LessOrEqual(t, forcedAttempts, forceUnreachableHookAttemptBudget,
		"force-unreachable teardown must abandon the hook within its retry budget")

	// The normal path must not silently skip the hook. With the production retry
	// policy it should still be retrying; if it did reach a terminal state, it
	// must at least have failed loudly naming the hook that blocked it.
	if status != enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
		err := blockedWorkflowError(ctx, blockedRun)
		require.Error(t, err, "normal teardown finished successfully despite a hook that never succeeds")
		require.Contains(t, err.Error(), framework.TeardownHookRunbook,
			"normal teardown failed without naming the hook that blocked it\nhistory:\n%s",
			historySummary(ctx, server.Client(), blockedRun))
		t.Logf("normal teardown surfaced the hook failure instead of skipping it\nhistory:\n%s",
			historySummary(ctx, server.Client(), blockedRun))
	}

	t.Logf("blocked teardown is still retrying the hook, as intended (%s elapsed)", time.Since(testStarted).Round(time.Second))
}

// blockedWorkflowError reports why a run that should still be retrying instead
// reached a terminal state, so a regression here says what actually happened.
func blockedWorkflowError(ctx context.Context, run client.WorkflowRun) error {
	getCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return run.Get(getCtx, nil)
}

func startTeardown(
	ctx context.Context,
	t *testing.T,
	temporalClient client.Client,
	taskQueue string,
	scenario framework.TeardownScenario,
	forceUnreachable bool,
) client.WorkflowRun {
	t.Helper()

	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                  taskQueue + "-" + scenario.Name,
		TaskQueue:           taskQueue,
		WorkflowRunTimeout:  teardownRunTimeout,
		WorkflowTaskTimeout: 5 * time.Second,
	}, workflows.TeardownNode, workflows.TeardownNodeInput{
		NodeID:           scenario.TargetNode,
		Reason:           "gofail-e2e",
		AllowCPNode:      true,
		ForceUnreachable: forceUnreachable,
	})
	require.NoError(t, err)
	t.Logf("started teardown %q (forceUnreachable=%t) as %s", scenario.Name, forceUnreachable, run.GetID())
	return run
}

// waitForActivityScheduled polls history until the named activity is scheduled
// or ctx expires.
func waitForActivityScheduled(ctx context.Context, temporalClient client.Client, run client.WorkflowRun, activityName string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		scheduled, err := activityScheduled(ctx, temporalClient, run, activityName)
		if err != nil {
			return err
		}
		if scheduled {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// hookActivityAttempts returns the highest attempt number Temporal recorded for
// the hook activity, which is the retry budget it actually applied. Temporal
// keeps intermediate retries as transient events, so the attempt counter on the
// started/failed events is the reliable source rather than the event count.
func hookActivityAttempts(ctx context.Context, temporalClient client.Client, run client.WorkflowRun) (int32, error) {
	scheduledIDs := map[int64]struct{}{}
	var attempts int32

	iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false,
		enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return 0, err
		}
		switch attrs := event.GetAttributes().(type) {
		case *historypb.HistoryEvent_ActivityTaskScheduledEventAttributes:
			if attrs.ActivityTaskScheduledEventAttributes.GetActivityType().GetName() == hookActivityName {
				scheduledIDs[event.GetEventId()] = struct{}{}
			}
		case *historypb.HistoryEvent_ActivityTaskStartedEventAttributes:
			started := attrs.ActivityTaskStartedEventAttributes
			if _, ok := scheduledIDs[started.GetScheduledEventId()]; ok && started.GetAttempt() > attempts {
				attempts = started.GetAttempt()
			}
		}
	}

	return attempts, nil
}

// historySummary renders event type + timestamp lines, so a surprising terminal
// state explains itself in the failure output instead of needing a re-run.
func historySummary(ctx context.Context, temporalClient client.Client, run client.WorkflowRun) string {
	var b strings.Builder
	iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false,
		enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var start time.Time
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			fmt.Fprintf(&b, "history error: %v\n", err)
			break
		}
		at := event.GetEventTime().AsTime()
		if start.IsZero() {
			start = at
		}
		fmt.Fprintf(&b, "  +%-8s %s\n", at.Sub(start).Round(time.Second), event.GetEventType())
	}
	return b.String()
}

func activityScheduled(ctx context.Context, temporalClient client.Client, run client.WorkflowRun, activityName string) (bool, error) {
	iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false,
		enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return false, err
		}
		attrs, ok := event.GetAttributes().(*historypb.HistoryEvent_ActivityTaskScheduledEventAttributes)
		if !ok {
			continue
		}
		if attrs.ActivityTaskScheduledEventAttributes.GetActivityType().GetName() == activityName {
			return true, nil
		}
	}
	return false, nil
}
