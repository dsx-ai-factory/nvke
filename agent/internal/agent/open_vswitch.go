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

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	openVSwitchPackage         = "openvswitch-switch"
	openVSwitchService         = "openvswitch-switch"
	openVSwitchDaemonService   = "ovs-vswitchd"
	openVSwitchSystemIDPath    = "/etc/openvswitch/system-id.conf"
	openVSwitchInstallUUIDPath = "/etc/openvswitch/install_uuid.conf"
	openVSwitchBridgePrefix    = "br"
	// openVSwitchUplinkPrefix is what the bridge prefix replaces on a
	// predictably-named NIC. Both are two characters, which is what keeps the
	// derived name inside interfaceNameMax.
	openVSwitchUplinkPrefix = "en"
	// interfaceNameMax is IFNAMSIZ-1: the kernel's buffer is 16 bytes and
	// includes the NUL, so 15 characters is the longest name it accepts.
	interfaceNameMax              = 15
	openVSwitchIPCommand          = "ip"
	openVSwitchDeviceFlag         = "dev"
	openVSwitchDefaultRoute       = "default"
	openVSwitchDefaultMTU         = 1500
	openVSwitchDefaultSettleTime  = 2 * time.Second
	openVSwitchConvergenceDelay   = time.Second
	openVSwitchDatapathTypeEnv    = "NKE_OVS_DATAPATH_TYPE"
	openVSwitchNetdevDatapath     = "netdev"
	openVSwitchSystemDatapath     = "system"
	openVSwitchDummyGatewayBridge = "br-ex"
	openVSwitchUserspaceTSOKey    = "userspace-tso-enable"

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
	if err := s.resetRestartableOpenVSwitchReconciliation(operationID); err != nil {
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
	if result, handled, err := s.reconcileOpenVSwitchDatapathOverride(ctx); handled {
		return result, nil, err
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
	bridgeName, err := openVSwitchBridgeName(link.Name)
	if err != nil {
		return nil, nil, err
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

func (s *NodeAgentServer) reconcileOpenVSwitchDatapathOverride(
	ctx context.Context,
) (*pb.OpenVSwitchReconciliationResult, bool, error) {
	datapathType := strings.TrimSpace(os.Getenv(openVSwitchDatapathTypeEnv))
	if datapathType == "" {
		return nil, false, nil
	}
	if datapathType != openVSwitchNetdevDatapath && datapathType != openVSwitchSystemDatapath {
		return nil, true, fmt.Errorf("unsupported Open vSwitch datapath type %q", datapathType)
	}
	defaultRoute, err := s.readOpenVSwitchDefaultRoute()
	if err != nil {
		return nil, true, fmt.Errorf("discover default route: %w", err)
	}
	if _, err := s.runOpenVSwitchCommand(
		"ovs-vsctl",
		"set", "Open_vSwitch", ".", "external_ids:ovn-bridge-datapath-type="+datapathType,
	); err != nil {
		return nil, true, fmt.Errorf("configure Open vSwitch %s datapath: %w", datapathType, err)
	}
	// The userspace datapath reads offload metadata only when this is set.
	// The kernel datapath checksums packets itself.
	if datapathType == openVSwitchNetdevDatapath {
		if err := s.ensureOpenVSwitchUserspaceTSO(); err != nil {
			return nil, true, err
		}
	}
	if _, err := s.runOpenVSwitchCommand(
		"ovs-vsctl",
		"--may-exist", "add-br", openVSwitchDummyGatewayBridge, "--",
		"set", "bridge", openVSwitchDummyGatewayBridge, "datapath_type="+datapathType,
	); err != nil {
		return nil, true, fmt.Errorf("configure dummy gateway bridge for Open vSwitch %s datapath: %w", datapathType, err)
	}
	slog.InfoContext(ctx, "configured Open vSwitch datapath without bridging the management uplink",
		"datapathType", datapathType,
		"uplink", defaultRoute.Device,
	)
	return &pb.OpenVSwitchReconciliationResult{
		Outcome:         pb.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED,
		UplinkInterface: defaultRoute.Device,
		NetworkBackend:  pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN,
	}, true, nil
}

// ensureOpenVSwitchUserspaceTSO lets the netdev datapath read the offload
// metadata on veth pod ports. Without it, packets a pod sends with a partial
// TCP or UDP checksum are forwarded unfinished and dropped by the receiver, so
// pods cannot reach services or the node. ovs-vswitchd reads the setting only
// at startup, so it is restarted when the value changes.
func (s *NodeAgentServer) ensureOpenVSwitchUserspaceTSO() error {
	output, err := s.runOpenVSwitchCommand(
		"ovs-vsctl", "--if-exists", "get", "Open_vSwitch", ".", "other_config:"+openVSwitchUserspaceTSOKey,
	)
	if err != nil {
		return fmt.Errorf("read Open vSwitch %s: %w", openVSwitchUserspaceTSOKey, err)
	}
	previous := strings.Trim(strings.TrimSpace(string(output)), `"`)
	if previous == "true" {
		return nil
	}
	if _, err := s.runOpenVSwitchCommand(
		"ovs-vsctl", "set", "Open_vSwitch", ".", "other_config:"+openVSwitchUserspaceTSOKey+"=true",
	); err != nil {
		return fmt.Errorf("enable Open vSwitch %s: %w", openVSwitchUserspaceTSOKey, err)
	}
	if _, err := s.runOpenVSwitchCommand("systemctl", "restart", openVSwitchDaemonService); err != nil {
		// ovs-vswitchd reads the setting only at startup. Leaving it set would
		// make the next attempt look unchanged and skip the restart.
		if restoreErr := s.restoreOpenVSwitchUserspaceTSO(previous); restoreErr != nil {
			return fmt.Errorf("restart %s to apply %s: %w; restoring previous value: %v", openVSwitchDaemonService, openVSwitchUserspaceTSOKey, err, restoreErr)
		}
		return fmt.Errorf("restart %s to apply %s: %w", openVSwitchDaemonService, openVSwitchUserspaceTSOKey, err)
	}
	return nil
}

// restoreOpenVSwitchUserspaceTSO puts the setting back so a failed daemon
// restart is attempted again. An empty previous value means the key was unset.
func (s *NodeAgentServer) restoreOpenVSwitchUserspaceTSO(previous string) error {
	if previous == "" {
		_, err := s.runOpenVSwitchCommand("ovs-vsctl", "remove", "Open_vSwitch", ".", "other_config", openVSwitchUserspaceTSOKey)
		return err
	}
	_, err := s.runOpenVSwitchCommand(
		"ovs-vsctl", "set", "Open_vSwitch", ".", "other_config:"+openVSwitchUserspaceTSOKey+"="+previous,
	)
	return err
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

// openVSwitchBridgeName is the bridge that carries uplink's addresses.
//
// Prefixing is kept wherever it fits, so a fleet that already runs brens1np0
// keeps that name and nothing is renamed by an upgrade. Only when the prefix
// would overflow is the uplink's leading "en" substituted instead: both
// affixes are two characters, so the result is always within the limit for
// any name the kernel can hand us. enP13p65s0f0np0 on a GB200 tray is
// exactly at the limit, and brenP13p65s0f0np0 is two over.
//
// Truncating would also fit and is worse: it would map every uplink sharing a
// prefix onto one bridge. Substitution collides far more narrowly -- only a
// 13-character name that matches the tail of a 15-character "en" name -- and
// callers guard the remainder by refusing a bridge already mapped elsewhere.
//
// A name with nothing to substitute keeps the prefix and fails the same check
// rather than being truncated into a plausible but wrong interface.
func openVSwitchBridgeName(uplink string) (string, error) {
	bridge := openVSwitchBridgePrefix + uplink
	if len(bridge) > interfaceNameMax {
		if after, found := strings.CutPrefix(uplink, openVSwitchUplinkPrefix); found {
			bridge = openVSwitchBridgePrefix + after
		}
	}
	if len(bridge) > interfaceNameMax {
		return "", status.Errorf(codes.FailedPrecondition,
			"derived bridge name %q exceeds the Linux interface-name limit", bridge)
	}
	return bridge, nil
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

// resetRestartableOpenVSwitchReconciliation drops a persisted reconciliation
// that cannot make progress, so the Start that follows runs it again: one left
// non-terminal by an agent restart, or one that FAILED. A failure here is
// usually transient (a systemctl call during a systemd re-exec, a default-route
// gap), and the controller retries by calling Start with the same operation ID;
// a kept FAILED record would answer every retry without running anything.
// SUCCEEDED stays, so a repeated Start after success remains idempotent.
func (s *NodeAgentServer) resetRestartableOpenVSwitchReconciliation(operationID string) error {
	s.mu.Lock()
	s.ensureNodeAgentOperationStateLocked()
	op, found := s.NodeStatus.NodeAgentOperations[operationID]
	_, active := s.activeOperations[operationID]
	stale := found && op.Type == operationTypeOpenVSwitchReconciliation && !active &&
		(!nodeAgentOperationTerminal(op.Phase) || op.Phase == NodeAgentOperationPhaseFailed)
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
