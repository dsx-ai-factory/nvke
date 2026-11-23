// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

//go:embed scripts/node-preflight-cloud-init.sh
var cloudInitCompletionScript string

func executeNodePreflight(ctx context.Context, client nvkeproto.NodeAgentClient, version string, req *nvkeproto.ExecuteNodePreflightRequest) (*nvkeproto.ExecuteNodePreflightResponse, error) {
	cloudInitCompletionCheckOnly := req.GetCheckCloudInitStatus() != nil && len(req.GetChecks()) == 0 && req.GetKubeadmPreflight() == nil
	if !cloudInitCompletionCheckOnly || VersionAtLeast(version, "0.1.24", true) {
		return client.ExecuteNodePreflight(ctx, req)
	}
	return executeCloudInitCompletionScript(ctx, client)
}

func executeCloudInitCompletionScript(ctx context.Context, client nvkeproto.NodeAgentClient) (*nvkeproto.ExecuteNodePreflightResponse, error) {
	req := &nvkeproto.ExecuteScriptRequest{ScriptName: "node-preflight-cloud-init", ScriptContents: cloudInitCompletionScript}
	resp, err := client.ExecuteScript(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.GetStatus() != "success" || resp.GetExitCode() != 0 || resp.GetErrorMessage() != "" {
		return nil, fmt.Errorf("preflight script %s failed: status=%q exit=%d error=%s output=%s", req.GetScriptName(), resp.GetStatus(), resp.GetExitCode(), resp.GetErrorMessage(), resp.GetOutput())
	}
	state := strings.TrimSpace(resp.GetOutput())
	switch state {
	case "ready", "notready":
	default:
		return nil, fmt.Errorf("preflight script %s returned invalid readiness: %q", req.GetScriptName(), resp.GetOutput())
	}
	result := &nvkeproto.ExecuteNodePreflightResponse{Status: state, Results: []*nvkeproto.NodePreflightResult{{Name: "cloud-init-status", Passed: state == "ready"}}}
	if state == "notready" {
		result.ErrorMessage = "bootstrap completion check pending"
	}
	return result, nil
}
