// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	openVSwitchNetplanDir       = "/etc/netplan"
	openVSwitchNetplanMatchName = "name"
	openVSwitchNetplanAddresses = "addresses"
)

// openVSwitchNetplanDocument holds validated views into a parsed netplan tree.
// Maps are intentional because netplan is extensible and unknown fields must
// round-trip unchanged. Mutating ethernets or bridges updates root directly.
type openVSwitchNetplanDocument struct {
	root      map[string]any
	ethernets map[string]any
	bridges   map[string]any
}

type openVSwitchNetplanCandidate struct {
	path       string
	document   openVSwitchNetplanDocument
	uplinkName string
}

func parseOpenVSwitchNetplanDocument(data []byte) (openVSwitchNetplanDocument, error) {
	root := map[string]any{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return openVSwitchNetplanDocument{}, fmt.Errorf("parse YAML: %w", err)
	}

	rawNetwork, exists := root["network"]
	if !exists {
		return openVSwitchNetplanDocument{}, errors.New("netplan network section is required")
	}
	network, ok := rawNetwork.(map[string]any)
	if !ok {
		return openVSwitchNetplanDocument{}, errors.New("netplan network section must be a mapping")
	}
	ethernets, err := openVSwitchNetplanSectionOrCreate(network, "ethernets")
	if err != nil {
		return openVSwitchNetplanDocument{}, err
	}
	bridges, err := openVSwitchNetplanSectionOrCreate(network, "bridges")
	if err != nil {
		return openVSwitchNetplanDocument{}, err
	}
	return openVSwitchNetplanDocument{
		root:      root,
		ethernets: ethernets,
		bridges:   bridges,
	}, nil
}

// openVSwitchNetplanSectionOrCreate creates an absent optional section, but
// rejects a present non-mapping value instead of silently replacing user data.
func openVSwitchNetplanSectionOrCreate(network map[string]any, name string) (map[string]any, error) {
	rawSection, exists := network[name]
	if !exists {
		section := map[string]any{}
		network[name] = section
		return section, nil
	}
	section, ok := rawSection.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("netplan network.%s section must be a mapping", name)
	}
	return section, nil
}

func (s *NodeAgentServer) buildOpenVSwitchNetplanPlan(link openVSwitchLink, gateway, bridgeName string) (openVSwitchPlan, error) {
	return s.buildOpenVSwitchNetplanPlanWithMode(link, gateway, bridgeName, false)
}

func (s *NodeAgentServer) buildConfiguredOpenVSwitchNetplanPlan(link openVSwitchLink, bridgeName string) (openVSwitchPlan, error) {
	return s.buildOpenVSwitchNetplanPlanWithMode(link, "", bridgeName, true)
}

func (s *NodeAgentServer) buildOpenVSwitchNetplanPlanWithMode(link openVSwitchLink, gateway, bridgeName string, strict bool) (openVSwitchPlan, error) {
	paths, err := s.openVSwitchNetplanPaths(strict)
	if err != nil {
		return openVSwitchPlan{}, err
	}
	if len(paths) == 0 {
		return openVSwitchPlan{}, errors.New("neither ifupdown nor netplan configuration is present")
	}

	var candidates, wildcardCandidates, legacyCandidates []openVSwitchNetplanCandidate
	var documents []openVSwitchNetplanDocument
	interfaceDefinitions := map[string]int{}
	fileDefinitions := map[string]int{}
	var bridgePaths []string
	var driver string
	for _, path := range paths {
		fileDefinitions[filepath.Base(path)]++
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return openVSwitchPlan{}, fmt.Errorf("read %s: %w", path, readErr)
		}
		document, parseErr := parseOpenVSwitchNetplanDocument(data)
		if parseErr != nil {
			return openVSwitchPlan{}, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		documents = append(documents, document)
		if _, exists := document.bridges[bridgeName]; exists {
			bridgePaths = append(bridgePaths, path)
		}
		for name, raw := range document.ethernets {
			interfaceDefinitions[name]++
			stanza, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			match, _ := stanza["match"].(map[string]any)
			if _, hasDriver := match["driver"]; hasDriver && driver == "" {
				target, err := os.Readlink(s.rootedPath(filepath.Join("/sys/class/net", link.Name, "device/driver")))
				if err != nil {
					return openVSwitchPlan{}, fmt.Errorf("read driver for netplan interface %s: %w", link.Name, err)
				}
				driver = filepath.Base(target)
			}
			candidate := openVSwitchNetplanCandidate{path: path, document: document, uplinkName: name}
			exact, wildcard := openVSwitchNetplanMatchesLink(name, stanza, link, driver)
			if exact {
				candidates = append(candidates, candidate)
			}
			if wildcard {
				wildcardCandidates = append(wildcardCandidates, candidate)
			}
			if name == "all-en" && wildcard {
				legacyCandidates = append(legacyCandidates, candidate)
			}
		}
	}
	if !strict && len(candidates) == 0 && len(legacyCandidates) == 1 {
		selected, splitErr := splitOpenVSwitchNetplanWildcard(legacyCandidates[0], link, interfaceDefinitions)
		if splitErr != nil {
			return openVSwitchPlan{}, splitErr
		}
		candidates = append(candidates, selected)
		interfaceDefinitions[selected.uplinkName] = 1
	}
	if len(candidates) != 1 {
		return openVSwitchPlan{}, fmt.Errorf("expected one netplan configuration for %s, got %d", link.Name, len(candidates))
	}

	selected := candidates[0]
	if strict {
		if filepath.Dir(selected.path) != s.rootedPath(openVSwitchNetplanDir) {
			return openVSwitchPlan{}, fmt.Errorf("selected netplan interface %s requires a persistent definition under %s", selected.uplinkName, openVSwitchNetplanDir)
		}
		// A higher-priority directory shadows the entire same-named file, even
		// when that file does not mention the selected interface. Reject this
		// layout rather than edit a definition Netplan may never read.
		if fileDefinitions[filepath.Base(selected.path)] != 1 {
			return openVSwitchPlan{}, fmt.Errorf("selected netplan file %s exists in multiple configuration directories", filepath.Base(selected.path))
		}
	}
	if interfaceDefinitions[bridgeName] != 0 {
		return openVSwitchPlan{}, fmt.Errorf("netplan bridge %s conflicts with an ethernet definition", bridgeName)
	}
	if interfaceDefinitions[selected.uplinkName] != 1 {
		return openVSwitchPlan{}, fmt.Errorf("netplan interface %s is defined in multiple files", selected.uplinkName)
	}
	if err := validateOpenVSwitchNetplanMembership(documents, selected.uplinkName, bridgeName); err != nil {
		return openVSwitchPlan{}, err
	}
	// networkd uses the first matching .network file. Keeping the logical ID
	// preserves that ordering; migrating a wildcard group would affect other
	// NICs, and an earlier wildcard would shadow this interface's configuration.
	for _, candidate := range wildcardCandidates {
		if candidate.uplinkName < selected.uplinkName {
			return openVSwitchPlan{}, fmt.Errorf("netplan wildcard %s precedes interface %s", candidate.uplinkName, selected.uplinkName)
		}
	}
	if len(bridgePaths) > 1 || (len(bridgePaths) == 1 && bridgePaths[0] != selected.path) {
		return openVSwitchPlan{}, fmt.Errorf("netplan bridge %s is defined outside the uplink configuration", bridgeName)
	}
	// Render before changing the parsed document so formatting differences in
	// the file do not count as configuration changes.
	original, err := yaml.Marshal(selected.document.root)
	if err != nil {
		return openVSwitchPlan{}, fmt.Errorf("render %s: %w", selected.path, err)
	}
	mtu, err := migrateOpenVSwitchNetplanInterface(selected, link, bridgeName)
	if err != nil {
		return openVSwitchPlan{}, err
	}
	data, err := yaml.Marshal(selected.document.root)
	if err != nil {
		return openVSwitchPlan{}, fmt.Errorf("render %s: %w", selected.path, err)
	}
	var expected *openVSwitchNetworkExpectation
	if strict {
		bridge, _ := selected.document.bridges[bridgeName].(map[string]any)
		expected, err = configuredOpenVSwitchExpectation(bridge, bridgeName)
		if err != nil {
			return openVSwitchPlan{}, fmt.Errorf("validate preserved network configuration: %w", err)
		}
	}
	return openVSwitchPlan{
		backend:           pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN,
		bridgeName:        bridgeName,
		uplinkName:        link.Name,
		uplinkMAC:         link.Address,
		uplinkMTU:         mtu,
		defaultGateway:    gateway,
		configPath:        selected.path,
		config:            data,
		configMatchesPlan: bytes.Equal(original, data),
		expected:          expected,
	}, nil
}

func (s *NodeAgentServer) openVSwitchNetplanPaths(strict bool) ([]string, error) {
	directories := []string{openVSwitchNetplanDir}
	if strict {
		// Netplan combines these directories. Inspect runtime and vendor input
		// for conflicts, but only a persistent /etc definition may be modified.
		directories = append(directories, "/run/netplan", "/lib/netplan")
	}
	var paths []string
	for _, directory := range directories {
		files, err := filepath.Glob(filepath.Join(s.rootedPath(directory), "*.yaml"))
		if err != nil {
			return nil, fmt.Errorf("list netplan files in %s: %w", directory, err)
		}
		paths = append(paths, files...)
	}
	return paths, nil
}

func validateOpenVSwitchNetplanMembership(documents []openVSwitchNetplanDocument, uplink, bridgeName string) error {
	for _, document := range documents {
		network, _ := document.root["network"].(map[string]any)
		// Migration does not handle VLANs that depend on the uplink. Refuse
		// this topology before changing their parent interface's configuration.
		vlans, _ := network["vlans"].(map[string]any)
		for name, raw := range vlans {
			vlan, _ := raw.(map[string]any)
			if parent, _ := vlan["link"].(string); parent == uplink {
				return fmt.Errorf("netplan interface %s is the parent link of vlan %s", uplink, name)
			}
		}
		for _, kind := range []string{"bridges", "bonds"} {
			devices, _ := network[kind].(map[string]any)
			for name, raw := range devices {
				if kind == "bridges" && name == bridgeName {
					continue
				}
				device, _ := raw.(map[string]any)
				interfaces, _ := device["interfaces"].([]any)
				for _, member := range interfaces {
					if member == uplink {
						return fmt.Errorf("netplan interface %s already belongs to %s %s", uplink, kind, name)
					}
				}
			}
		}
	}
	return nil
}

func splitOpenVSwitchNetplanWildcard(candidate openVSwitchNetplanCandidate, link openVSwitchLink, definitions map[string]int) (openVSwitchNetplanCandidate, error) {
	// Preserve the historical all-en fallback without moving L3 configuration
	// off every device in that group. The new narrow definition must sort ahead
	// of all-en so networkd applies it only to this uplink.
	name := "00-nvke-" + link.Name
	if definitions[name] != 0 {
		return openVSwitchNetplanCandidate{}, fmt.Errorf("netplan interface %s already exists", name)
	}
	stanza, ok := candidate.document.ethernets[candidate.uplinkName].(map[string]any)
	if !ok {
		return openVSwitchNetplanCandidate{}, errors.New("netplan all-en interface is not an object")
	}
	physical := cloneStringMap(stanza)
	match, _ := physical["match"].(map[string]any)
	match = cloneStringMap(match)
	match[openVSwitchNetplanMatchName] = link.Name
	if link.Address != "" {
		match["macaddress"] = link.Address
	}
	physical["match"] = match
	candidate.document.ethernets[name] = physical
	candidate.uplinkName = name
	return candidate, nil
}

func migrateOpenVSwitchNetplanInterface(selected openVSwitchNetplanCandidate, link openVSwitchLink, bridgeName string) (int, error) {
	stanza, ok := selected.document.ethernets[selected.uplinkName].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("netplan interface %s is not an object", selected.uplinkName)
	}
	mtu, err := openVSwitchNetplanMTU(stanza, link.MTU)
	if err != nil {
		return 0, err
	}
	if rawBridge, exists := selected.document.bridges[bridgeName]; exists {
		// Resolve the bridge member through its Netplan ID, which need not be
		// the kernel interface name, when recovering an interrupted apply.
		bridge, isMapping := rawBridge.(map[string]any)
		if !isMapping || !openVSwitchBridgeUsesInterface(rawBridge, selected.uplinkName) {
			return 0, fmt.Errorf("netplan bridge %s does not exclusively use interface %s", bridgeName, selected.uplinkName)
		}
		if _, isOVS := bridge["openvswitch"].(map[string]any); !isOVS {
			return 0, fmt.Errorf("netplan bridge %s is not an Open vSwitch bridge", bridgeName)
		}
		bridgeMTU, mtuErr := openVSwitchNetplanMTU(bridge, mtu)
		if mtuErr != nil {
			return 0, mtuErr
		}
		if _, configured := stanza["mtu"]; configured && bridgeMTU != mtu {
			return 0, fmt.Errorf("netplan bridge %s MTU differs from interface %s", bridgeName, selected.uplinkName)
		}
		physical := cloneStringMap(stanza)
		bridge = cloneStringMap(bridge)
		physical["mtu"], bridge["mtu"] = bridgeMTU, bridgeMTU
		selected.document.ethernets[selected.uplinkName] = physical
		selected.document.bridges[bridgeName] = bridge
		return bridgeMTU, nil
	}

	// Keep the logical ID, match rules, rename, and device settings on the
	// physical NIC. Move only L3 configuration; unknown device properties must
	// not be copied to a bridge where Netplan may reject them.
	physical := cloneStringMap(stanza)
	bridge := map[string]any{
		"interfaces":  []string{selected.uplinkName},
		"openvswitch": map[string]any{},
		"mtu":         mtu,
	}
	for key, value := range stanza {
		if openVSwitchNetplanL3Key(key) {
			bridge[key] = value
			delete(physical, key)
		}
	}
	// These common link settings apply to both links, including a per-device
	// renderer override and boot-time wait-online behavior.
	for _, key := range []string{"renderer", "optional", "optional-addresses", "critical", "activation-mode", "ignore-carrier"} {
		if value, exists := stanza[key]; exists {
			bridge[key] = value
		}
	}
	physical["mtu"] = mtu
	selected.document.ethernets[selected.uplinkName] = physical
	selected.document.bridges[bridgeName] = bridge
	return mtu, nil
}

func openVSwitchNetplanL3Key(key string) bool {
	switch key {
	case openVSwitchNetplanAddresses, "nameservers", "routes", "routing-policy", "gateway4", openVSwitchNetplanGateway6,
		"dhcp4", "dhcp6", "dhcp-identifier", "dhcp4-overrides", "dhcp6-overrides",
		"accept-ra", "ra-overrides", "ipv6-address-generation", "ipv6-address-token", "ipv6-privacy", "ipv6-mtu", "link-local":
		return true
	default:
		return false
	}
}

func openVSwitchNetplanMTU(stanza map[string]any, observed int) (int, error) {
	raw, exists := stanza["mtu"]
	if !exists {
		return observed, nil
	}
	mtu, ok := raw.(float64)
	if !ok || mtu <= 0 || mtu > math.MaxInt32 || math.Trunc(mtu) != mtu {
		return 0, fmt.Errorf("netplan interface MTU %v must be a positive integer", raw)
	}
	return int(mtu), nil
}

func (s *NodeAgentServer) applyOpenVSwitchNetplanPlan(ctx context.Context, plan openVSwitchPlan) error {
	if err := atomicWriteFile(plan.configPath, plan.config, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", plan.configPath, err)
	}
	if _, err := s.runOpenVSwitchCommand("netplan", "generate"); err != nil {
		return fmt.Errorf("validate netplan: %w", err)
	}
	if _, err := s.runOpenVSwitchCommand("netplan", "apply"); err != nil {
		return fmt.Errorf("apply netplan: %w", err)
	}
	return sleepOrDone(ctx, s.openVSwitchSettleDelayOrDefault())
}

func cloneStringMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func openVSwitchNetplanMatchesLink(name string, stanza map[string]any, link openVSwitchLink, driver string) (bool, bool) {
	match, hasMatch := stanza["match"].(map[string]any)
	if !hasMatch {
		return name == link.Name, false
	}
	mac, _ := match["macaddress"].(string)
	if mac != "" && (link.Address == "" || !strings.EqualFold(mac, link.Address)) {
		return false, false
	}
	if patterns, exists := match["driver"]; exists && !openVSwitchNetplanDriverMatches(patterns, driver) {
		return false, false
	}
	renamed, _ := stanza["set-name"].(string)
	if renamed != "" && renamed != link.Name {
		return false, false
	}
	matchName, _ := match[openVSwitchNetplanMatchName].(string)
	// A renamed interface no longer has the name used by its original match.
	if renamed != link.Name && matchName != "" {
		matches, err := filepath.Match(matchName, link.Name)
		if err != nil || !matches {
			return false, false
		}
	}
	if mac != "" || renamed == link.Name || matchName == link.Name {
		return true, false
	}
	return false, true
}

func openVSwitchNetplanDriverMatches(raw any, driver string) bool {
	if driver == "" {
		return false
	}
	patterns, ok := raw.([]any)
	if !ok {
		patterns = []any{raw}
	}
	for _, rawPattern := range patterns {
		pattern, ok := rawPattern.(string)
		if !ok {
			continue
		}
		if matches, err := filepath.Match(pattern, driver); err == nil && matches {
			return true
		}
	}
	return false
}

func openVSwitchBridgeUsesInterface(raw any, interfaceName string) bool {
	stanza, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	interfaces, ok := stanza["interfaces"].([]any)
	if !ok {
		if typed, typedOK := stanza["interfaces"].([]string); typedOK {
			return len(typed) == 1 && typed[0] == interfaceName
		}
		return false
	}
	return len(interfaces) == 1 && interfaces[0] == interfaceName
}
