// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	openVSwitchBridgeNameField      = "bridgeName"
	openVSwitchUplinkInterfaceField = "uplinkInterface"
)

func validateOpenVSwitchConfig(cniPlugin db.CniPlugin, cfg *db.OpenVSwitchConfig) error {
	if cfg == nil {
		return nil
	}
	if cniPlugin != db.CNIOvnKubernetes {
		return errors.New("openVSwitch requires the ovn-kubernetes CNI plugin")
	}
	validName := regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]{0,14}$`)
	for _, field := range []struct{ name, value string }{{openVSwitchUplinkInterfaceField, cfg.UplinkInterface}, {openVSwitchBridgeNameField, cfg.BridgeName}} {
		if !validName.MatchString(field.value) || field.value == "." || field.value == ".." || field.value == "lo" {
			return fmt.Errorf("openVSwitch.%s must be a safe Linux interface name of 1 to 15 characters, excluding lo", field.name)
		}
	}
	if cfg.UplinkInterface == cfg.BridgeName {
		return errors.New("openVSwitch uplinkInterface and bridgeName must differ")
	}
	return nil
}

func dbOpenVSwitchFromAPI(cfg *api.OpenVSwitchConfig) *db.OpenVSwitchConfig {
	if cfg == nil {
		return nil
	}
	return &db.OpenVSwitchConfig{UplinkInterface: cfg.UplinkInterface, BridgeName: cfg.BridgeName}
}

func dbOpenVSwitchFromUpdateAPI(cfg *api.OpenVSwitchUpdateConfig) *db.OpenVSwitchConfig {
	if cfg == nil || (cfg.UplinkInterface == nil && cfg.BridgeName == nil) {
		return nil
	}
	config := &db.OpenVSwitchConfig{}
	if cfg.UplinkInterface != nil {
		config.UplinkInterface = *cfg.UplinkInterface
	}
	if cfg.BridgeName != nil {
		config.BridgeName = *cfg.BridgeName
	}
	return config
}

func apiOpenVSwitchFromDB(cfg *db.OpenVSwitchConfig) *api.OpenVSwitchConfig {
	if cfg == nil {
		return nil
	}
	return &api.OpenVSwitchConfig{UplinkInterface: cfg.UplinkInterface, BridgeName: cfg.BridgeName}
}
