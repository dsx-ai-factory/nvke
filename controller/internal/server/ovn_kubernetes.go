// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"regexp"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

var ovnGatewayInterfacePattern = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]{0,14}$`)

func validateOVNKubernetesConfig(plugin db.CniPlugin, cfg *api.OVNKubernetesConfig) error {
	if cfg == nil {
		return nil
	}
	if plugin != db.CNIOvnKubernetes {
		return fmt.Errorf("ovnKubernetes requires the ovn-kubernetes CNI plugin")
	}
	if cfg.GatewayInterface != nil {
		name := *cfg.GatewayInterface
		if !ovnGatewayInterfacePattern.MatchString(name) || name == "." || name == ".." || name == "lo" {
			return fmt.Errorf("ovnKubernetes.gatewayInterface must be a valid non-loopback interface name of 1 to 15 characters")
		}
	}
	if cfg.GatewayMode != nil && *cfg.GatewayMode != api.OVNKubernetesConfigGatewayModeLocal && *cfg.GatewayMode != api.OVNKubernetesConfigGatewayModeShared {
		return fmt.Errorf("ovnKubernetes.gatewayMode must be shared or local")
	}
	if cfg.MTU != nil && (*cfg.MTU < 576 || *cfg.MTU > 65535) {
		return fmt.Errorf("ovnKubernetes.mtu must be between 576 and 65535 bytes")
	}
	return nil
}

func dbOVNKubernetesConfig(cfg *api.OVNKubernetesConfig) *db.OVNKubernetesConfig {
	if cfg == nil {
		return nil
	}
	return &db.OVNKubernetesConfig{
		GatewayInterface: cfg.GatewayInterface,
		AllowNoUplink:    cfg.AllowNoUplink,
		GatewayMode:      (*string)(cfg.GatewayMode),
		MTU:              cfg.MTU,
	}
}

func apiOVNKubernetesConfig(cfg *db.OVNKubernetesConfig) *api.OVNKubernetesConfig {
	if cfg == nil {
		return nil
	}
	return &api.OVNKubernetesConfig{
		GatewayInterface: cfg.GatewayInterface,
		AllowNoUplink:    cfg.AllowNoUplink,
		GatewayMode:      (*api.OVNKubernetesConfigGatewayMode)(cfg.GatewayMode),
		MTU:              cfg.MTU,
	}
}
