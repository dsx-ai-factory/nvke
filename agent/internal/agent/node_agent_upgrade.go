// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/agentupgrade"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

var canonicalAgentVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type agentUpgradeLauncher interface {
	Start(operationID string) error
}

type systemdAgentUpgradeLauncher struct {
	runner nvkeutils.CommandRunner
}

func (l systemdAgentUpgradeLauncher) Start(operationID string) error {
	_, err := l.runner.Run(
		"systemctl",
		[]string{"start", "--no-block", "nke-agent-updater@" + operationID + ".service"},
		nil,
	)
	if err != nil {
		return fmt.Errorf("starting independent node-agent updater: %w", err)
	}
	return nil
}

// StartNodeAgentUpgrade persists an idempotent request and starts the
// independent updater service without waiting for agent replacement.
func (s *NodeAgentServer) StartNodeAgentUpgrade(
	ctx context.Context,
	req *pb.StartNodeAgentUpgradeRequest,
) (*pb.StartNodeAgentUpgradeResponse, error) {
	defer logRPCSpan(ctx, "StartNodeAgentUpgrade")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	operationID := strings.TrimSpace(req.GetOperationId())
	if err := agentupgrade.ValidateOperationID(operationID); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	targetVersion, err := parseCanonicalAgentVersion(req.GetTargetVersion())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	method, err := upgradeMethodFromProto(req.GetMethod())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.AgentUpgradeStore == nil || s.AgentUpgradeLauncher == nil {
		return nil, status.Error(codes.FailedPrecondition, "node-agent updater is not configured")
	}

	s.agentUpgradeMu.Lock()
	defer s.agentUpgradeMu.Unlock()

	s.mu.Lock()
	previousVersion := strings.TrimSpace(s.NodeStatus.AgentVersion)
	architecture := s.NodeStatus.Architecture
	s.mu.Unlock()
	if previousVersion == "" {
		return nil, status.Error(codes.FailedPrecondition, "current node-agent version is unknown")
	}
	request := agentupgrade.Operation{
		ID:              operationID,
		Action:          agentupgrade.ActionUpgrade,
		TargetVersion:   req.GetTargetVersion(),
		PreviousVersion: previousVersion,
		Method:          method,
	}
	if existing, found, loadErr := s.AgentUpgradeStore.Existing(request); loadErr != nil {
		return nil, upgradeStoreError(loadErr)
	} else if found {
		if existing.Phase == agentupgrade.PhasePending {
			if launchErr := s.AgentUpgradeLauncher.Start(operationID); launchErr != nil {
				existing, err = s.AgentUpgradeStore.SetStatus(
					operationID,
					agentupgrade.PhaseFailedRestored,
					"updater launch failed",
					launchErr.Error(),
				)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "restarting updater: %v; persisting failure: %v", launchErr, err)
				}
			}
		}
		return &pb.StartNodeAgentUpgradeResponse{Operation: upgradeOperationToProto(existing)}, nil
	}

	artifactURL := ""
	if method == agentupgrade.MethodBundle {
		if s.BundleIndexFile == "" || s.BundleSource == nil {
			return nil, status.Error(codes.FailedPrecondition, "bundle source is not configured")
		}
		if !s.BundleSource.IsHTTP() && !s.BundleSource.IsFile() {
			return nil, status.Error(codes.FailedPrecondition, "node-agent self-upgrade supports HTTP and file bundles")
		}
		bundle, resolveErr := nvkeutils.FetchBundleFromIndex(
			ctx,
			s.BundleServerURL,
			s.BundleIndexFile,
			"agent",
			targetVersion,
			architecture,
		)
		if resolveErr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "resolving exact agent bundle: %v", resolveErr)
		}
		artifactURL = bundle.URL
	}

	request.ArtifactURL = artifactURL
	op, created, err := s.AgentUpgradeStore.Create(request)
	if err != nil {
		return nil, upgradeStoreError(err)
	}
	if !created {
		return &pb.StartNodeAgentUpgradeResponse{Operation: upgradeOperationToProto(op)}, nil
	}
	if previousVersion == req.GetTargetVersion() {
		op, err = s.AgentUpgradeStore.SetStatus(operationID, agentupgrade.PhaseSucceeded, "already at requested version", "")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "persisting converged upgrade operation: %v", err)
		}
		return &pb.StartNodeAgentUpgradeResponse{Operation: upgradeOperationToProto(op)}, nil
	}
	if err := s.AgentUpgradeLauncher.Start(operationID); err != nil {
		op, persistErr := s.AgentUpgradeStore.SetStatus(operationID, agentupgrade.PhaseFailedRestored, "updater launch failed", err.Error())
		if persistErr != nil {
			return nil, status.Errorf(codes.Internal, "starting updater: %v; persisting failure: %v", err, persistErr)
		}
		return &pb.StartNodeAgentUpgradeResponse{Operation: upgradeOperationToProto(op)}, nil
	}
	return &pb.StartNodeAgentUpgradeResponse{Operation: upgradeOperationToProto(op)}, nil
}

// GetNodeAgentUpgradeStatus returns the durable state of an upgrade or rollback.
func (s *NodeAgentServer) GetNodeAgentUpgradeStatus(
	ctx context.Context,
	req *pb.GetNodeAgentUpgradeStatusRequest,
) (*pb.GetNodeAgentUpgradeStatusResponse, error) {
	defer logRPCSpan(ctx, "GetNodeAgentUpgradeStatus")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	operationID := strings.TrimSpace(req.GetOperationId())
	if err := agentupgrade.ValidateOperationID(operationID); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.AgentUpgradeStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "node-agent updater is not configured")
	}
	op, err := s.AgentUpgradeStore.Load(operationID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "node-agent upgrade operation %s not found", operationID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "loading node-agent upgrade operation: %v", err)
	}
	return &pb.GetNodeAgentUpgradeStatusResponse{Operation: upgradeOperationToProto(op)}, nil
}

// StartNodeAgentRollback restores the exact artifact retained by a prior
// successful node-agent upgrade.
func (s *NodeAgentServer) StartNodeAgentRollback(
	ctx context.Context,
	req *pb.StartNodeAgentRollbackRequest,
) (*pb.StartNodeAgentRollbackResponse, error) {
	defer logRPCSpan(ctx, "StartNodeAgentRollback")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	operationID := strings.TrimSpace(req.GetOperationId())
	upgradeOperationID := strings.TrimSpace(req.GetUpgradeOperationId())
	if err := agentupgrade.ValidateOperationID(operationID); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := agentupgrade.ValidateOperationID(upgradeOperationID); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "upgrade_operation_id: %v", err)
	}
	if s.AgentUpgradeStore == nil || s.AgentUpgradeLauncher == nil {
		return nil, status.Error(codes.FailedPrecondition, "node-agent updater is not configured")
	}

	s.agentUpgradeMu.Lock()
	defer s.agentUpgradeMu.Unlock()
	original, err := s.AgentUpgradeStore.Load(upgradeOperationID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "upgrade operation %s not found", upgradeOperationID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "loading upgrade operation: %v", err)
	}
	if original.Action != agentupgrade.ActionUpgrade || original.Phase != agentupgrade.PhaseSucceeded {
		return nil, status.Errorf(codes.FailedPrecondition, "operation %s is not a successful upgrade", upgradeOperationID)
	}

	s.mu.Lock()
	currentVersion := strings.TrimSpace(s.NodeStatus.AgentVersion)
	s.mu.Unlock()
	op, created, err := s.AgentUpgradeStore.Create(agentupgrade.Operation{
		ID:                     operationID,
		Action:                 agentupgrade.ActionRollback,
		TargetVersion:          original.PreviousVersion,
		PreviousVersion:        currentVersion,
		Method:                 original.Method,
		RestoreFromOperationID: original.ID,
	})
	if err != nil {
		return nil, upgradeStoreError(err)
	}
	if created || op.Phase == agentupgrade.PhasePending {
		if launchErr := s.AgentUpgradeLauncher.Start(operationID); launchErr != nil {
			op, persistErr := s.AgentUpgradeStore.SetStatus(operationID, agentupgrade.PhaseFailedNotRestored, "updater launch failed", launchErr.Error())
			if persistErr != nil {
				return nil, status.Errorf(codes.Internal, "starting rollback updater: %v; persisting failure: %v", launchErr, persistErr)
			}
			return &pb.StartNodeAgentRollbackResponse{Operation: upgradeOperationToProto(op)}, nil
		}
	}
	return &pb.StartNodeAgentRollbackResponse{Operation: upgradeOperationToProto(op)}, nil
}

func parseCanonicalAgentVersion(value string) (*semver.Version, error) {
	if !canonicalAgentVersionPattern.MatchString(value) {
		return nil, errors.New("target_version must be canonical vMAJOR.MINOR.PATCH")
	}
	version, err := semver.StrictNewVersion(strings.TrimPrefix(value, "v"))
	if err != nil {
		return nil, fmt.Errorf("target_version is invalid: %w", err)
	}
	return version, nil
}

func upgradeMethodFromProto(method pb.NodeAgentUpgradeMethod) (agentupgrade.Method, error) {
	switch method {
	case pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT:
		return agentupgrade.MethodAPT, nil
	case pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_BUNDLE:
		return agentupgrade.MethodBundle, nil
	case pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_UNSPECIFIED:
		return "", errors.New("method is required")
	default:
		return "", fmt.Errorf("unsupported node-agent upgrade method %d", method)
	}
}

func upgradeOperationToProto(op agentupgrade.Operation) *pb.NodeAgentUpgradeOperation {
	return &pb.NodeAgentUpgradeOperation{
		OperationId:     op.ID,
		TargetVersion:   op.TargetVersion,
		PreviousVersion: op.PreviousVersion,
		Method:          upgradeMethodToProto(op.Method),
		Phase:           upgradePhaseToProto(op.Phase),
		Step:            op.Step,
		ErrorMessage:    op.ErrorMessage,
	}
}

func upgradeMethodToProto(method agentupgrade.Method) pb.NodeAgentUpgradeMethod {
	switch method {
	case agentupgrade.MethodAPT:
		return pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_APT
	case agentupgrade.MethodBundle:
		return pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_BUNDLE
	default:
		return pb.NodeAgentUpgradeMethod_NODE_AGENT_UPGRADE_METHOD_UNSPECIFIED
	}
}

func upgradePhaseToProto(phase agentupgrade.Phase) pb.NodeAgentUpgradePhase {
	switch phase {
	case agentupgrade.PhasePending:
		return pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_PENDING
	case agentupgrade.PhaseRunning:
		return pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_RUNNING
	case agentupgrade.PhaseSucceeded:
		return pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_SUCCEEDED
	case agentupgrade.PhaseFailedRestored:
		return pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_RESTORED
	case agentupgrade.PhaseFailedNotRestored:
		return pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_FAILED_NOT_RESTORED
	default:
		return pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_UNSPECIFIED
	}
}

func upgradeStoreError(err error) error {
	switch {
	case errors.Is(err, agentupgrade.ErrOperationConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, agentupgrade.ErrOperationActive):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "persisting node-agent upgrade operation: %v", err)
	}
}
