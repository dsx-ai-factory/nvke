// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
)

const (
	defaultCloudInitStatusCheckName = "cloud-init-status"
	dsxCIDATAConsumedPath           = "/var/lib/dsxos/.cidata-consumed"
)

// cloudInitStatusRecoverableExitCode is the exit code `cloud-init status` uses
// to report recoverable errors, which it reports as a "degraded" status. It
// says nothing about how far cloud-init got: "degraded done" means cloud-init
// completed its work and recorded warnings on the way, so it still satisfies a
// completion check. Exit code 1 means cloud-init hit an unrecoverable error.
// See https://docs.cloud-init.io/en/latest/explanation/failure_states.html.
const cloudInitStatusRecoverableExitCode = 2

func (s *NodeAgentServer) ExecuteNodePreflight(
	ctx context.Context,
	req *pb.ExecuteNodePreflightRequest,
) (*pb.ExecuteNodePreflightResponse, error) {
	defer logRPCSpan(ctx, "ExecuteNodePreflight")()

	if req == nil || !hasExecuteNodePreflightWork(req) {
		return nodePreflightErrorResponse(
			"at least one node check, check_cloud_init_status, or kubeadm_preflight is required",
		), nil
	}

	resp := &pb.ExecuteNodePreflightResponse{
		Status: ReadyStatus,
	}

	if req.GetCheckCloudInitStatus() != nil {
		result, err := s.runCloudInitStatusPreflightCheck()
		if err != nil {
			return nodePreflightErrorResponse(err.Error()), nil
		}
		if !result.GetPassed() {
			resp.Status = NotReadyStatus
		}
		resp.Results = append(resp.Results, result)
	}

	for i, check := range req.GetChecks() {
		result, err := s.runNodePreflightCheck(check, i)
		if err != nil {
			return nodePreflightErrorResponse(err.Error()), nil
		}
		if !result.GetPassed() {
			resp.Status = NotReadyStatus
		}
		resp.Results = append(resp.Results, result)
	}

	if k := req.GetKubeadmPreflight(); k != nil {
		kubeadmResp, err := s.runKubeadmPreflight(ctx, k.GetConfig(), k.GetRole())
		if err != nil {
			return nodePreflightErrorResponse(err.Error()), nil
		}
		resp.KubeadmPreflight = kubeadmResp
		if strings.EqualFold(kubeadmResp.GetStatus(), ErrorStatus) {
			resp.Status = ErrorStatus
		} else if resp.Status == ReadyStatus && !strings.EqualFold(kubeadmResp.GetStatus(), ReadyStatus) {
			resp.Status = NotReadyStatus
		}
	}

	if !strings.EqualFold(resp.GetStatus(), ReadyStatus) {
		resp.ErrorMessage = executeNodePreflightErrorMessage(resp)
	}

	return resp, nil
}

func hasExecuteNodePreflightWork(req *pb.ExecuteNodePreflightRequest) bool {
	if req == nil {
		return false
	}
	return len(req.GetChecks()) > 0 ||
		req.GetCheckCloudInitStatus() != nil ||
		req.GetKubeadmPreflight() != nil
}

func (s *NodeAgentServer) runCloudInitStatusPreflightCheck() (*pb.NodePreflightResult, error) {
	markerPath, err := s.resolveNodeAbsolutePath(dsxCIDATAConsumedPath)
	if err != nil {
		return nil, fmt.Errorf("check_cloud_init_status: resolve DSX CIDATA completion marker: %w", err)
	}
	if info, err := os.Lstat(markerPath); err == nil && info.Mode().IsRegular() {
		return &pb.NodePreflightResult{
			Name:    defaultCloudInitStatusCheckName,
			Passed:  true,
			Message: fmt.Sprintf("DSX CIDATA configuration complete: %s", dsxCIDATAConsumedPath),
		}, nil
	} else if !os.IsNotExist(err) {
		if err == nil {
			return nil, fmt.Errorf("check_cloud_init_status: DSX CIDATA completion marker is not a regular file: %s", dsxCIDATAConsumedPath)
		}
		return nil, fmt.Errorf("check_cloud_init_status: stat DSX CIDATA completion marker: %w", err)
	}

	if s.Runner == nil {
		return nil, fmt.Errorf("check_cloud_init_status: command runner not configured")
	}

	output, runErr := s.Runner.Run("cloud-init", []string{"status"}, nil)
	report := strings.TrimSpace(string(output))

	// A non-zero exit only tells us cloud-init recorded errors, not that it
	// failed to report a status, so the report on stdout still decides this
	// check. Anything other than a degraded exit is treated as fatal.
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("check_cloud_init_status: cloud-init status: %w", runErr)
		}

		if exitCode := exitErr.ExitCode(); exitCode != cloudInitStatusRecoverableExitCode {
			return nil, fmt.Errorf(
				"check_cloud_init_status: cloud-init status exited %d: %s", exitCode, report,
			)
		}
	}

	passed, message, permanentErr := evaluateCloudInitStatus(report)
	if permanentErr != nil {
		return nil, fmt.Errorf("check_cloud_init_status: %w", permanentErr)
	}

	return &pb.NodePreflightResult{
		Name:    defaultCloudInitStatusCheckName,
		Passed:  passed,
		Message: message,
	}, nil
}

func evaluateCloudInitStatus(output string) (bool, string, error) {
	outputTrimmed := strings.TrimSpace(output)
	outputLower := strings.ToLower(outputTrimmed)

	if strings.Contains(outputLower, "status: done") {
		return true, fmt.Sprintf("cloud-init done: %s", outputTrimmed), nil
	}
	if strings.Contains(outputLower, "status: error") {
		return false, "", fmt.Errorf("cloud-init error status: %s", outputTrimmed)
	}
	if strings.Contains(outputLower, "status: running") {
		return false, fmt.Sprintf("cloud-init not done: %s", outputTrimmed), nil
	}
	if outputTrimmed == "" {
		return false, "cloud-init status returned empty output", nil
	}

	return false, fmt.Sprintf("cloud-init not done: %s", outputTrimmed), nil
}

func nodePreflightErrorResponse(message string) *pb.ExecuteNodePreflightResponse {
	return &pb.ExecuteNodePreflightResponse{
		Status:       ErrorStatus,
		ErrorMessage: message,
	}
}

func executeNodePreflightErrorMessage(resp *pb.ExecuteNodePreflightResponse) string {
	if resp == nil {
		return ""
	}

	if msg := strings.TrimSpace(resp.GetErrorMessage()); msg != "" {
		return msg
	}

	if k := resp.GetKubeadmPreflight(); k != nil {
		if msg := strings.TrimSpace(k.GetErrorMessage()); msg != "" {
			return msg
		}
	}

	var parts []string
	for _, r := range resp.GetResults() {
		if r.GetPassed() {
			continue
		}
		name := strings.TrimSpace(r.GetName())
		msg := strings.TrimSpace(r.GetMessage())
		switch {
		case name != "" && msg != "":
			parts = append(parts, fmt.Sprintf("%s: %s", name, msg))
		case name != "":
			parts = append(parts, name)
		case msg != "":
			parts = append(parts, msg)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}

	return fmt.Sprintf("preflight status: %s", resp.GetStatus())
}

func (s *NodeAgentServer) runNodePreflightCheck(
	check *pb.NodePreflightCheck,
	index int,
) (*pb.NodePreflightResult, error) {
	switch c := check.GetCheck().(type) {
	case *pb.NodePreflightCheck_FileExists:
		return s.runFileExistsPreflightCheck(check.GetName(), c.FileExists, index)
	default:
		return nil, fmt.Errorf("check %d: unsupported or missing check type", index)
	}
}

func (s *NodeAgentServer) runFileExistsPreflightCheck(
	name string,
	check *pb.FileExistsPreflightCheck,
	index int,
) (*pb.NodePreflightResult, error) {
	path := strings.TrimSpace(check.GetPath())
	if path == "" {
		return nil, fmt.Errorf("check %d: file_exists.path is required", index)
	}

	fullPath, err := s.resolveNodeAbsolutePath(path)
	if err != nil {
		return nil, fmt.Errorf("check %d: %w", index, err)
	}

	checkName := strings.TrimSpace(name)
	if checkName == "" {
		checkName = defaultFileExistsPreflightName(path)
	}

	if _, err := os.Stat(fullPath); err == nil {
		return &pb.NodePreflightResult{
			Name:    checkName,
			Passed:  true,
			Message: fmt.Sprintf("file exists: %s", path),
		}, nil
	} else if os.IsNotExist(err) {
		return &pb.NodePreflightResult{
			Name:    checkName,
			Passed:  false,
			Message: fmt.Sprintf("file does not exist: %s", path),
		}, nil
	}
	return nil, fmt.Errorf("check %d: stat %q: %v", index, path, err)
}

func defaultFileExistsPreflightName(path string) string {
	base := filepath.Base(path)
	if base == "" || base == string(filepath.Separator) {
		return "file_exists"
	}
	return "file_exists:" + base
}

func (s *NodeAgentServer) resolveNodeAbsolutePath(absPath string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(absPath))
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("path must be absolute: %q", absPath)
	}

	root := s.RootPath
	if root == "" {
		return cleaned, nil
	}

	fullPath := filepath.Join(root, strings.TrimPrefix(cleaned, string(filepath.Separator)))
	rel, err := filepath.Rel(root, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path outside root: %q", absPath)
	}
	return fullPath, nil
}
