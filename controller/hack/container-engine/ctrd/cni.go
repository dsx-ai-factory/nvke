// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	cniVersion = "1.0.0"
	cniIfName  = "eth0"
	cniNameKey = "name"
)

// The engine runs the CNI plugins itself, the way libcni does, rather than
// through a library: a plugin is a binary that reads its configuration on
// standard input and the container on CNI_* variables, and a chain passes each
// plugin's result to the next as prevResult.

type cniResult struct {
	Interfaces []struct {
		Name    string `json:"name"`
		Mac     string `json:"mac"`
		Sandbox string `json:"sandbox"`
	} `json:"interfaces"`
	IPs []struct {
		Address string `json:"address"`
	} `json:"ips"`
}

func (e *engine) cniPlugin(name string) (string, error) {
	for _, dir := range e.cfg.CNIDirs {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("CNI plugin %s not found in %s: install the containernetworking plugins or set CNI_PATH", name, strings.Join(e.cfg.CNIDirs, ":"))
}

type cniContainer struct {
	ID    string
	Netns string
	// Addresses are asked of the address manager, which gives them if they are free and in range.
	Addresses []string
	// Ports are what portmap publishes.
	Ports []portMapping
}

// cniInvoke runs one plugin and returns what it printed. On failure the plugin's
// error, which is JSON on standard output, is part of the error.
func (e *engine) cniInvoke(command, plugin, ifName string, config map[string]any, c cniContainer) ([]byte, error) {
	path, err := e.cniPlugin(plugin)
	if err != nil {
		return nil, err
	}
	stdin, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	cmd := e.command(path)
	cmd.Env = append(os.Environ(),
		"CNI_COMMAND="+command,
		"CNI_CONTAINERID="+c.ID,
		"CNI_NETNS="+c.Netns,
		"CNI_IFNAME="+ifName,
		"CNI_ARGS=IgnoreUnknown=1",
		"CNI_PATH="+strings.Join(e.cfg.CNIDirs, ":"),
	)
	var stdout bytes.Buffer
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("CNI plugin %s %s: %w: %s", plugin, command, err, strings.TrimSpace(stdout.String()))
	}
	return stdout.Bytes(), nil
}

// conflistPlugins reads the plugin entries of a network's conflist, each with the list's name and version.
func (e *engine) conflistPlugins(network *networkMeta) ([]map[string]any, error) {
	data, err := os.ReadFile(e.networkPath(network.Name, ".conflist"))
	if err != nil {
		return nil, err
	}
	var list struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("reading the CNI configuration of network %s: %w", network.Name, err)
	}
	for _, plugin := range list.Plugins {
		plugin["cniVersion"] = cniVersion
		plugin[cniNameKey] = network.Name
	}
	return list.Plugins, nil
}

// withRuntime adds what the container asks of a plugin that declares the
// capability: the ports portmap publishes, and the addresses the bridge's
// address manager is asked for.
func withRuntime(plugin map[string]any, c cniContainer, prev json.RawMessage) map[string]any {
	capabilities, _ := plugin["capabilities"].(map[string]any)
	runtimeConfig := map[string]any{}
	if capabilities["ips"] == true && len(c.Addresses) > 0 {
		runtimeConfig["ips"] = c.Addresses
	}
	if capabilities["portMappings"] == true && len(c.Ports) > 0 {
		ports := make([]map[string]any, 0, len(c.Ports))
		for _, port := range c.Ports {
			ports = append(ports, map[string]any{
				"hostPort": port.HostPort, "containerPort": port.ContainerPort, "protocol": port.Protocol, "hostIP": port.HostIP,
			})
		}
		runtimeConfig["portMappings"] = ports
	}
	if len(runtimeConfig) > 0 {
		plugin["runtimeConfig"] = runtimeConfig
	}
	if prev != nil {
		plugin["prevResult"] = prev
	}
	return plugin
}

// cniAdd attaches the container to the network and returns the last plugin's result.
func (e *engine) cniAdd(network *networkMeta, c cniContainer) (json.RawMessage, *cniResult, error) {
	plugins, err := e.conflistPlugins(network)
	if err != nil {
		return nil, nil, err
	}
	var result json.RawMessage
	for _, plugin := range plugins {
		kind, _ := plugin[typeKey].(string)
		out, err := e.cniInvoke("ADD", kind, cniIfName, withRuntime(plugin, c, result), c)
		if err != nil {
			return nil, nil, err
		}
		result = out
	}
	var parsed cniResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, nil, fmt.Errorf("reading the CNI result %q: %w", result, err)
	}
	return result, &parsed, nil
}

// cniDel detaches the container, handing each plugin the result of the attachment, last plugin first.
func (e *engine) cniDel(network *networkMeta, c cniContainer, result json.RawMessage) error {
	plugins, err := e.conflistPlugins(network)
	if err != nil {
		return err
	}
	var firstErr error
	for i := len(plugins) - 1; i >= 0; i-- {
		kind, _ := plugins[i][typeKey].(string)
		if _, err := e.cniInvoke("DEL", kind, cniIfName, withRuntime(plugins[i], c, result), c); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// cniLoopback brings up lo in the container's network namespace.
func (e *engine) cniLoopback(c cniContainer) error {
	_, err := e.cniInvoke("ADD", "loopback", "lo", map[string]any{"cniVersion": cniVersion, cniNameKey: "ctrd-lo", typeKey: "loopback"}, c)
	return err
}
