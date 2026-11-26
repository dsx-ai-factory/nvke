// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func netnsName(id string) string { return "ctrd-" + shortID(id) }

func netnsPath(id string) string { return "/run/netns/" + netnsName(id) }

var sysctlKey = regexp.MustCompile(`^net(\.[A-Za-z0-9_-]+)+$`)

// attach gives the container a network namespace and joins it to its network.
// The state lock is held. A restarted container asks for the address it had; one
// that is taken, or that no longer fits the network, gets another.
func (e *engine) attach(meta *containerMeta) error {
	network, err := e.loadNetwork(meta.Network)
	if err != nil {
		return err
	}
	c := cniContainer{ID: meta.ID, Netns: netnsPath(meta.ID), Ports: meta.Publish}
	// Whatever a crash or a reboot left behind: a lease or a namespace of this container.
	_ = e.cniDel(network, c, meta.CNIResult)
	_, _ = e.output("ip", "netns", "del", netnsName(meta.ID))
	if _, err := e.output("ip", "netns", "add", netnsName(meta.ID)); err != nil {
		return err
	}
	if err := e.configureNamespace(meta); err != nil {
		return e.abortAttach(network, c, err)
	}
	if err := e.cniLoopback(c); err != nil {
		return e.abortAttach(network, c, err)
	}

	result, parsed, err := e.cniAddWithAddresses(network, c, meta.LastAddresses)
	if err != nil {
		return e.abortAttach(network, c, err)
	}
	meta.CNIResult = result
	meta.IPv4, meta.IPv6, meta.MAC = "", "", ""
	for _, ip := range parsed.IPs {
		address, _, _ := net.ParseCIDR(ip.Address)
		switch {
		case address == nil:
		case address.To4() != nil:
			meta.IPv4 = address.String()
		default:
			meta.IPv6 = address.String()
		}
	}
	for _, iface := range parsed.Interfaces {
		if iface.Sandbox != "" {
			meta.MAC = iface.Mac
		}
	}
	meta.LastAddresses = dedupe([]string{meta.IPv4, meta.IPv6})
	if err := e.saveContainer(meta); err != nil {
		return e.rollbackAttach(meta, err)
	}
	if err := e.refreshHosts(meta.Network); err != nil {
		return e.rollbackAttach(meta, err)
	}
	return nil
}

// rollbackAttach undoes an attachment that could not be recorded or announced to
// the peers. It returns cause, with whatever the rollback could not undo.
func (e *engine) rollbackAttach(meta *containerMeta, cause error) error {
	return errors.Join(cause, e.detach(meta))
}

func (e *engine) cniAddWithAddresses(network *networkMeta, c cniContainer, addresses []string) ([]byte, *cniResult, error) {
	if len(addresses) > 0 {
		c.Addresses = addresses
		result, parsed, err := e.cniAdd(network, c)
		if err == nil {
			return result, parsed, nil
		}
		_, _ = fmt.Fprintf(e.stderr, "ctrd: could not keep the address of %s, taking another: %v\n", c.ID[:12], err)
		_ = e.cniDel(network, c, nil)
		c.Addresses = nil
	}
	return e.cniAdd(network, c)
}

func (e *engine) abortAttach(network *networkMeta, c cniContainer, cause error) error {
	_ = e.cniDel(network, c, nil)
	_, _ = e.output("ip", "netns", "del", netnsName(c.ID))
	return cause
}

// configureNamespace applies the container's sysctls. They are net.* settings,
// which belong to the network namespace the engine made, so they are written there.
func (e *engine) configureNamespace(meta *containerMeta) error {
	keys := make([]string, 0, len(meta.Sysctls))
	for key := range meta.Sysctls {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
		if _, err := e.output("ip", "netns", "exec", netnsName(meta.ID), "sh", "-c", `echo "$1" > "$2"`, "sh", meta.Sysctls[key], path); err != nil {
			return err
		}
	}
	return nil
}

// detach removes the container from its network and frees its address. The
// state lock is held. It does what it can and reports the first problem. When
// the plugins fail to release the address, the container keeps their result
// for the next detach, and is not listed as a peer meanwhile.
func (e *engine) detach(meta *containerMeta) error {
	if meta.Network == hostNetwork || meta.CNIResult == nil {
		return nil
	}
	var delErr error
	if network, err := e.loadNetwork(meta.Network); err == nil {
		delErr = e.cniDel(network, cniContainer{ID: meta.ID, Netns: netnsPath(meta.ID), Ports: meta.Publish}, meta.CNIResult)
	}
	firstErr := delErr
	if _, err := e.output("ip", "netns", "del", netnsName(meta.ID)); err != nil && firstErr == nil {
		firstErr = err
	}
	if delErr == nil {
		meta.CNIResult = nil
	}
	meta.IPv4, meta.IPv6, meta.MAC = "", "", ""
	if err := e.saveContainer(meta); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := e.refreshHosts(meta.Network); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// The containers of a network resolve each other by name through /etc/hosts, the
// way they do under nerdctl, which has no DNS of its own either: every
// attached container's hosts file lists every attached container of its network,
// and is rewritten when one joins or leaves.

func hostsFile(self *containerMeta, attached []*containerMeta) string {
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	for _, other := range attached {
		names := []string{other.Name}
		if other.Hostname != "" && other.Hostname != other.Name {
			names = append(names, other.Hostname)
		}
		if other.ID == self.ID {
			names = append([]string{self.Hostname}, names...)
			names = dedupe(names)
		}
		fmt.Fprintf(&b, "%s\t%s\n", other.IPv4, strings.Join(names, " "))
	}
	return b.String()
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// refreshHosts rewrites the hosts file of every container attached to the
// network. The files are written in place: they are bind-mounted into the containers.
func (e *engine) refreshHosts(network string) error {
	all, err := e.listContainers()
	if err != nil {
		return err
	}
	var attached []*containerMeta
	for _, meta := range all {
		if meta.Network == network && meta.IPv4 != "" {
			attached = append(attached, meta)
		}
	}
	sort.Slice(attached, func(i, j int) bool { return attached[i].Name < attached[j].Name })
	for _, meta := range attached {
		if err := os.WriteFile(filepath.Join(e.containerDir(meta.ID), fileHosts), []byte(hostsFile(meta, attached)), 0o644); err != nil { //nolint:gosec // read by the container's processes
			return err
		}
	}
	return nil
}
