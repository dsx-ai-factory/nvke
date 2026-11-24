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
	"slices"
	"strings"
	"testing"
	"time"

	nicosdk "github.com/NVIDIA/infra-controller/rest-api/sdk/standard"
	"github.com/stretchr/testify/require"
)

func TestUnit_New(t *testing.T) {
	base := Config{
		OrgID: "org", TenantID: "tenant", SiteID: "site", VPCID: "vpc",
		SubnetID: "subnet", SSHKeyGroupID: "ssh-group", InstanceTypeID: "instance-type",
	}
	type parameters struct {
		method     string
		wantMethod string
		wantError  string
	}
	tests := map[string]parameters{
		"defaults to TPM":        {wantMethod: "tpm"},
		"trims TPM whitespace":   {method: " tpm ", wantMethod: "tpm"},
		"accepts join token":     {method: "joinToken", wantMethod: "joinToken"},
		"accepts DSX IMDS":       {method: "dsxImds", wantMethod: "dsxImds"},
		"rejects unknown method": {method: "unknown", wantError: "unsupported SPIRE attestation method"},
		"rejects incorrect case": {method: "TPM", wantError: "unsupported SPIRE attestation method"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.SpireAttestationMethod = params.method
			service, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if params.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), params.wantError) {
					t.Fatalf("New() error = %v, want containing %q", err, params.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if service.cfg.SpireAttestationMethod != params.wantMethod {
				t.Errorf("SPIRE attestation method = %q, want %q", service.cfg.SpireAttestationMethod, params.wantMethod)
			}
		})
	}
}

// A foreground bootstrap gate must not prevent CAPNICo from receiving the
// instance and its address. The fake Docker command blocks attached runcmds.
func TestUnit_StartInstanceContainer(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	docker := filepath.Join(dir, "docker")
	inspect := `[{"Config":{"Labels":{"nvidia.com/nico-containers.instance-id":"instance","nvidia.com/nico-containers.site-id":"site","nvidia.com/nico-containers.vpc-id":"vpc","nvidia.com/nico-containers.tenant-id":"tenant","nvidia.com/nico-containers.spire-attestation-method":"dsxImds"}},"State":{"Running":true},"NetworkSettings":{"Networks":{"kind":{"IPAddress":"192.0.2.10"}}}}]`
	fakeDocker := fmt.Sprintf(`#!/bin/sh
case "$1" in
  inspect) printf '%%s' '%s' ;;
  exec)
    if [ "$2" = --detach ]; then
      exit 0
    fi
    if [ "$3" = sh ] && [ "$4" = -lc ] && [ "$5" = 'block-until-release' ]; then
      while [ ! -f '%s' ]; do sleep 0.01; done
    fi
    cat >/dev/null
    ;;
esac
`, inspect, release)
	require.NoError(t, os.WriteFile(docker, []byte(fakeDocker), 0o755))
	service := &Service{cfg: Config{DockerBinary: docker, NodeNetwork: "kind", SiteID: "site", VPCID: "vpc", TenantID: "tenant"}}
	req := &nicosdk.InstanceCreateRequest{}
	req.SetUserData("#cloud-config\nruncmd:\n  - block-until-release\n")
	type result struct {
		instance *nicosdk.Instance
		err      error
	}
	completed := make(chan result, 1)
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o644)
		select {
		case <-completed:
		case <-time.After(5 * time.Second):
		}
	})
	go func() {
		defer close(completed)
		instance, err := service.startInstanceContainer(context.Background(), "container", *req)
		completed <- result{instance, err}
	}()
	select {
	case got := <-completed:
		require.NoError(t, got.err)
		require.Equal(t, "192.0.2.10", got.instance.GetInterfaces()[0].GetIpAddresses()[0])
	case <-time.After(2 * time.Second):
		t.Fatal("instance creation waited for the bootstrap gate")
	}
}

// The fake Docker records the arguments of `docker create`, one per line.
func TestUnit_CreateInstanceNodeFlags(t *testing.T) {
	const instanceID = "0123456789abcdef"
	inspect := `[{"Config":{"Labels":{"nvidia.com/nico-containers.instance-id":"` + instanceID + `","nvidia.com/nico-containers.site-id":"site","nvidia.com/nico-containers.vpc-id":"vpc","nvidia.com/nico-containers.tenant-id":"tenant","nvidia.com/nico-containers.spire-attestation-method":"dsxImds"}},"State":{"Running":true},"NetworkSettings":{"Networks":{"kind":{"IPAddress":"192.0.2.10"}}}}]`
	tests := map[string]struct {
		gpus     string
		wantGPUs []string
	}{
		"defaults pass no GPU flag":          {},
		"whitespace GPUs pass no flag":       {gpus: " \t "},
		"GPUs are passed trimmed":            {gpus: " all ", wantGPUs: []string{"--gpus", "all"}},
		"device selection is passed through": {gpus: `"device=0,1"`, wantGPUs: []string{"--gpus", `"device=0,1"`}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			docker := filepath.Join(dir, "docker")
			calls := filepath.Join(dir, "create-args")
			fakeDocker := fmt.Sprintf(`#!/bin/sh
case "$1" in
  create)
    shift
    for arg in "$@"; do printf '%%s\n' "$arg"; done > '%s'
    echo container
    ;;
  inspect) printf '%%s' '%s' ;;
  exec) cat >/dev/null ;;
esac
`, calls, inspect)
			require.NoError(t, os.WriteFile(docker, []byte(fakeDocker), 0o755))
			service := &Service{
				cfg: Config{
					DockerBinary: docker, NodeNetwork: "kind", NodeImage: "image", SiteID: "site", VPCID: "vpc", TenantID: "tenant",
					NodeGPUs: tt.gpus,
				},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			req := &nicosdk.InstanceCreateRequest{}
			req.SetName("node")
			req.SetVpcId("vpc")
			req.SetTenantId("tenant")
			_, err := service.createInstance(context.Background(), instanceID, *req)
			require.NoError(t, err)

			recorded, err := os.ReadFile(calls)
			require.NoError(t, err)
			args := strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n")

			if tt.wantGPUs == nil {
				require.NotContains(t, args, "--gpus")
			} else {
				index := slices.Index(args, "--gpus")
				require.GreaterOrEqual(t, index, 0)
				require.Equal(t, tt.wantGPUs, args[index:index+2])
			}
			require.Contains(t, args, "/var/lib/etcd:size=4g")
			// Existing flags keep their relative order.
			wantOrder := []string{"--tmpfs", "/tmp", "--tmpfs", "/run"}
			require.Equal(t, wantOrder, args[slices.Index(args, "--security-opt")+4:][:4])
			require.Less(t, slices.Index(args, "--restart"), slices.Index(args, "--network"))
			require.Less(t, slices.Index(args, "--network"), slices.Index(args, "--volume"))
		})
	}
}
