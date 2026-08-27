// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/Masterminds/semver/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
	"github.com/NVIDIA/nke/agent/pkg/utils"
)

const (
	kubeadmPatchesDir     = "/etc/kubernetes/patches"
	kubeadmUpgradeCommand = "upgrade"
	kubeadmPlanCommand    = "plan"
	kubeadmApplyCommand   = "apply"
	kubeadmAssumeYesFlag  = "-y"
)

// KubeadmUpgradeApply upgrades the control plane and its add-ons to the target
// Kubernetes version. kubeadm upgrade apply is idempotent, so retries run the
// command again to finish any incomplete control-plane or add-on convergence.
func (s *NodeAgentServer) KubeadmUpgradeApply(
	ctx context.Context,
	req *pb.KubeadmUpgradeApplyRequest,
) (*pb.KubeadmUpgradeApplyResponse, error) {
	defer logRPCSpan(ctx, "KubeadmUpgradeApply")()

	target, err := semver.StrictNewVersion(strings.TrimSpace(req.GetTargetVersion()))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "target_version must be a semantic version without a leading v: %v", err)
	}
	targetVersion := "v" + target.String()

	if !s.isControlPlaneRole() {
		return nil, status.Errorf(codes.FailedPrecondition, "kubeadm upgrade apply requires a control-plane node")
	}

	if err := os.MkdirAll(s.rootedPath(kubeadmPatchesDir), 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "create kubeadm patches directory: %v", err)
	}

	response := &pb.KubeadmUpgradeApplyResponse{TargetVersion: targetVersion}
	planOutput, err := s.Runner.Run("kubeadm", []string{kubeadmUpgradeCommand, kubeadmPlanCommand}, &utils.CommandOptions{})
	response.PlanOutput = string(planOutput)
	if err != nil {
		slog.ErrorContext(ctx, "kubeadm upgrade plan failed", "targetVersion", targetVersion, "err", err)
		return nil, status.Errorf(codes.Internal, "kubeadm upgrade plan failed: %v", err)
	}

	applyArgs := []string{kubeadmUpgradeCommand, kubeadmApplyCommand, targetVersion, "--patches=" + kubeadmPatchesDir, kubeadmAssumeYesFlag}
	applyOutput, err := s.Runner.Run("kubeadm", applyArgs, &utils.CommandOptions{})
	response.ApplyOutput = string(applyOutput)
	if err != nil {
		slog.ErrorContext(ctx, "kubeadm upgrade apply failed", "targetVersion", targetVersion, "err", err)
		return nil, status.Errorf(codes.Internal, "kubeadm upgrade apply %s failed: %v", targetVersion, err)
	}

	response.Outcome = pb.KubeadmUpgradeApplyResponse_OUTCOME_APPLIED
	return response, nil
}
