// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func stubVMNetHelper(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "vmnet-helper")
	require.NoError(t, os.WriteFile(helper, []byte(`#!/bin/sh
printf '%s\n' '{"vmnet_start_address":"192.168.64.1","vmnet_end_address":"192.168.64.254","vmnet_subnet_mask":"255.255.255.0"}'
`), 0o700))
	t.Setenv("NICO_VMS_VMNET_HELPER_PATH", helper)
}

func TestDarwinSharedSubnetOverride(t *testing.T) {
	stubVMNetHelper(t)
	for _, cidr := range []string{"", "192.168.64.0/24", "172.30.0.0/24"} {
		t.Run(cidr, func(t *testing.T) {
			cfg := Config{VMBackend: VMBackendDarwinQEMU, BridgeCIDR: cidr}
			err := cfg.normalizeDarwinVMNetBridgeCIDR(slog.Default())
			if cidr == "172.30.0.0/24" {
				require.ErrorContains(t, err, "does not match")
			} else {
				require.NoError(t, err)
				require.Equal(t, "192.168.64.0/24", cfg.BridgeCIDR)
			}
		})
	}
}

func TestDarwinBGPDefaults(t *testing.T) {
	stubVMNetHelper(t)
	cfg, err := Parse(slog.Default())
	require.NoError(t, err)
	require.True(t, cfg.BGPEnabled)
	require.Equal(t, 179, cfg.BGPListenPort)
	require.Equal(t, []string{"172.31.0.0/24"}, cfg.BGPAllowedVIPCIDRs)
}
