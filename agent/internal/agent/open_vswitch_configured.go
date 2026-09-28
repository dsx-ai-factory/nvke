// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

type configuredOpenVSwitchInput struct {
	UplinkInterface string
	BridgeName      string
}

const (
	openVSwitchParentDirectory   = ".."
	openVSwitchIPv4Family        = "-4"
	openVSwitchIPv6Family        = "-6"
	openVSwitchIPv4DefaultPrefix = "0.0.0.0/0"
	openVSwitchIPv6DefaultPrefix = "::/0"
	openVSwitchNetplanGateway6   = "gateway6"
	openVSwitchRouteTo           = "to"
	openVSwitchRouteVia          = "via"
)

type openVSwitchNetworkExpectation struct {
	addresses     []netip.Prefix
	routes        []openVSwitchRoute
	dhcp4Required bool
	dhcp6Required bool
}

// StartConfiguredOpenVSwitchReconciliation starts OVS setup for an explicitly
// selected Netplan interface.
func (s *NodeAgentServer) StartConfiguredOpenVSwitchReconciliation(ctx context.Context, req *pb.StartConfiguredOpenVSwitchReconciliationRequest) (*pb.StartOpenVSwitchReconciliationResponse, error) {
	defer logRPCSpan(ctx, "StartConfiguredOpenVSwitchReconciliation")()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	input := configuredOpenVSwitchInput{UplinkInterface: req.GetUplinkInterface(), BridgeName: req.GetBridgeName()}
	if err := input.validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid Open vSwitch configuration: %v", err)
	}
	if s.Runner == nil || s.Persister == nil {
		return nil, status.Error(codes.FailedPrecondition, "command runner and status persister are required")
	}
	// Use the shared operation behavior after an agent restart: report the
	// interrupted operation.
	op, err := s.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   strings.TrimSpace(req.GetOperationId()),
		Type: operationTypeOpenVSwitchReconciliation,
	}, func(ctx context.Context, progress nodeAgentOperationProgress) error {
		return s.runOpenVSwitchOperation(ctx, progress, func(ctx context.Context) (*pb.OpenVSwitchReconciliationResult, *openVSwitchPlan, error) {
			return s.reconcileConfiguredOpenVSwitchLocked(ctx, input)
		})
	})
	if err != nil {
		return nil, err
	}
	operation, err := openVSwitchOperationToProto(op)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode Open vSwitch operation: %v", err)
	}
	return &pb.StartOpenVSwitchReconciliationResponse{Operation: operation}, nil
}

func (input configuredOpenVSwitchInput) validate() error {
	for _, name := range []string{input.UplinkInterface, input.BridgeName} {
		if name == "" || len(name) > 15 || name == "." || name == openVSwitchParentDirectory || name == loopbackInterface || strings.HasPrefix(name, "-") {
			return fmt.Errorf("invalid Linux interface name %q", name)
		}
		for _, char := range name {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
				return fmt.Errorf("invalid Linux interface name %q", name)
			}
		}
	}
	if input.UplinkInterface == input.BridgeName {
		return errors.New("uplink and bridge must be different interfaces")
	}
	return nil
}

func (s *NodeAgentServer) reconcileConfiguredOpenVSwitchLocked(ctx context.Context, input configuredOpenVSwitchInput) (*pb.OpenVSwitchReconciliationResult, *openVSwitchPlan, error) {
	// Build and validate before enabling services or writing host configuration.
	// Mixing backend ownership is unsafe even if a Netplan file also exists.
	if err := s.requireOpenVSwitchNetplanBackend(); err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "explicit Open vSwitch configuration: %v", err)
	}
	link, err := s.readOpenVSwitchLinkDetails(input.UplinkInterface, true)
	if err != nil || link.Name != input.UplinkInterface {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "discover requested uplink %s: %v", input.UplinkInterface, err)
	}
	if link.LinkInfo.Kind != "" {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "requested uplink %s is a virtual %s link; a physical Ethernet interface is required", link.Name, link.LinkInfo.Kind)
	}
	plan, err := s.buildConfiguredOpenVSwitchNetplanPlan(link, input.BridgeName)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "build explicit Open vSwitch plan: %v", err)
	}
	if err := s.ensureOpenVSwitchService(); err != nil {
		return nil, nil, err
	}
	bridges, err := s.listOpenVSwitchBridges()
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(bridges, input.BridgeName) {
		if err := s.requireUnusedOpenVSwitchBridgeName(input.BridgeName); err != nil {
			return nil, nil, err
		}
	}
	if slices.Contains(bridges, input.UplinkInterface) {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "requested uplink %s is an OVS bridge", input.UplinkInterface)
	}
	for _, bridge := range bridges {
		ports, err := s.runOpenVSwitchCommand("ovs-vsctl", "list-ports", bridge)
		if err != nil {
			return nil, nil, err
		}
		if bridge != input.BridgeName && slices.Contains(strings.Fields(string(ports)), input.UplinkInterface) {
			return nil, nil, status.Errorf(codes.FailedPrecondition, "requested uplink %s already belongs to bridge %s", input.UplinkInterface, bridge)
		}
		if bridge == input.BridgeName {
			if err := s.validateConfiguredOpenVSwitchPorts(input, strings.Fields(string(ports)), plan.configMatchesPlan); err != nil {
				return nil, nil, err
			}
		}
	}
	if plan.configMatchesPlan {
		if err := s.verifyConfiguredOpenVSwitchPlan(plan); err == nil {
			result := configuredOpenVSwitchResult(plan)
			result.Outcome = pb.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED
			return result, &plan, nil
		} else if !errors.Is(err, errOpenVSwitchNotConverged) {
			return nil, nil, err
		}
	}
	if len(bridges) == 0 {
		if err := s.regenerateOpenVSwitchSystemID(); err != nil {
			return nil, nil, fmt.Errorf("regenerate Open vSwitch system ID: %w", err)
		}
	}
	if err := s.applyOpenVSwitchPlan(ctx, plan); err != nil {
		return nil, &plan, err
	}
	return configuredOpenVSwitchResult(plan), &plan, nil
}

func (s *NodeAgentServer) validateConfiguredOpenVSwitchPorts(input configuredOpenVSwitchInput, ports []string, configMatchesPlan bool) error {
	if len(ports) > 0 && !slices.Contains(ports, input.UplinkInterface) && !configMatchesPlan {
		return status.Errorf(codes.FailedPrecondition, "bridge %s contains different ports; refusing to adopt it", input.BridgeName)
	}
	for _, port := range ports {
		if port == input.UplinkInterface {
			continue
		}
		output, err := s.runOpenVSwitchCommand("ovs-vsctl", "get", "Interface", port, "type")
		if err != nil {
			return err
		}
		typeName := strings.Trim(strings.TrimSpace(string(output)), "\"")
		if typeName != "patch" && typeName != "internal" {
			return status.Errorf(codes.FailedPrecondition, "bridge %s contains another uplink %s; refusing to reconfigure it", input.BridgeName, port)
		}
	}
	return nil
}

func (s *NodeAgentServer) requireUnusedOpenVSwitchBridgeName(bridgeName string) error {
	data, err := s.runOpenVSwitchCommand(openVSwitchIPCommand, ipJSONFlag, openVSwitchLinkAction, openVSwitchShowAction)
	if err != nil {
		return err
	}
	var links []openVSwitchLink
	if err := json.Unmarshal(data, &links); err != nil {
		return fmt.Errorf("parse links before bridge creation: %w", err)
	}
	for _, link := range links {
		if link.Name == bridgeName {
			return status.Errorf(codes.FailedPrecondition, "requested bridge name %s is already used by a non-OVS interface", bridgeName)
		}
	}
	return nil
}

func (s *NodeAgentServer) requireOpenVSwitchNetplanBackend() error {
	interfacesPath := s.rootedPath(openVSwitchInterfacesPath)
	interfacesDir := s.rootedPath(openVSwitchInterfacesDir)
	ifupdownPaths := []string{interfacesPath}
	entries, err := os.ReadDir(interfacesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("list ifupdown configurations: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			ifupdownPaths = append(ifupdownPaths, filepath.Join(interfacesDir, entry.Name()))
		}
	}
	for _, path := range ifupdownPaths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		// Permit ordinary loopback-only files without allowing another backend
		// to configure network interfaces alongside Netplan.
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			switch strings.Join(strings.Fields(line), " ") {
			case "auto lo", "allow-auto lo", "iface lo inet loopback", "iface lo inet6 loopback":
				continue
			case "source " + openVSwitchInterfacesDir + "/*", "source-directory " + openVSwitchInterfacesDir,
				"source interfaces.d/*", "source-directory interfaces.d":
				// Only the main file may include the directory checked above.
				if path == interfacesPath {
					continue
				}
			}
			return fmt.Errorf("explicit selection supports Netplan only; ifupdown configuration is present in %s", path)
		}
	}
	return nil
}

func configuredOpenVSwitchExpectation(bridge map[string]any, bridgeName string) (*openVSwitchNetworkExpectation, error) {
	dhcp4, _ := bridge["dhcp4"].(bool)
	dhcp6, _ := bridge["dhcp6"].(bool)
	optionalAddresses, _ := bridge["optional-addresses"].([]any)
	// Optional DHCP families must not delay OVS readiness.
	expected := &openVSwitchNetworkExpectation{
		dhcp4Required: dhcp4 && !slices.Contains(optionalAddresses, "dhcp4"),
		dhcp6Required: dhcp6 && !slices.Contains(optionalAddresses, "dhcp6"),
	}
	if addresses, ok := bridge[openVSwitchNetplanAddresses].([]any); ok {
		for _, raw := range addresses {
			value, ok := raw.(string)
			if !ok {
				return nil, errors.New("explicit selection requires string-form Netplan addresses")
			}
			address, err := netip.ParsePrefix(value)
			if err != nil {
				return nil, fmt.Errorf("parse bridge address: %w", err)
			}
			expected.addresses = append(expected.addresses, address)
		}
	}
	if len(expected.addresses) == 0 && !dhcp4 && !dhcp6 {
		return nil, errors.New("selected interface requires static addresses or DHCP in Netplan")
	}
	for _, key := range []string{"gateway4", openVSwitchNetplanGateway6} {
		if gateway, _ := bridge[key].(string); gateway != "" {
			destination := openVSwitchIPv4DefaultPrefix
			if key == openVSwitchNetplanGateway6 {
				destination = openVSwitchIPv6DefaultPrefix
			}
			route, err := configuredOpenVSwitchRoute(map[string]any{openVSwitchRouteTo: destination, openVSwitchRouteVia: gateway}, bridgeName)
			if err != nil {
				return nil, err
			}
			expected.routes = append(expected.routes, route)
		}
	}
	if routes, ok := bridge["routes"].([]any); ok {
		for _, raw := range routes {
			route, err := configuredOpenVSwitchRoute(raw, bridgeName)
			if err != nil {
				return nil, err
			}
			expected.routes = append(expected.routes, route)
		}
	}
	return expected, nil
}

func configuredOpenVSwitchRoute(raw any, bridgeName string) (openVSwitchRoute, error) {
	route, ok := raw.(map[string]any)
	if !ok {
		return openVSwitchRoute{}, errors.New("Netplan route must be a mapping")
	}
	if routeType, _ := route["type"].(string); routeType != "" && routeType != "unicast" {
		return openVSwitchRoute{}, fmt.Errorf("explicit selection does not support Netplan route type %q", routeType)
	}
	destination, _ := route[openVSwitchRouteTo].(string)
	gateway, _ := route[openVSwitchRouteVia].(string)
	family := ""
	if prefix, err := netip.ParsePrefix(destination); err == nil {
		family = openVSwitchAddressFamily(prefix.Addr())
	} else if address, err := netip.ParseAddr(destination); err == nil {
		family = openVSwitchAddressFamily(address)
	}
	destination, err := normalizeOpenVSwitchDestination(destination)
	if err != nil {
		return openVSwitchRoute{}, fmt.Errorf("parse preserved route destination: %w", err)
	}
	if gateway != "" {
		address, err := netip.ParseAddr(gateway)
		if err != nil {
			return openVSwitchRoute{}, fmt.Errorf("parse preserved route gateway: %w", err)
		}
		gatewayFamily := openVSwitchAddressFamily(address)
		if family != "" && family != gatewayFamily {
			return openVSwitchRoute{}, errors.New("preserved route gateway and destination address families differ")
		}
		family = gatewayFamily
	}
	if family == "" {
		return openVSwitchRoute{}, errors.New("gatewayless default route requires an explicit IPv4 or IPv6 destination prefix")
	}
	metric, _ := route["metric"].(float64)
	table, _ := route["table"].(float64)
	return openVSwitchRoute{Destination: destination, Gateway: gateway, Device: bridgeName, Metric: int(metric), Table: int(table), Family: family}, nil
}

func openVSwitchAddressFamily(address netip.Addr) string {
	if address.Is4() {
		return openVSwitchIPv4Family
	}
	return openVSwitchIPv6Family
}

func (s *NodeAgentServer) readOpenVSwitchRoutes() ([]openVSwitchRoute, error) {
	var all []openVSwitchRoute
	for _, family := range []string{openVSwitchIPv4Family, openVSwitchIPv6Family} {
		data, err := s.runOpenVSwitchCommand("ip", "-N", ipJSONFlag, family, "route", openVSwitchShowAction, "table", "all")
		if err != nil {
			return nil, err
		}
		var routes []openVSwitchRoute
		if err := json.Unmarshal(data, &routes); err != nil {
			return nil, fmt.Errorf("parse routes: %w", err)
		}
		for i := range routes {
			routes[i].Family = family
		}
		all = append(all, routes...)
	}
	return all, nil
}

func (s *NodeAgentServer) verifyConfiguredOpenVSwitchPlan(plan openVSwitchPlan) error {
	bridges, err := s.listOpenVSwitchBridges()
	if err != nil {
		return err
	}
	if !slices.Contains(bridges, plan.bridgeName) {
		return fmt.Errorf("%w: bridge %s is missing", errOpenVSwitchNotConverged, plan.bridgeName)
	}
	ports, err := s.runOpenVSwitchCommand("ovs-vsctl", "list-ports", plan.bridgeName)
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Fields(string(ports)), plan.uplinkName) {
		return fmt.Errorf("%w: bridge %s does not contain uplink %s", errOpenVSwitchNotConverged, plan.bridgeName, plan.uplinkName)
	}
	for _, name := range []string{plan.bridgeName, plan.uplinkName} {
		link, err := s.readOpenVSwitchLink(name)
		if err != nil || link.MTU != plan.uplinkMTU {
			return fmt.Errorf("%w: interface %s must have MTU %d: %v", errOpenVSwitchNotConverged, name, plan.uplinkMTU, err)
		}
	}
	if err := s.verifyConfiguredOpenVSwitchAddresses(plan); err != nil {
		return err
	}
	if len(plan.expected.routes) == 0 {
		return nil
	}
	routes, err := s.readOpenVSwitchRoutes()
	if err != nil {
		return err
	}
	// Verify the routes configured for this bridge. DHCP and router advertisements
	// can change other routes while setup is running.
	for _, expected := range plan.expected.routes {
		if !slices.ContainsFunc(routes, func(actual openVSwitchRoute) bool { return openVSwitchRouteMatches(actual, expected) }) {
			return fmt.Errorf("%w: missing preserved route %s via %s dev %s", errOpenVSwitchNotConverged, expected.Destination, expected.Gateway, expected.Device)
		}
	}
	return nil
}

func openVSwitchRouteMatches(actual, expected openVSwitchRoute) bool {
	if actual.Gateway != expected.Gateway {
		actualGateway, actualErr := netip.ParseAddr(actual.Gateway)
		expectedGateway, expectedErr := netip.ParseAddr(expected.Gateway)
		if actualErr != nil || expectedErr != nil || actualGateway != expectedGateway {
			return false
		}
	}
	actualDestination, actualErr := normalizeOpenVSwitchDestination(actual.Destination)
	expectedDestination, expectedErr := normalizeOpenVSwitchDestination(expected.Destination)
	return actualErr == nil && expectedErr == nil && actualDestination == expectedDestination && actual.Device == expected.Device &&
		(expected.Family == "" || actual.Family == expected.Family) &&
		(expected.Metric == 0 || actual.Metric == expected.Metric) && normalizeOpenVSwitchRouteTable(actual.Table) == normalizeOpenVSwitchRouteTable(expected.Table)
}

func normalizeOpenVSwitchDestination(value string) (string, error) {
	if value == openVSwitchDefaultRoute || value == openVSwitchIPv4DefaultPrefix || value == openVSwitchIPv6DefaultPrefix {
		return openVSwitchDefaultRoute, nil
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return netip.PrefixFrom(address, address.BitLen()).String(), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return "", err
	}
	return prefix.Masked().String(), nil
}

func normalizeOpenVSwitchRouteTable(table any) string {
	value := fmt.Sprint(table)
	if table == nil || value == "main" || value == "254" || value == "0" {
		return "main"
	}
	return value
}

func (s *NodeAgentServer) verifyConfiguredOpenVSwitchAddresses(plan openVSwitchPlan) error {
	bridgeAddresses, err := s.readOpenVSwitchAddresses(plan.bridgeName)
	if err != nil {
		return fmt.Errorf("%w: bridge addresses: %v", errOpenVSwitchNotConverged, err)
	}
	for _, expected := range plan.expected.addresses {
		if !slices.Contains(bridgeAddresses, expected) {
			return fmt.Errorf("%w: bridge %s is missing address %s", errOpenVSwitchNotConverged, plan.bridgeName, expected)
		}
	}
	for _, family := range []struct{ required, ipv4 bool }{{plan.expected.dhcp4Required, true}, {plan.expected.dhcp6Required, false}} {
		if family.required && !slices.ContainsFunc(bridgeAddresses, func(address netip.Prefix) bool { return address.Addr().Is4() == family.ipv4 }) {
			return fmt.Errorf("%w: bridge %s is waiting for DHCP", errOpenVSwitchNotConverged, plan.bridgeName)
		}
	}
	uplinkAddresses, err := s.readOpenVSwitchAddresses(plan.uplinkName)
	if err != nil {
		return err
	}
	if len(uplinkAddresses) != 0 {
		return fmt.Errorf("%w: uplink %s still has global addresses", errOpenVSwitchNotConverged, plan.uplinkName)
	}
	return nil
}

func (s *NodeAgentServer) readOpenVSwitchAddresses(name string) ([]netip.Prefix, error) {
	data, err := s.runOpenVSwitchCommand("ip", ipJSONFlag, "address", openVSwitchShowAction, "dev", name, "scope", "global")
	if err != nil {
		return nil, err
	}
	var links []struct {
		Addresses []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(data, &links); err != nil {
		return nil, fmt.Errorf("parse addresses: %w", err)
	}
	var addresses []netip.Prefix
	for _, link := range links {
		for _, address := range link.Addresses {
			prefix, err := netip.ParsePrefix(address.Local + "/" + strconv.Itoa(address.PrefixLen))
			if err != nil {
				return nil, fmt.Errorf("parse interface address: %w", err)
			}
			addresses = append(addresses, prefix)
		}
	}
	return addresses, nil
}
