// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
)

const testRestartAgentCmd = "systemctl restart nke-agent.service"

func TestUnit_NodeAgentTLSJWTBootstrapCommands(t *testing.T) {
	t.Parallel()

	type parameters struct {
		enabled        bool
		wantCommands   int
		agentEnv       string
		wantCommandErr bool
		wantRestart    bool
	}
	tests := map[string]parameters{
		"disabled returns no commands": {},
		"enabled migrates legacy agent arguments": {
			enabled:      true,
			wantCommands: 1,
			agentEnv:     `AGENT_ARGS="--disable-mtls --secure-server-port 15005"`,
			wantRestart:  true,
		},
		"enabled replaces an existing gRPC auth mode": {
			enabled:      true,
			wantCommands: 1,
			agentEnv:     `AGENT_ARGS="--grpc-auth-mode=mtls --secure-server-port 15005"`,
			wantRestart:  true,
		},
		"enabled does not restart without an agent environment file": {
			wantCommands:   1,
			enabled:        true,
			wantCommandErr: true,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			activities := &Activities{cfg: config.Config{NodeAgentTLSJWTEnabled: params.enabled}}
			commands := activities.nodeAgentTLSJWTBootstrapCommands()
			require.Len(t, commands, params.wantCommands)
			if params.wantCommands > 0 {
				assert.Contains(t, commands[0], "--grpc-auth-mode=tls-jwt")

				testDir := t.TempDir()
				agentEnvPath := filepath.Join(testDir, "nke-agent")
				if params.agentEnv != "" {
					require.NoError(t, os.WriteFile(agentEnvPath, []byte(params.agentEnv), 0o600))
				}
				restartMarker := filepath.Join(testDir, "restarted")
				command := strings.ReplaceAll(commands[0], "/etc/default/nke-agent", agentEnvPath)
				command = strings.ReplaceAll(command, testRestartAgentCmd, `touch "`+restartMarker+`"`)
				err := exec.CommandContext(context.Background(), "sh", "-c", command).Run()
				if params.wantCommandErr {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					agentArgs, readErr := os.ReadFile(agentEnvPath)
					require.NoError(t, readErr)
					assert.Contains(t, string(agentArgs), "--grpc-auth-mode=tls-jwt")
					assert.NotContains(t, string(agentArgs), "--disable-mtls")
					assert.NotContains(t, string(agentArgs), "--grpc-auth-mode=mtls")
				}
				_, markerErr := os.Stat(restartMarker)
				assert.Equal(t, params.wantRestart, markerErr == nil)
			}
		})
	}
}

func TestUnit_InjectNodeAgentTLSJWTBootstrap(t *testing.T) {
	t.Parallel()

	commands := []string{configureTLSJWTGrpcAuthCmd}

	type parameters struct {
		userData     string
		wantErr      string
		wantExisting string
	}
	tests := map[string]parameters{
		"root cloud config": {
			userData: `#cloud-config
write_files:
  - path: /etc/existing
    content: existing
runcmd:
  - echo existing
`,
			wantExisting: "/etc/existing",
		},
		"autoinstall nested cloud config": {
			userData: `#cloud-config
autoinstall:
  version: 1
  user-data:
    runcmd:
      - echo existing
`,
			wantExisting: "autoinstall:",
		},
		"creates absent cloud-init collections": {
			userData:     "#cloud-config\nhostname: node-a\n",
			wantExisting: "hostname: node-a",
		},
		"rejects malformed YAML": {
			userData: "write_files: [",
			wantErr:  "parse rendered user data",
		},
		"rejects non-sequence runcmd": {
			userData: "#cloud-config\nruncmd: echo invalid\n",
			wantErr:  "runcmd must be a sequence",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			userData, err := injectNodeAgentTLSJWTBootstrap(params.userData, commands)
			if params.wantErr != "" {
				require.ErrorContains(t, err, params.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, userData, "#cloud-config")
			assert.Contains(t, userData, params.wantExisting)
			assert.Contains(t, userData, "--grpc-auth-mode=tls-jwt")
			assert.Contains(t, userData, testRestartAgentCmd)
		})
	}
}
