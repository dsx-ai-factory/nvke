// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

const (
	openVSwitchInterfacesPath = "/etc/network/interfaces"
	openVSwitchInterfacesDir  = "/etc/network/interfaces.d"
	openVSwitchIPAddrCommand  = "addr"
)

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
		backend:        pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_IFUPDOWN,
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

func openVSwitchIfupdownValue(config []byte, key string) string {
	for _, line := range strings.Split(string(config), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			return fields[1]
		}
	}
	return ""
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
