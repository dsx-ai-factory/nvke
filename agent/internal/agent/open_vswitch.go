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

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
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
	backend        pb.ReconcileOpenVSwitchResponse_NetworkBackend
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

// ReconcileOpenVSwitch preserves the default-route interface's current L3
// configuration while moving it onto an OVS bridge. The network transition can
// temporarily sever this RPC connection, so the on-disk desired state is
// written before activation and recognized on every retry.
func (s *NodeAgentServer) ReconcileOpenVSwitch(
	ctx context.Context,
	req *pb.ReconcileOpenVSwitchRequest,
) (*pb.ReconcileOpenVSwitchResponse, error) {
	defer logRPCSpan(ctx, "ReconcileOpenVSwitch")()

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if s.Runner == nil {
		return nil, status.Error(codes.FailedPrecondition, "command runner is not configured")
	}

	s.openVSwitchMu.Lock()
	defer s.openVSwitchMu.Unlock()

	if err := s.ensureOpenVSwitchService(); err != nil {
		return nil, err
	}
	defaultRoute, err := s.readOpenVSwitchDefaultRoute()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "discover default route: %v", err)
	}
	bridges, err := s.listOpenVSwitchBridges()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list Open vSwitch bridges: %v", err)
	}
	if strings.HasPrefix(defaultRoute.Device, openVSwitchBridgePrefix) && slices.Contains(bridges, defaultRoute.Device) {
		return &pb.ReconcileOpenVSwitchResponse{
			Outcome:    pb.ReconcileOpenVSwitchResponse_OUTCOME_UNCHANGED,
			BridgeName: defaultRoute.Device,
		}, nil
	}

	link, err := s.readOpenVSwitchLink(defaultRoute.Device)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "discover default-route interface %s: %v", defaultRoute.Device, err)
	}
	bridgeName := openVSwitchBridgePrefix + link.Name
	if len(bridgeName) > 15 {
		return nil, status.Errorf(codes.FailedPrecondition, "derived bridge name %q exceeds the Linux interface-name limit", bridgeName)
	}
	plan, err := s.buildOpenVSwitchPlan(link, defaultRoute.Gateway, bridgeName)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "build Open vSwitch network plan: %v", err)
	}
	if err := s.regenerateOpenVSwitchSystemID(); err != nil {
		return nil, status.Errorf(codes.Internal, "regenerate Open vSwitch system ID: %v", err)
	}
	if err := s.applyOpenVSwitchPlan(ctx, plan); err != nil {
		return nil, status.Errorf(codes.Internal, "apply Open vSwitch network plan: %v", err)
	}

	slog.InfoContext(ctx, "reconciled Open vSwitch bridge",
		"bridge", plan.bridgeName,
		"uplink", plan.uplinkName,
		"networkBackend", plan.backend.String(),
	)
	return &pb.ReconcileOpenVSwitchResponse{
		Outcome:         pb.ReconcileOpenVSwitchResponse_OUTCOME_CONFIGURED,
		BridgeName:      plan.bridgeName,
		UplinkInterface: plan.uplinkName,
		NetworkBackend:  plan.backend,
	}, nil
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
	return openVSwitchRoute{}, errors.New("no default route with an interface is present")
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
	case pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_NETPLAN:
		err = s.applyOpenVSwitchNetplanPlan(ctx, plan)
	case pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_IFUPDOWN:
		err = s.applyOpenVSwitchIfupdownPlan(plan)
	case pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_UNSPECIFIED:
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
		return fmt.Errorf("bridge %s is not present after reconciliation", plan.bridgeName)
	}
	route, err := s.readOpenVSwitchDefaultRoute()
	if err != nil {
		return err
	}
	if route.Device != plan.bridgeName {
		return fmt.Errorf("default route uses %s instead of bridge %s", route.Device, plan.bridgeName)
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
