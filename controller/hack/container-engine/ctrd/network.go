// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// networkMeta is a named bridge network. Its CNI configuration is generated from
// it (conflist) and kept next to it, in the engine's own directory rather than
// /etc/cni/net.d, so that nothing else on the host picks it up.
type networkMeta struct {
	Name    string    `json:"Name"`
	ID      string    `json:"ID"`
	Created time.Time `json:"Created"`
	// Subnets and Gateways are parallel: the IPv4 subnet, then the IPv6 one when the network has it.
	Subnets  []string          `json:"Subnets"`
	Gateways []string          `json:"Gateways"`
	MTU      int               `json:"MTU"`
	Options  map[string]string `json:"Options"`
}

const (
	hostNetwork = "host"
)

func (n *networkMeta) bridge() string { return "br-" + shortID(n.ID) }

func (e *engine) networksDir() string { return filepath.Join(e.cfg.Root, "networks") }

func (e *engine) networkPath(name, ext string) string {
	return filepath.Join(e.networksDir(), name+ext)
}

func checkNetworkName(name string) error {
	if !nameSpec.MatchString(name) {
		return fmt.Errorf("invalid network name %q", name)
	}
	return nil
}

func (e *engine) loadNetwork(name string) (*networkMeta, error) {
	if err := checkNetworkName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(e.networkPath(name, ".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("network %s not found", name)
	}
	if err != nil {
		return nil, err
	}
	var network networkMeta
	if err := json.Unmarshal(data, &network); err != nil {
		return nil, fmt.Errorf("reading network %s: %w", name, err)
	}
	return &network, nil
}

func (e *engine) listNetworks() ([]*networkMeta, error) {
	entries, err := os.ReadDir(e.networksDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var networks []*networkMeta
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".json"); ok {
			if network, err := e.loadNetwork(name); err == nil {
				networks = append(networks, network)
			}
		}
	}
	return networks, nil
}

// conflist is the CNI configuration of the network: a bridge with host-local
// addressing, port publishing and the firewall rules that let traffic in and out
// of the container. The default route is IPv4 only, as under nerdctl.
func (n *networkMeta) conflist(ipamDir string) ([]byte, error) {
	ranges := make([][]map[string]string, 0, len(n.Subnets))
	for i, subnet := range n.Subnets {
		ranges = append(ranges, []map[string]string{{"subnet": subnet, "gateway": n.Gateways[i]}})
	}
	bridge := map[string]any{
		typeKey:        bridgeDriver,
		"bridge":       n.bridge(),
		"isGateway":    true,
		"ipMasq":       true,
		"hairpinMode":  true,
		"capabilities": map[string]bool{"ips": true},
		"ipam": map[string]any{
			"type":    "host-local",
			"dataDir": ipamDir,
			"ranges":  ranges,
			"routes":  []map[string]string{{"dst": "0.0.0.0/0"}},
		},
	}
	if n.MTU > 0 {
		bridge["mtu"] = n.MTU
	}
	return json.MarshalIndent(map[string]any{
		"cniVersion": cniVersion,
		cniNameKey:   n.Name,
		"plugins": []any{
			bridge,
			map[string]any{typeKey: "portmap", "capabilities": map[string]bool{"portMappings": true}},
			map[string]any{typeKey: "firewall"},
		},
	}, "", "  ")
}

// poolOverlapError is what a subnet that is already in use reports. Docker words
// it this way and kind retries with another IPv6 subnet when it reads it.
type poolOverlapError struct{ subnet string }

func (e poolOverlapError) Error() string {
	return "Error response from daemon: Pool overlaps with other one on this address space (" + e.subnet + ")"
}

func overlaps(a, b *net.IPNet) bool { return a.Contains(b.IP) || b.Contains(a.IP) }

// subnetsInUse is every subnet an interface of the host or a network of the engine holds.
func (e *engine) subnetsInUse() ([]*net.IPNet, error) {
	var used []*net.IPNet
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && !ipNet.IP.IsLinkLocalUnicast() {
			used = append(used, ipNet)
		}
	}
	networks, err := e.listNetworks()
	if err != nil {
		return nil, err
	}
	for _, network := range networks {
		for _, subnet := range network.Subnets {
			if _, ipNet, err := net.ParseCIDR(subnet); err == nil {
				used = append(used, ipNet)
			}
		}
	}
	return used, nil
}

// freeIPv4Subnet is the first /24 of 10.4.0.0/16 that nothing uses, the pool nerdctl allocates from.
func freeIPv4Subnet(used []*net.IPNet) (string, error) {
	for i := range 256 {
		_, candidate, _ := net.ParseCIDR("10.4." + strconv.Itoa(i) + ".0/24")
		free := true
		for _, other := range used {
			free = free && !overlaps(candidate, other)
		}
		if free {
			return candidate.String(), nil
		}
	}
	return "", errors.New("no free IPv4 subnet left in 10.4.0.0/16: pass --subnet")
}

// firstAddress is the gateway address of a subnet: its first host.
func firstAddress(subnet *net.IPNet) string {
	ip := subnet.IP.Mask(subnet.Mask)
	next := make(net.IP, len(ip))
	copy(next, ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next.String()
}

// createNetwork implements `network create`. Only bridge networks exist. The
// IPv6 subnet that kind asks for is configured; an IPv4 subnet is chosen when none is given.
func (e *engine) createNetwork(name string, subnets []string, options map[string]string) (string, error) {
	if err := checkNetworkName(name); err != nil {
		return "", err
	}
	var network *networkMeta
	err := e.withLock(func() error {
		if _, err := e.loadNetwork(name); err == nil {
			return fmt.Errorf("network with name %s already exists", name)
		}
		used, err := e.subnetsInUse()
		if err != nil {
			return err
		}
		network = &networkMeta{Name: name, ID: randomID(), Created: time.Now().UTC(), Options: options}
		if mtu, ok := options["com.docker.network.driver.mtu"]; ok {
			if network.MTU, err = strconv.Atoi(mtu); err != nil {
				return fmt.Errorf("invalid MTU %q: %w", mtu, err)
			}
		}
		hasIPv4 := false
		for _, subnet := range subnets {
			_, ipNet, err := net.ParseCIDR(subnet)
			if err != nil {
				return fmt.Errorf("invalid subnet %q: %w", subnet, err)
			}
			for _, other := range used {
				if overlaps(ipNet, other) {
					return poolOverlapError{subnet: subnet}
				}
			}
			hasIPv4 = hasIPv4 || ipNet.IP.To4() != nil
			network.Subnets = append(network.Subnets, ipNet.String())
			network.Gateways = append(network.Gateways, firstAddress(ipNet))
		}
		if !hasIPv4 {
			subnet, err := freeIPv4Subnet(used)
			if err != nil {
				return err
			}
			_, ipNet, _ := net.ParseCIDR(subnet)
			network.Subnets = append([]string{subnet}, network.Subnets...)
			network.Gateways = append([]string{firstAddress(ipNet)}, network.Gateways...)
		}
		conflist, err := network.conflist(filepath.Join(e.cfg.Root, "ipam"))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(e.networksDir(), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(e.networkPath(name, ".conflist"), conflist, 0o600); err != nil {
			return err
		}
		return writeJSON(e.networkPath(name, ".json"), network)
	})
	if err != nil {
		return "", err
	}
	return network.ID, nil
}

// removeNetwork implements `network rm`: the network goes with its bridge and its address leases.
func (e *engine) removeNetwork(name string) error {
	return e.withLock(func() error {
		network, err := e.loadNetwork(name)
		if err != nil {
			return err
		}
		all, err := e.listContainers()
		if err != nil {
			return err
		}
		for _, meta := range all {
			if meta.Network == name && meta.IPv4 != "" {
				return fmt.Errorf("network %s has active endpoints (container %s)", name, meta.Name)
			}
		}
		// The bridge exists once a container has been attached; there is nothing to delete before.
		if _, err := net.InterfaceByName(network.bridge()); err == nil {
			if _, err := e.output("ip", "link", "delete", network.bridge()); err != nil {
				return err
			}
		}
		// The metadata goes last, so that a failed removal can be retried.
		if err := os.Remove(e.networkPath(name, ".conflist")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.RemoveAll(filepath.Join(e.cfg.Root, "ipam", name)); err != nil {
			return err
		}
		return os.Remove(e.networkPath(name, ".json"))
	})
}
