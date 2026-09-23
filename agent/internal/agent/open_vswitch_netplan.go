// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
)

const openVSwitchNetplanDir = "/etc/netplan"

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
	pending    bool
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
	paths, err := filepath.Glob(filepath.Join(s.rootedPath(openVSwitchNetplanDir), "*.yaml"))
	if err != nil {
		return openVSwitchPlan{}, fmt.Errorf("list netplan files: %w", err)
	}
	if len(paths) == 0 {
		return openVSwitchPlan{}, errors.New("neither ifupdown nor netplan configuration is present")
	}

	var candidates []openVSwitchNetplanCandidate
	var allENCandidates []openVSwitchNetplanCandidate
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return openVSwitchPlan{}, fmt.Errorf("read %s: %w", path, readErr)
		}
		document, parseErr := parseOpenVSwitchNetplanDocument(data)
		if parseErr != nil {
			return openVSwitchPlan{}, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		// Netplan may have been written before the connection dropped. Treat
		// that bridge stanza as pending desired state and retry activation.
		if openVSwitchBridgeUsesInterface(document.bridges[bridgeName], link.Name) {
			candidates = append(candidates, openVSwitchNetplanCandidate{
				path:       path,
				document:   document,
				uplinkName: link.Name,
				pending:    true,
			})
			continue
		}
		for name, raw := range document.ethernets {
			stanza, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			candidate := openVSwitchNetplanCandidate{path: path, document: document, uplinkName: name}
			if name == link.Name || openVSwitchStanzaMatchesLink(stanza, link) {
				candidates = append(candidates, candidate)
			}
			if name == "all-en" {
				allENCandidates = append(allENCandidates, candidate)
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
	if !selected.pending {
		rawStanza, ok := selected.document.ethernets[selected.uplinkName]
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
		selected.document.bridges[bridgeName] = bridgeStanza
		delete(selected.document.ethernets, selected.uplinkName)
		physical := map[string]any{"mtu": link.MTU}
		if link.Address != "" {
			physical["match"] = map[string]any{"macaddress": link.Address}
		}
		selected.document.ethernets[link.Name] = physical
	}
	data, err := yaml.Marshal(selected.document.root)
	if err != nil {
		return openVSwitchPlan{}, fmt.Errorf("render %s: %w", selected.path, err)
	}
	return openVSwitchPlan{
		backend:        pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN,
		bridgeName:     bridgeName,
		uplinkName:     link.Name,
		uplinkMAC:      link.Address,
		uplinkMTU:      link.MTU,
		defaultGateway: gateway,
		configPath:     selected.path,
		config:         data,
	}, nil
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
