// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/agentupgrade"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

var canonicalAgentVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

type agentUpgradeLauncher interface {
	Start(requestKey string) error
}

type systemdAgentUpgradeLauncher struct {
	runner nvkeutils.CommandRunner
}

func (l systemdAgentUpgradeLauncher) Start(requestKey string) error {
	if _, err := os.Stat(filepath.Join(agentupgrade.DefaultRoot, requestKey, "updater.py")); err == nil {
		// The bootstrap runbook owns this attempt until it writes final status.
		// Starting the packaged updater for it would race the bootstrap process.
		_, err := l.runner.Run("systemctl", []string{"start", "--no-block", "nvke-agent-upgrade-" + requestKey + ".service"}, nil)
		return err
	}
	_, err := l.runner.Run(
		"systemctl",
		[]string{"start", "--no-block", "nvke-agent-updater@" + requestKey + ".service"},
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
	unlock, lockErr := s.AgentUpgradeStore.Lock()
	if lockErr != nil {
		return nil, status.Errorf(codes.Internal, "locking upgrade admission: %v", lockErr)
	}
	defer unlock()
	current, err := s.AgentUpgradeStore.Current()
	if err != nil {
		return nil, upgradeStoreError(err)
	}
	if current != nil && !current.Terminal() && (current.TargetVersion != req.GetTargetVersion() || current.Method != method) {
		return nil, status.Error(codes.FailedPrecondition, "another agent upgrade is active")
	}
	if current == nil || current.Terminal() {
		if err := s.AgentUpgradeStore.Prune(); err != nil {
			return nil, upgradeStoreError(err)
		}
	}
	requestKey := (agentupgrade.Request{TargetVersion: req.GetTargetVersion(), Method: method}).Key()

	s.mu.Lock()
	previousVersion := strings.TrimSpace(s.NodeStatus.AgentVersion)
	architecture := s.NodeStatus.Architecture
	s.mu.Unlock()
	if previousVersion == "" {
		return nil, status.Error(codes.FailedPrecondition, "current node-agent version is unknown")
	}
	if previousVersion == req.GetTargetVersion() && (current == nil || current.Terminal()) {
		return &pb.StartNodeAgentUpgradeResponse{Operation: &pb.NodeAgentUpgradeOperation{TargetVersion: previousVersion, Method: req.GetMethod(), Phase: pb.NodeAgentUpgradePhase_NODE_AGENT_UPGRADE_PHASE_SUCCEEDED, Step: "already at requested version"}}, nil
	}
	request := agentupgrade.Operation{
		TargetVersion:   req.GetTargetVersion(),
		PreviousVersion: previousVersion,
		Method:          method,
	}
	if existing, found, loadErr := s.AgentUpgradeStore.Existing(request); loadErr != nil {
		return nil, upgradeStoreError(loadErr)
	} else if found {
		existing, err = s.launchNodeAgentUpdater(existing)
		if err != nil {
			return nil, err
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
		op, err = s.AgentUpgradeStore.SetStatus(requestKey, agentupgrade.PhaseSucceeded, "already at requested version", "")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "persisting converged upgrade operation: %v", err)
		}
		return &pb.StartNodeAgentUpgradeResponse{Operation: upgradeOperationToProto(op)}, nil
	}
	op, err = s.launchNodeAgentUpdater(op)
	if err != nil {
		return nil, err
	}
	return &pb.StartNodeAgentUpgradeResponse{Operation: upgradeOperationToProto(op)}, nil
}

// GetNodeAgentUpgradeStatus returns the durable state of a version change.
func (s *NodeAgentServer) GetNodeAgentUpgradeStatus(
	ctx context.Context,
	req *pb.GetNodeAgentUpgradeStatusRequest,
) (*pb.GetNodeAgentUpgradeStatusResponse, error) {
	defer logRPCSpan(ctx, "GetNodeAgentUpgradeStatus")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if s.AgentUpgradeStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "node-agent updater is not configured")
	}
	s.agentUpgradeMu.Lock()
	defer s.agentUpgradeMu.Unlock()
	unlock, err := s.AgentUpgradeStore.Lock()
	if err != nil {
		return nil, upgradeStoreError(err)
	}
	defer unlock()
	current, err := s.AgentUpgradeStore.Current()
	if err != nil {
		return nil, upgradeStoreError(err)
	}
	if current == nil {
		return &pb.GetNodeAgentUpgradeStatusResponse{}, nil
	}
	return &pb.GetNodeAgentUpgradeStatusResponse{Operation: upgradeOperationToProto(*current)}, nil
}

// launchNodeAgentUpdater reasserts the service start and persists launch failures.
func (s *NodeAgentServer) launchNodeAgentUpdater(op agentupgrade.Operation) (agentupgrade.Operation, error) {
	if op.Phase != agentupgrade.PhasePending && op.Phase != agentupgrade.PhaseRunning {
		return op, nil
	}
	if err := s.AgentUpgradeLauncher.Start(op.Key()); err != nil {
		phase := agentupgrade.PhaseFailedNotRestored
		if op.Phase == agentupgrade.PhasePending {
			phase = agentupgrade.PhaseFailedRestored
		}
		updated, persistErr := s.AgentUpgradeStore.SetStatus(op.Key(), phase, "updater launch failed", err.Error())
		if persistErr != nil {
			return op, status.Errorf(codes.Internal, "starting updater: %v; persisting failure: %v", err, persistErr)
		}
		return updated, nil
	}
	return op, nil
}

func parseCanonicalAgentVersion(value string) (*semver.Version, error) {
	if !canonicalAgentVersionPattern.MatchString(value) {
		return nil, errors.New("target_version must be canonical vMAJOR.MINOR.PATCH with an optional prerelease")
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
	case errors.Is(err, agentupgrade.ErrOperationActive):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "persisting node-agent upgrade operation: %v", err)
	}
}
