// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
	"github.com/dsx-ai-factory/nvke/controller/internal/runbook"
)

const (
	runbookLogOutputLimit = 8192
)

type ExecuteHostRunbookInput struct {
	HostID      uuid.UUID
	RunbookName string
	RunbookEnv  map[string]string
	RunbookArgs []string
	// ExpectedExitCodes lists acceptable process exit codes. If empty, the
	// historical default of exit code 0 is used.
	ExpectedExitCodes []int
}

type ExecuteHostRunbookResult struct {
	Output       string
	ErrorMessage string
	ExitCode     int
}

func (a *Activities) ExecuteHostRunbook(ctx context.Context, input ExecuteHostRunbookInput) (ExecuteHostRunbookResult, error) {
	return a.runHostRunbook(ctx, input, a.cfg.EffectiveRunbooksDir())
}

// runHostRunbook loads scripts from runbooksDir before embedded scripts. An
// empty runbooksDir selects only the controller-owned embedded script.
func (a *Activities) runHostRunbook(ctx context.Context, input ExecuteHostRunbookInput, runbooksDir string) (ExecuteHostRunbookResult, error) {
	node, err := a.db.GetNode(ctx, input.HostID)
	if err != nil {
		return ExecuteHostRunbookResult{}, fmt.Errorf("failed to get node %s: %w", input.HostID, err)
	}

	res, err := a.executeHostRunbook(ctx, input, node, runbooksDir)

	clusterName := ""
	if cluster, clusterErr := a.db.GetCluster(ctx, node.ClusterID); clusterErr != nil {
		activity.GetLogger(ctx).Debug("failed to resolve cluster name for runbook metric", "clusterID", node.ClusterID, "error", clusterErr)
	} else {
		clusterName = cluster.Name
	}

	metrics.RecordRunbookExecution(input.RunbookName, clusterName, node.Name, err)

	return res, err
}

func (a *Activities) executeHostRunbook(ctx context.Context, input ExecuteHostRunbookInput, node db.Node, runbooksDir string) (ExecuteHostRunbookResult, error) {
	script, err := runbook.Script(runbooksDir, input.RunbookName)
	if err != nil {
		return ExecuteHostRunbookResult{}, err
	}
	script, err = runbook.InjectEnvironment(input.RunbookEnv, script)
	if err != nil {
		return ExecuteHostRunbookResult{}, fmt.Errorf("failed to inject runbook environment: %w", err)
	}

	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	//nolint:gocritic // gofail rewrites this commented block in instrumented builds.
	// gofail: var CWERunbookWorkerCrash string
	// if CWERunbookWorkerCrash == input.RunbookName {
	// 	fmt.Println("cwe-gofail-runbook-worker-crash")
	// 	select {}
	// }

	// Fails the named runbook without hanging, so e2e scenarios about retry
	// budgets and failure tolerance do not have to wait out a heartbeat timeout
	// per attempt. Models a runbook that cannot succeed — e.g. a teardown hook
	// waiting on a node whose kubelet is gone.
	// gofail: var CWERunbookExecutionError string
	// if CWERunbookExecutionError == input.RunbookName {
	// 	fmt.Println("cwe-gofail-runbook-execution-error")
	// 	return ExecuteHostRunbookResult{}, fmt.Errorf("gofail: injected failure for runbook %q", input.RunbookName)
	// }

	result, err := a.nodeAgentClient.ExecuteScript(ctx, node, input.RunbookName, script, input.RunbookArgs)
	if err != nil {
		return ExecuteHostRunbookResult{}, fmt.Errorf("failed to execute runbook %q on node %s: %w", input.RunbookName, node.Name, err)
	}

	// Preserve SWE behavior by default: SWE RunbookExecution.Finish translated any
	// non-zero agent ExecuteScript exit code into ExecutionStatusFailed, which
	// the controller surfaced as RunbookError.
	if !isExpectedRunbookExitCode(int(result.GetExitCode()), input) {
		output := result.GetOutput()
		scriptError := result.GetErrorMessage()
		errMessage := fmt.Sprintf("runbook %q on node %s exited %d", input.RunbookName, node.Name, result.GetExitCode())
		activity.GetLogger(ctx).Error("runbook execution failed",
			"runbookName", input.RunbookName,
			"nodeName", node.Name,
			"nodeID", node.ID,
			"exitCode", result.GetExitCode(),
			"expectedExitCodes", expectedRunbookExitCodes(input),
			"output", truncateForLog(output, runbookLogOutputLimit),
			"scriptError", truncateForLog(scriptError, runbookLogOutputLimit),
		)
		errDetails := map[string]string{
			"output": output,
			"error":  scriptError,
		}
		return ExecuteHostRunbookResult{}, temporal.NewApplicationError(errMessage, "RunbookError", errDetails)
	}

	return ExecuteHostRunbookResult{
		Output:       result.GetOutput(),
		ErrorMessage: result.GetErrorMessage(),
		ExitCode:     int(result.GetExitCode()),
	}, nil
}

func isExpectedRunbookExitCode(exitCode int, input ExecuteHostRunbookInput) bool {
	for _, expected := range expectedRunbookExitCodes(input) {
		if exitCode == expected {
			return true
		}
	}
	return false
}

func expectedRunbookExitCodes(input ExecuteHostRunbookInput) []int {
	if len(input.ExpectedExitCodes) == 0 {
		return []int{0}
	}
	return input.ExpectedExitCodes
}

// truncateForLog keeps the tail of s, since a failing runbook's fatal error is
// almost always the last thing printed. The full, untruncated output is still
// preserved in the RunbookError details.
func truncateForLog(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return "..." + s[len(s)-limit:]
}
