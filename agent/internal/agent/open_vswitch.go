// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
)

const (
	openVSwitchPackage           = "openvswitch-switch"
	openVSwitchService           = "openvswitch-switch"
	openVSwitchDaemonService     = "ovs-vswitchd"
	openVSwitchSystemIDPath      = "/etc/openvswitch/system-id.conf"
	openVSwitchInstallUUIDPath   = "/etc/openvswitch/install_uuid.conf"
	openVSwitchBridgePrefix      = "br"
	openVSwitchIPCommand         = "ip"
	openVSwitchDeviceFlag        = "dev"
	openVSwitchDefaultRoute      = "default"
	openVSwitchDefaultMTU        = 1500
	openVSwitchDefaultSettleTime = 2 * time.Second
	openVSwitchConvergenceDelay  = time.Second

	operationTypeOpenVSwitchReconciliation = "open_vswitch_reconciliation"
)

var (
	errOpenVSwitchDefaultRouteMissing = errors.New("no default route with an interface is present")
	errOpenVSwitchNotConverged        = errors.New("Open vSwitch network has not converged")
)

type openVSwitchRoute struct {
	Destination string `json:"dst"`
	Gateway     string `json:"gateway"`
	Device      string `json:"dev"`
}

type openVSwitchLink struct {
	Name    string `json:"ifname"`
	Address string `json:"address"`
	MTU     int    `json:"mtu"`
}

type openVSwitchPlan struct {
	backend        pb.OpenVSwitchReconciliationResult_NetworkBackend
	bridgeName     string
	uplinkName     string
	uplinkMAC      string
	uplinkMTU      int
	defaultGateway string
	configPath     string
	config         []byte
	address        string
	netmask        string
}

type persistedOpenVSwitchResult struct {
	Outcome         int32  `json:"outcome"`
	BridgeName      string `json:"bridgeName"`
	UplinkInterface string `json:"uplinkInterface"`
	NetworkBackend  int32  `json:"networkBackend"`
}

// StartOpenVSwitchReconciliation persists an idempotent operation before the
// disruptive host-network mutation begins.
func (s *NodeAgentServer) StartOpenVSwitchReconciliation(
	ctx context.Context,
	req *pb.StartOpenVSwitchReconciliationRequest,
) (*pb.StartOpenVSwitchReconciliationResponse, error) {
	defer logRPCSpan(ctx, "StartOpenVSwitchReconciliation")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if s.Runner == nil {
		return nil, status.Error(codes.FailedPrecondition, "command runner is not configured")
	}
	if s.Persister == nil {
		return nil, status.Error(codes.FailedPrecondition, "status persister is not configured")
	}

	operationID := strings.TrimSpace(req.GetOperationId())
	if err := s.recoverInterruptedOpenVSwitchReconciliation(operationID); err != nil {
		return nil, err
	}
	op, err := s.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   operationID,
		Type: operationTypeOpenVSwitchReconciliation,
	}, s.runOpenVSwitchReconciliation)
	if err != nil {
		return nil, err
	}
	operation, err := openVSwitchOperationToProto(op)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode Open vSwitch operation: %v", err)
	}
	return &pb.StartOpenVSwitchReconciliationResponse{Operation: operation}, nil
}

// GetOpenVSwitchReconciliationStatus returns the persisted state of an
// asynchronous Open vSwitch reconciliation.
func (s *NodeAgentServer) GetOpenVSwitchReconciliationStatus(
	ctx context.Context,
	req *pb.GetOpenVSwitchReconciliationStatusRequest,
) (*pb.GetOpenVSwitchReconciliationStatusResponse, error) {
	defer logRPCSpan(ctx, "GetOpenVSwitchReconciliationStatus")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := s.getNodeAgentOperation(nodeAgentOperationSpec{
		ID:   strings.TrimSpace(req.GetOperationId()),
		Type: operationTypeOpenVSwitchReconciliation,
	})
	if err != nil {
		return nil, err
	}
	operation, err := openVSwitchOperationToProto(op)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode Open vSwitch operation: %v", err)
	}
	return &pb.GetOpenVSwitchReconciliationStatusResponse{Operation: operation}, nil
}

func (s *NodeAgentServer) runOpenVSwitchReconciliation(
	ctx context.Context,
	progress nodeAgentOperationProgress,
) error {
	progress.SetStep("reconciling")
	s.openVSwitchMu.Lock()
	defer s.openVSwitchMu.Unlock()

	result, plan, err := s.reconcileOpenVSwitchLocked(ctx)
	for errors.Is(err, errOpenVSwitchDefaultRouteMissing) && plan == nil {
		progress.SetStep("waiting_for_default_route")
		if err = sleepOrDone(ctx, s.openVSwitchConvergenceDelayOrDefault()); err != nil {
			return err
		}
		result, plan, err = s.reconcileOpenVSwitchLocked(ctx)
	}
	if errors.Is(err, errOpenVSwitchNotConverged) && plan != nil {
		progress.SetStep("waiting_for_network")
		for errors.Is(err, errOpenVSwitchNotConverged) {
			if err = sleepOrDone(ctx, s.openVSwitchConvergenceDelayOrDefault()); err != nil {
				return err
			}
			err = s.verifyOpenVSwitchPlan(*plan)
		}
		if err == nil {
			result = configuredOpenVSwitchResult(*plan)
		}
	}
	if err != nil {
		return err
	}

	encodedResult, err := json.Marshal(persistedOpenVSwitchResult{
		Outcome:         int32(result.GetOutcome()),
		BridgeName:      result.GetBridgeName(),
		UplinkInterface: result.GetUplinkInterface(),
		NetworkBackend:  int32(result.GetNetworkBackend()),
	})
	if err != nil {
		return fmt.Errorf("encode Open vSwitch result: %w", err)
	}
	progress.SetResult(string(encodedResult))
	return nil
}

func (s *NodeAgentServer) reconcileOpenVSwitchLocked(
	ctx context.Context,
) (*pb.OpenVSwitchReconciliationResult, *openVSwitchPlan, error) {
	if err := s.ensureOpenVSwitchService(); err != nil {
		return nil, nil, err
	}
	defaultRoute, err := s.readOpenVSwitchDefaultRoute()
	if err != nil {
		return nil, nil, fmt.Errorf("discover default route: %w", err)
	}
	bridges, err := s.listOpenVSwitchBridges()
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "list Open vSwitch bridges: %v", err)
	}
	if strings.HasPrefix(defaultRoute.Device, openVSwitchBridgePrefix) && slices.Contains(bridges, defaultRoute.Device) {
		return &pb.OpenVSwitchReconciliationResult{
			Outcome:    pb.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED,
			BridgeName: defaultRoute.Device,
		}, nil, nil
	}

	link, err := s.readOpenVSwitchLink(defaultRoute.Device)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "discover default-route interface %s: %v", defaultRoute.Device, err)
	}
	bridgeName := openVSwitchBridgePrefix + link.Name
	if len(bridgeName) > 15 {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "derived bridge name %q exceeds the Linux interface-name limit", bridgeName)
	}
	plan, err := s.buildOpenVSwitchPlan(link, defaultRoute.Gateway, bridgeName)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "build Open vSwitch network plan: %v", err)
	}
	if err := s.regenerateOpenVSwitchSystemID(); err != nil {
		return nil, nil, status.Errorf(codes.Internal, "regenerate Open vSwitch system ID: %v", err)
	}
	if err := s.applyOpenVSwitchPlan(ctx, plan); err != nil {
		return nil, &plan, err
	}

	slog.InfoContext(ctx, "reconciled Open vSwitch bridge",
		"bridge", plan.bridgeName,
		"uplink", plan.uplinkName,
		"networkBackend", plan.backend.String(),
	)
	return configuredOpenVSwitchResult(plan), &plan, nil
}

func configuredOpenVSwitchResult(plan openVSwitchPlan) *pb.OpenVSwitchReconciliationResult {
	return &pb.OpenVSwitchReconciliationResult{
		Outcome:         pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED,
		BridgeName:      plan.bridgeName,
		UplinkInterface: plan.uplinkName,
		NetworkBackend:  plan.backend,
	}
}

func (s *NodeAgentServer) ensureOpenVSwitchService() error {
	output, err := s.Runner.Run("dpkg-query", []string{"--show", "--showformat=${db:Status-Abbrev}", openVSwitchPackage}, nil)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(output)), "ii") {
		return status.Error(codes.FailedPrecondition, "openvswitch-switch is not installed")
	}
	if _, err := s.runOpenVSwitchCommand("systemctl", "enable", "--now", openVSwitchService); err != nil {
		return status.Errorf(codes.Internal, "enable %s: %v", openVSwitchService, err)
	}
	return nil
}

func (s *NodeAgentServer) readOpenVSwitchDefaultRoute() (openVSwitchRoute, error) {
	output, err := s.runOpenVSwitchCommand(openVSwitchIPCommand, "-j", "route", "show", "default")
	if err != nil {
		return openVSwitchRoute{}, err
	}
	var routes []openVSwitchRoute
	if err := json.Unmarshal(output, &routes); err != nil {
		return openVSwitchRoute{}, fmt.Errorf("parse route JSON: %w", err)
	}
	for _, route := range routes {
		if route.Destination == openVSwitchDefaultRoute && route.Device != "" {
			return route, nil
		}
	}
	return openVSwitchRoute{}, errOpenVSwitchDefaultRouteMissing
}

func (s *NodeAgentServer) readOpenVSwitchLink(name string) (openVSwitchLink, error) {
	output, err := s.runOpenVSwitchCommand(openVSwitchIPCommand, "-j", "link", "show", openVSwitchDeviceFlag, name)
	if err != nil {
		return openVSwitchLink{}, err
	}
	var links []openVSwitchLink
	if err := json.Unmarshal(output, &links); err != nil {
		return openVSwitchLink{}, fmt.Errorf("parse link JSON: %w", err)
	}
	if len(links) != 1 || links[0].Name == "" {
		return openVSwitchLink{}, fmt.Errorf("expected one link named %s, got %d", name, len(links))
	}
	if links[0].MTU <= 0 {
		links[0].MTU = openVSwitchDefaultMTU
	}
	return links[0], nil
}

func (s *NodeAgentServer) listOpenVSwitchBridges() ([]string, error) {
	output, err := s.runOpenVSwitchCommand("ovs-vsctl", "list-br")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(output)), nil
}

func (s *NodeAgentServer) regenerateOpenVSwitchSystemID() error {
	for _, path := range []string{openVSwitchSystemIDPath, openVSwitchInstallUUIDPath} {
		if err := os.Remove(s.rootedPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	_, err := s.runOpenVSwitchCommand("systemctl", "restart", openVSwitchDaemonService)
	return err
}

func (s *NodeAgentServer) buildOpenVSwitchPlan(link openVSwitchLink, gateway, bridgeName string) (openVSwitchPlan, error) {
	plan, found, err := s.buildOpenVSwitchIfupdownPlan(link, gateway, bridgeName)
	if err != nil || found {
		return plan, err
	}
	return s.buildOpenVSwitchNetplanPlan(link, gateway, bridgeName)
}

func (s *NodeAgentServer) applyOpenVSwitchPlan(ctx context.Context, plan openVSwitchPlan) error {
	var err error
	switch plan.backend {
	case pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN:
		err = s.applyOpenVSwitchNetplanPlan(ctx, plan)
	case pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_IFUPDOWN:
		err = s.applyOpenVSwitchIfupdownPlan(plan)
	case pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_UNSPECIFIED:
		return errors.New("Open vSwitch network backend is unspecified")
	default:
		return fmt.Errorf("unsupported Open vSwitch network backend %s", plan.backend)
	}
	if err != nil {
		return err
	}
	return s.verifyOpenVSwitchPlan(plan)
}

func (s *NodeAgentServer) verifyOpenVSwitchPlan(plan openVSwitchPlan) error {
	bridges, err := s.listOpenVSwitchBridges()
	if err != nil {
		return err
	}
	if !slices.Contains(bridges, plan.bridgeName) {
		return fmt.Errorf("%w: bridge %s is not present", errOpenVSwitchNotConverged, plan.bridgeName)
	}
	route, err := s.readOpenVSwitchDefaultRoute()
	if err != nil {
		return fmt.Errorf("%w: %v", errOpenVSwitchNotConverged, err)
	}
	if route.Device != plan.bridgeName {
		return fmt.Errorf("%w: default route uses %s instead of bridge %s", errOpenVSwitchNotConverged, route.Device, plan.bridgeName)
	}
	return nil
}

func (s *NodeAgentServer) runOpenVSwitchCommand(name string, args ...string) ([]byte, error) {
	output, err := s.Runner.Run(name, args, nil)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (s *NodeAgentServer) openVSwitchSettleDelayOrDefault() time.Duration {
	if s.openVSwitchSettleDelay > 0 {
		return s.openVSwitchSettleDelay
	}
	return openVSwitchDefaultSettleTime
}

func (s *NodeAgentServer) openVSwitchConvergenceDelayOrDefault() time.Duration {
	if s.openVSwitchConvergenceDelay > 0 {
		return s.openVSwitchConvergenceDelay
	}
	return openVSwitchConvergenceDelay
}

func (s *NodeAgentServer) recoverInterruptedOpenVSwitchReconciliation(operationID string) error {
	s.mu.Lock()
	s.ensureNodeAgentOperationStateLocked()
	op, found := s.NodeStatus.NodeAgentOperations[operationID]
	_, active := s.activeOperations[operationID]
	stale := found && op.Type == operationTypeOpenVSwitchReconciliation &&
		!nodeAgentOperationTerminal(op.Phase) && !active
	if stale {
		delete(s.NodeStatus.NodeAgentOperations, operationID)
	}
	statusCopy := s.nodeStatusSnapshotLocked()
	s.mu.Unlock()
	if !stale {
		return nil
	}
	if err := s.Persister.Persist(&statusCopy); err != nil {
		return status.Errorf(codes.Internal, "persist recovered Open vSwitch operation: %v", err)
	}
	return nil
}

func openVSwitchOperationToProto(op NodeAgentOperation) (*pb.OpenVSwitchReconciliationOperation, error) {
	operation := &pb.OpenVSwitchReconciliationOperation{
		Operation: nodeAgentOperationToProto(op),
	}
	if op.Result == "" {
		return operation, nil
	}
	var result persistedOpenVSwitchResult
	if err := json.Unmarshal([]byte(op.Result), &result); err != nil {
		return nil, fmt.Errorf("decode persisted result: %w", err)
	}
	operation.Result = &pb.OpenVSwitchReconciliationResult{
		Outcome:         pb.OpenVSwitchReconciliationResult_Outcome(result.Outcome),
		BridgeName:      result.BridgeName,
		UplinkInterface: result.UplinkInterface,
		NetworkBackend:  pb.OpenVSwitchReconciliationResult_NetworkBackend(result.NetworkBackend),
	}
	return operation, nil
}
