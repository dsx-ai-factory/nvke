// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

// apiAuthz is off when absent, and needs a bundle directory when on.
func TestValidateAPIAuthz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		apiAuthz string
		wantOn   bool
		wantErr  string
	}{
		{name: "absent is off"},
		{name: "on with a bundle directory", apiAuthz: "apiAuthz:\n  enabled: true\n  authorizer:\n    localBundle:\n      dir: /etc/nvke/authz\n", wantOn: true},
		{name: "on with no bundle directory is refused", apiAuthz: "apiAuthz:\n  enabled: true\n", wantErr: "apiAuthz: authorizer.localBundle.dir is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := controllerconfig.Load(writeConfig(t, minimalConfig+tt.apiAuthz))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantOn, cfg.APIAuthz.Enabled)
		})
	}
}

// Authorization needs authentication on, and enforcing authorization needs
// authentication enforcing.
func TestCheckAuthzNeedsAuthn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		authnOn         bool
		authnPermissive bool
		authzOn         bool
		authzPermissive bool
		wantErr         string
	}{
		{name: "authentication alone", authnOn: true},
		{name: "both enforcing", authnOn: true, authzOn: true},
		{name: "permissive authorization behind enforcing authentication", authnOn: true, authzOn: true, authzPermissive: true},
		{
			name: "enforcing authorization behind permissive authentication", authnOn: true, authnPermissive: true, authzOn: true,
			wantErr: "apiAuthz.permissive is false but apiAuth.permissive is true",
		},
		{name: "authorization without authentication", authzOn: true, wantErr: "API authentication is off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg controllerconfig.Config
			cfg.APIAuth.Enabled = tt.authnOn
			cfg.APIAuth.Permissive = tt.authnPermissive
			cfg.APIAuthz.Enabled = tt.authzOn
			cfg.APIAuthz.Permissive = tt.authzPermissive
			err := cfg.CheckAuthzNeedsAuthn("")
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}
