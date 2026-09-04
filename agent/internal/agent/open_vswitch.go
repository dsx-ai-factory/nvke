// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

const (
	openVSwitchPackage           = "openvswitch-switch"
	openVSwitchService           = "openvswitch-switch"
	openVSwitchDaemonService     = "ovs-vswitchd"
	openVSwitchSystemIDPath      = "/etc/openvswitch/system-id.conf"
	openVSwitchInstallUUIDPath   = "/etc/openvswitch/install_uuid.conf"
	openVSwitchNetplanDir        = "/etc/netplan"
	openVSwitchInterfacesPath    = "/etc/network/interfaces"
	openVSwitchInterfacesDir     = "/etc/network/interfaces.d"
	openVSwitchBridgePrefix      = "br"
	openVSwitchIPCommand         = "ip"
	openVSwitchIPAddrCommand     = "addr"
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

func (s *NodeAgentServer) buildOpenVSwitchNetplanPlan(link openVSwitchLink, gateway, bridgeName string) (openVSwitchPlan, error) {
	paths, err := filepath.Glob(filepath.Join(s.rootedPath(openVSwitchNetplanDir), "*.yaml"))
	if err != nil {
		return openVSwitchPlan{}, fmt.Errorf("list netplan files: %w", err)
	}
	if len(paths) == 0 {
		return openVSwitchPlan{}, errors.New("neither ifupdown nor netplan configuration is present")
	}

	type candidate struct {
		path       string
		config     map[string]any
		uplinkName string
		pending    bool
	}
	var candidates []candidate
	var allENCandidates []candidate
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return openVSwitchPlan{}, fmt.Errorf("read %s: %w", path, readErr)
		}
		config := map[string]any{}
		if unmarshalErr := yaml.Unmarshal(data, &config); unmarshalErr != nil {
			return openVSwitchPlan{}, fmt.Errorf("parse %s: %w", path, unmarshalErr)
		}
		network := nestedStringMap(config, "network")
		ethernets := nestedStringMap(network, "ethernets")
		bridges := nestedStringMap(network, "bridges")
		// netplan may have been written before the connection dropped. Treat
		// that bridge stanza as pending desired state and retry activation.
		if openVSwitchBridgeUsesInterface(bridges[bridgeName], link.Name) {
			candidates = append(candidates, candidate{path: path, config: config, uplinkName: link.Name, pending: true})
			continue
		}
		for name, raw := range ethernets {
			stanza, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if name == link.Name || openVSwitchStanzaMatchesLink(stanza, link) {
				candidates = append(candidates, candidate{path: path, config: config, uplinkName: name})
			}
			if name == "all-en" {
				allENCandidates = append(allENCandidates, candidate{path: path, config: config, uplinkName: name})
			}
		}
	}
	if len(candidates) == 0 {
		candidates = allENCandidates
	}
	if len(candidates) != 1 {
		return openVSwitchPlan{}, fmt.Errorf("expected one netplan configuration for %s, got %d", link.Name, len(candidates))
	}

	selected := candidates[0]
	network := nestedStringMap(selected.config, "network")
	ethernets := nestedStringMap(network, "ethernets")
	bridges := nestedStringMap(network, "bridges")
	if !selected.pending {
		rawStanza, ok := ethernets[selected.uplinkName]
		if !ok {
			return openVSwitchPlan{}, fmt.Errorf("netplan interface %s disappeared during reconciliation", selected.uplinkName)
		}
		bridgeStanza, ok := rawStanza.(map[string]any)
		if !ok {
			return openVSwitchPlan{}, fmt.Errorf("netplan interface %s is not an object", selected.uplinkName)
		}
		// Move the complete L3 stanza to the bridge so DHCP, static addresses,
		// routes, and DNS survive. The physical NIC keeps only its L2 identity.
		bridgeStanza = cloneStringMap(bridgeStanza)
		delete(bridgeStanza, "match")
		delete(bridgeStanza, "set-name")
		delete(bridgeStanza, "macaddress")
		bridgeStanza["interfaces"] = []string{link.Name}
		bridgeStanza["openvswitch"] = map[string]any{}
		bridges[bridgeName] = bridgeStanza
		delete(ethernets, selected.uplinkName)
		physical := map[string]any{"mtu": link.MTU}
		if link.Address != "" {
			physical["match"] = map[string]any{"macaddress": link.Address}
		}
		ethernets[link.Name] = physical
	}
	network["ethernets"] = ethernets
	network["bridges"] = bridges
	selected.config["network"] = network
	data, err := yaml.Marshal(selected.config)
	if err != nil {
		return openVSwitchPlan{}, fmt.Errorf("render %s: %w", selected.path, err)
	}
	return openVSwitchPlan{
		backend:        pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_NETPLAN,
		bridgeName:     bridgeName,
		uplinkName:     link.Name,
		uplinkMAC:      link.Address,
		uplinkMTU:      link.MTU,
		defaultGateway: gateway,
		configPath:     selected.path,
		config:         data,
	}, nil
}

func nestedStringMap(parent map[string]any, key string) map[string]any {
	if child, ok := parent[key].(map[string]any); ok {
		return child
	}
	child := map[string]any{}
	parent[key] = child
	return child
}

func cloneStringMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func openVSwitchStanzaMatchesLink(stanza map[string]any, link openVSwitchLink) bool {
	match, _ := stanza["match"].(map[string]any)
	if name, _ := match["name"].(string); name == link.Name {
		return true
	}
	mac, _ := match["macaddress"].(string)
	return link.Address != "" && strings.EqualFold(mac, link.Address)
}

func openVSwitchBridgeUsesInterface(raw any, interfaceName string) bool {
	stanza, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	interfaces, ok := stanza["interfaces"].([]any)
	if !ok {
		if typed, typedOK := stanza["interfaces"].([]string); typedOK {
			return slices.Contains(typed, interfaceName)
		}
		return false
	}
	for _, current := range interfaces {
		if current == interfaceName {
			return true
		}
	}
	return false
}

func (s *NodeAgentServer) buildOpenVSwitchIfupdownPlan(link openVSwitchLink, gateway, bridgeName string) (openVSwitchPlan, bool, error) {
	paths := []string{s.rootedPath(openVSwitchInterfacesPath)}
	entries, err := os.ReadDir(s.rootedPath(openVSwitchInterfacesDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return openVSwitchPlan{}, false, fmt.Errorf("list ifupdown configurations: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			paths = append(paths, filepath.Join(s.rootedPath(openVSwitchInterfacesDir), entry.Name()))
		}
	}

	var sourcePath string
	var sourceConfig []byte
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return openVSwitchPlan{}, false, fmt.Errorf("read %s: %w", path, readErr)
		}
		if strings.Contains(string(data), "iface "+link.Name+" ") {
			if sourcePath != "" {
				return openVSwitchPlan{}, true, fmt.Errorf("multiple ifupdown files configure %s", link.Name)
			}
			sourcePath, sourceConfig = path, data
		}
	}
	if sourcePath == "" {
		return openVSwitchPlan{}, false, nil
	}

	address := openVSwitchIfupdownValue(sourceConfig, "address")
	netmask := openVSwitchIfupdownValue(sourceConfig, "netmask")
	configuredGateway := openVSwitchIfupdownValue(sourceConfig, "gateway")
	if configuredGateway != "" {
		gateway = configuredGateway
	}
	bridgePath := filepath.Join(s.rootedPath(openVSwitchInterfacesDir), "ifcfg-"+bridgeName)
	if address == "" || netmask == "" {
		bridgeConfig, readErr := os.ReadFile(bridgePath)
		if readErr != nil {
			return openVSwitchPlan{}, true, fmt.Errorf("parse address and netmask from %s: %w", sourcePath, readErr)
		}
		address = openVSwitchIfupdownValue(bridgeConfig, "address")
		netmask = openVSwitchIfupdownValue(bridgeConfig, "netmask")
		if configuredGateway := openVSwitchIfupdownValue(bridgeConfig, "gateway"); configuredGateway != "" {
			gateway = configuredGateway
		}
	}
	if address == "" || netmask == "" || gateway == "" {
		return openVSwitchPlan{}, true, fmt.Errorf("ifupdown configuration for %s requires address, netmask, and gateway", link.Name)
	}
	config := []byte(fmt.Sprintf("auto %s\niface %s inet manual\n\tmtu %d\n", link.Name, link.Name, link.MTU))
	return openVSwitchPlan{
		backend:        pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_IFUPDOWN,
		bridgeName:     bridgeName,
		uplinkName:     link.Name,
		uplinkMAC:      link.Address,
		uplinkMTU:      link.MTU,
		defaultGateway: gateway,
		configPath:     sourcePath,
		config:         config,
		address:        address,
		netmask:        netmask,
	}, true, nil
}

func openVSwitchIfupdownValue(config []byte, key string) string {
	for _, line := range strings.Split(string(config), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			return fields[1]
		}
	}
	return ""
}

func (s *NodeAgentServer) applyOpenVSwitchPlan(ctx context.Context, plan openVSwitchPlan) error {
	if plan.backend == pb.ReconcileOpenVSwitchResponse_NETWORK_BACKEND_NETPLAN {
		if err := atomicWriteFile(plan.configPath, plan.config, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", plan.configPath, err)
		}
		if _, err := s.runOpenVSwitchCommand("netplan", "generate"); err != nil {
			return fmt.Errorf("validate netplan: %w", err)
		}
		if _, err := s.runOpenVSwitchCommand("netplan", "apply"); err != nil {
			return fmt.Errorf("apply netplan: %w", err)
		}
		if err := sleepOrDone(ctx, s.openVSwitchSettleDelayOrDefault()); err != nil {
			return err
		}
	} else {
		if err := s.applyOpenVSwitchIfupdownPlan(plan); err != nil {
			return err
		}
	}
	return s.verifyOpenVSwitchPlan(plan)
}

func (s *NodeAgentServer) applyOpenVSwitchIfupdownPlan(plan openVSwitchPlan) error {
	prefix, err := openVSwitchNetmaskPrefix(plan.netmask)
	if err != nil {
		return err
	}
	bridgeConfig := []byte(fmt.Sprintf("auto %s\niface %s inet static\n\taddress %s\n\tnetmask %s\n\tgateway %s\n\tmtu %d\n\tovs_type OVSBridge\n\tovs_ports %s\n",
		plan.bridgeName, plan.bridgeName, plan.address, plan.netmask, plan.defaultGateway, plan.uplinkMTU, plan.uplinkName))
	bridgePath := filepath.Join(s.rootedPath(openVSwitchInterfacesDir), "ifcfg-"+plan.bridgeName)
	// Persist the bridge stanza before reducing the uplink stanza to L2. A
	// retry can recover values from this file if the process stops between the
	// two atomic writes; the opposite order would discard the only L3 copy.
	if err := atomicWriteFile(bridgePath, bridgeConfig, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", bridgePath, err)
	}
	if err := atomicWriteFile(plan.configPath, plan.config, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", plan.configPath, err)
	}
	if _, err := s.runOpenVSwitchCommand("ovs-vsctl", "--may-exist", "add-br", plan.bridgeName); err != nil {
		return err
	}
	// OVS auto-inherits globally unique NIC MACs, but not locally administered
	// ones. Pinning the latter keeps DHCP identity stable across the bridge move.
	if openVSwitchMACIsLocallyAdministered(plan.uplinkMAC) {
		if _, err := s.runOpenVSwitchCommand("ovs-vsctl", "set", "bridge", plan.bridgeName, "other-config:hwaddr="+plan.uplinkMAC); err != nil {
			return err
		}
	}
	if _, err := s.runOpenVSwitchCommand("ovs-vsctl", "--may-exist", "add-port", plan.bridgeName, plan.uplinkName); err != nil {
		return err
	}
	commands := [][]string{
		{openVSwitchIPCommand, openVSwitchIPAddrCommand, "replace", plan.address + "/" + strconv.Itoa(prefix), openVSwitchDeviceFlag, plan.bridgeName},
		{openVSwitchIPCommand, "link", "set", plan.bridgeName, "up"},
		{openVSwitchIPCommand, "link", "set", plan.bridgeName, "mtu", strconv.Itoa(plan.uplinkMTU)},
		{openVSwitchIPCommand, "route", "replace", "default", "via", plan.defaultGateway, openVSwitchDeviceFlag, plan.bridgeName, "onlink"},
		{openVSwitchIPCommand, openVSwitchIPAddrCommand, "flush", openVSwitchDeviceFlag, plan.uplinkName},
	}
	for _, command := range commands {
		if _, err := s.runOpenVSwitchCommand(command[0], command[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func openVSwitchNetmaskPrefix(mask string) (int, error) {
	ip := net.ParseIP(mask).To4()
	if ip == nil {
		return 0, fmt.Errorf("invalid IPv4 netmask %q", mask)
	}
	ones, bits := net.IPMask(ip).Size()
	if bits != 32 || ones == 0 {
		return 0, fmt.Errorf("invalid contiguous IPv4 netmask %q", mask)
	}
	return ones, nil
}

func openVSwitchMACIsLocallyAdministered(address string) bool {
	mac, err := net.ParseMAC(address)
	return err == nil && len(mac) > 0 && mac[0]&2 != 0
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
