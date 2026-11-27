// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
	"github.com/stretchr/testify/require"
)

// writeFakeEngine writes an executable named name that runs script, and
// returns its path. Tests that use it are not parallel: running a script this
// test just wrote, beside another test's fork, fails with "text file busy".
func writeFakeEngine(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755))
	return path
}

// The engine CLI is nerdctl behind sudo on a containerd host, and either can
// write warnings to standard error. Those must not reach the container ID that
// `create` returns, nor the IDs `ps` lists, and must still explain a failure.
func TestUnit_RunDockerSeparatesStandardError(t *testing.T) {
	tests := map[string]struct {
		script     string
		want       string
		wantErrors []string
	}{
		"warning beside the result": {
			script: "echo 'sudo: unable to resolve host example' >&2\necho abc123\n",
			want:   "abc123",
		},
		"failure keeps both streams": {
			script:     "echo 'out line'\necho 'cannot reach containerd' >&2\nexit 3\n",
			wantErrors: []string{"cannot reach containerd", "out line", "info"},
		},
		"failure with no output names the exit": {
			script:     "exit 4\n",
			wantErrors: []string{"exit status 4"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			service := &Service{cfg: Config{DockerBinary: writeFakeEngine(t, "nerdctl", tt.script)}}
			got, err := service.runDocker(context.Background(), "info")
			if len(tt.wantErrors) > 0 {
				require.Error(t, err)
				for _, want := range tt.wantErrors {
					require.Contains(t, err.Error(), want)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// Everything createInstance asks of the engine, so a change to it is a change
// to what nerdctl has to accept. Each flag below was checked against nerdctl
// 2.4.2's `create`; a new one needs the same check.
func TestUnit_CreateInstanceEngineArguments(t *testing.T) {
	dir := t.TempDir()
	createArgs := filepath.Join(dir, "create-args")
	inspect := fmt.Sprintf(`[{"Config":{"Labels":{%q:"instance",%q:"site",%q:"vpc",%q:"tenant",%q:"instance-type"}},"State":{"Running":true},"NetworkSettings":{"Networks":{"kind":{"IPAddress":"192.0.2.10"}}}}]`,
		labelInstanceID, labelSiteID, labelVPCID, labelTenantID, labelInstanceType)
	engine := writeFakeEngine(t, "nerdctl", fmt.Sprintf(`case "$1" in
  create)
    echo 'WARN[0000] a warning on standard error' >&2
    printf '%%s\n' "$@" > '%s'
    echo container-1
    ;;
  inspect) printf '%%s' '%s' ;;
  exec) cat >/dev/null ;;
esac
`, createArgs, inspect))
	service, err := New(Config{
		OrgID: "org", TenantID: "tenant", SiteID: "site", VPCID: "vpc", SubnetID: "subnet",
		SSHKeyGroupID: "ssh-group", InstanceTypeID: "instance-type",
		NodeNetwork: "kind-nodes", NodeImage: "node-image:test", DockerBinary: engine,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	req := &nicosdk.InstanceCreateRequest{}
	req.SetName("node")
	req.SetTenantId("tenant")
	req.SetVpcId("vpc")
	instance, err := service.createInstance(context.Background(), "0123456789abcdef", *req)
	require.NoError(t, err)
	require.Equal(t, "instance", instance.GetId())

	raw, err := os.ReadFile(createArgs)
	require.NoError(t, err)
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	require.Equal(t, "create", args[0])

	hasPair := func(flag, value string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == value {
				return true
			}
		}
		return false
	}
	for _, pair := range [][2]string{
		{"--name", "node-01234567"},
		{"--hostname", "node"},
		{"--security-opt", "seccomp=unconfined"},
		{"--security-opt", "apparmor=unconfined"},
		{"--tmpfs", "/tmp"},
		{"--tmpfs", "/run"},
		{"--tmpfs", "/var/lib/etcd:size=4g"},
		{"--restart", "always"},
		{"--network", "kind-nodes"},
		{"--volume", "/var"},
		{"--volume", "/lib/modules:/lib/modules:ro"},
		{"--entrypoint", "/bin/sh"},
		{"--label", labelManaged + "=true"},
		{"--label", labelInstanceID + "=0123456789abcdef"},
	} {
		require.Truef(t, hasPair(pair[0], pair[1]), "create has no %s %s: %q", pair[0], pair[1], args)
	}
	require.Contains(t, args, "--privileged")
	require.Contains(t, args, "--tty")
	require.True(t, hasPair("node-image:test", "-c"), "the image is followed by the node entrypoint script: %q", args)
}
