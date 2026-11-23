// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestUnit_VersionAtLeast(t *testing.T) {
	type parameters struct {
		version  string
		minimum  string
		fallback bool
		want     bool
	}
	tests := map[string]parameters{
		"minimum release":                  {version: "v0.1.26", minimum: "0.1.26", want: true},
		"git describe after minimum":       {version: "v0.1.26-2-g1f5274f-dirty", minimum: "0.1.26", want: true},
		"dirty minimum tag":                {version: "v0.1.26-dirty", minimum: "0.1.26", want: true},
		"git describe after older release": {version: "v0.1.25-9-g1f5274f", minimum: "0.1.26"},
		"older release":                    {version: "0.1.25", minimum: "0.1.26"},
		"minimum prerelease":               {version: "v0.1.26-rc.1", minimum: "0.1.26"},
		"unknown native default":           {minimum: "0.1.24", fallback: true, want: true},
		"unknown encryption default":       {version: "unknown", minimum: "0.1.26"},
		"invalid minimum default":          {version: "0.1.26", minimum: "unknown", fallback: true, want: true},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, params.want, VersionAtLeast(params.version, params.minimum, params.fallback))
		})
	}
}

func TestUnit_ExecuteNodePreflightCompatibility(t *testing.T) {
	type parameters struct {
		version      string
		nativeCode   codes.Code
		fieldError   string
		output       string
		scriptStatus string
		scriptExit   int32
		scriptError  string
		scriptCode   codes.Code
		wantNative   int
		wantScripts  int
		wantStatus   string
		wantError    string
	}
	tests := map[string]parameters{
		"old bypasses unsupported RPC":           {version: "v0.1.7", output: "notready", wantScripts: 1, wantStatus: "notready"},
		"old git describe":                       {version: "v0.1.23-2-gabcdef", output: "ready", wantScripts: 1, wantStatus: "ready"},
		"supported stays native":                 {version: "v0.1.24", wantNative: 1, wantStatus: "ready"},
		"supported git describe stays native":    {version: "v0.1.24-2-gabcdef", wantNative: 1, wantStatus: "ready"},
		"unknown native":                         {wantNative: 1, wantStatus: "ready"},
		"malformed native":                       {version: "unknown", wantNative: 1, wantStatus: "ready"},
		"missing RPC remains native error":       {nativeCode: codes.Unimplemented, wantNative: 1, wantError: "Unimplemented"},
		"unsupported field remains native error": {fieldError: "at least one node check or kubeadm_preflight is required", wantNative: 1, wantStatus: "error"},
		"near field error":                       {fieldError: "other error", wantNative: 1, wantStatus: "error"},
		"unavailable":                            {nativeCode: codes.Unavailable, wantNative: 1, wantError: "Unavailable"},
		"unauthenticated":                        {nativeCode: codes.Unauthenticated, wantNative: 1, wantError: "Unauthenticated"},
		"permission denied":                      {nativeCode: codes.PermissionDenied, wantNative: 1, wantError: "PermissionDenied"},
		"internal":                               {nativeCode: codes.Internal, wantNative: 1, wantError: "Internal"},
		"canceled":                               {nativeCode: codes.Canceled, wantNative: 1, wantError: "Canceled"},
		"deadline":                               {nativeCode: codes.DeadlineExceeded, wantNative: 1, wantError: "DeadlineExceeded"},
		"empty script fails closed":              {version: "0.1.7", wantScripts: 1, wantError: "invalid readiness"},
		"script failure with zero exit":          {version: "0.1.7", scriptStatus: "error", wantScripts: 1, wantError: "failed"},
		"script nonzero exit":                    {version: "0.1.7", scriptExit: 1, wantScripts: 1, wantError: "failed"},
		"script error message":                   {version: "0.1.7", scriptError: "permission denied", wantScripts: 1, wantError: "permission denied"},
		"script transport failure":               {version: "0.1.7", scriptCode: codes.Unavailable, wantScripts: 1, wantError: "Unavailable"},
		"malformed output":                       {version: "0.1.7", output: "ready\nnotready", wantScripts: 1, wantError: "invalid readiness"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := &preflightTestServer{code: params.nativeCode, fieldError: params.fieldError, output: params.output, scriptStatus: params.scriptStatus}
			fixture.scriptExit = params.scriptExit
			fixture.scriptError = params.scriptError
			fixture.scriptCode = params.scriptCode
			client := preflightTestClient(t, fixture)
			resp, err := executeNodePreflight(t.Context(), client, params.version, &nvkeproto.ExecuteNodePreflightRequest{CheckCloudInitStatus: &nvkeproto.CheckCloudInitStatus{}})
			if params.wantError != "" {
				require.ErrorContains(t, err, params.wantError)
			} else {
				require.NoError(t, err)
				require.Equal(t, params.wantStatus, resp.GetStatus())
			}
			require.Equal(t, params.wantNative, fixture.nativeCalls)
			require.Len(t, fixture.scripts, params.wantScripts)
		})
	}
}

type preflightTestServer struct {
	nvkeproto.UnimplementedNodeAgentServer
	code         codes.Code
	fieldError   string
	output       string
	scriptStatus string
	scriptExit   int32
	scriptError  string
	scriptCode   codes.Code
	nativeCalls  int
	scripts      []*nvkeproto.ExecuteScriptRequest
}

func (s *preflightTestServer) ExecuteNodePreflight(context.Context, *nvkeproto.ExecuteNodePreflightRequest) (*nvkeproto.ExecuteNodePreflightResponse, error) {
	s.nativeCalls++
	if s.code != codes.OK {
		return nil, status.Error(s.code, "native failure")
	}
	if s.fieldError != "" {
		return &nvkeproto.ExecuteNodePreflightResponse{Status: "error", ErrorMessage: s.fieldError}, nil
	}
	return &nvkeproto.ExecuteNodePreflightResponse{Status: "ready"}, nil
}

func (s *preflightTestServer) ExecuteScript(_ context.Context, req *nvkeproto.ExecuteScriptRequest) (*nvkeproto.ExecuteScriptResponse, error) {
	s.scripts = append(s.scripts, req)
	if s.scriptCode != codes.OK {
		return nil, status.Error(s.scriptCode, "script transport failed")
	}
	scriptStatus := s.scriptStatus
	if scriptStatus == "" {
		scriptStatus = "success"
	}
	return &nvkeproto.ExecuteScriptResponse{Status: scriptStatus, Output: s.output, ExitCode: s.scriptExit, ErrorMessage: s.scriptError}, nil
}

func preflightTestClient(t *testing.T, fixture *preflightTestServer) nvkeproto.NodeAgentClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	nvkeproto.RegisterNodeAgentServer(server, fixture)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///preflight", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return nvkeproto.NewNodeAgentClient(conn)
}

func TestUnit_PreflightCloudInitScript(t *testing.T) {
	type parameters struct {
		report    string
		exit      string
		want      string
		wantError bool
		marker    string
	}
	tests := map[string]parameters{
		"done":                     {report: "status: done", want: "ready"},
		"running":                  {report: "status: running", want: "notready"},
		"disabled":                 {report: "status: disabled", want: "notready"},
		"empty":                    {want: "notready"},
		"misleading detail":        {report: "status: running\ndetail: done", want: "notready"},
		"first line wins":          {report: "status: running\nstatus: done", want: "notready"},
		"recoverable done":         {report: "status: done", exit: "2", want: "ready"},
		"recoverable running":      {report: "status: running", exit: "2", want: "notready"},
		"recoverable error":        {report: "status: error", exit: "2", wantError: true},
		"command unavailable":      {exit: "127", wantError: true},
		"error":                    {report: "status: error", wantError: true},
		"unrecoverable":            {report: "status: done", exit: "1", wantError: true},
		"marker":                   {marker: "file", want: "ready"},
		"invalid marker directory": {marker: "directory", wantError: true},
		"invalid marker symlink":   {marker: "symlink", wantError: true},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			installPreflightStat(t, dir)
			marker := filepath.Join(dir, "marker")
			switch params.marker {
			case "file":
				require.NoError(t, os.WriteFile(marker, nil, 0o600))
			case "directory":
				require.NoError(t, os.Mkdir(marker, 0o700))
			case "symlink":
				require.NoError(t, os.Symlink("absent", marker))
			}
			script := cloudInitCompletionScript
			script = strings.ReplaceAll(script, "/var/lib/dsxos/.cidata-consumed", marker)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud-init"), []byte("#!/bin/bash\nprintf '%s\\n' \"$TEST_REPORT\"\nexit \"${TEST_EXIT:-0}\"\n"), 0o700))
			command := exec.CommandContext(t.Context(), "bash")
			command.Stdin = strings.NewReader(script)
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_REPORT="+params.report, "TEST_EXIT="+params.exit)
			out, err := command.CombinedOutput()
			if params.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err, string(out))
				require.Equal(t, params.want, strings.TrimSpace(string(out)))
			}
		})
	}
}

func TestUnit_NodeAgentClientUsesPinnedSecurityProfile(t *testing.T) {
	t.Parallel()
	type parameters struct {
		node    db.Node
		wantErr string
	}
	tests := map[string]parameters{
		"old SPIFFE node does not switch to available external mTLS": {
			node:    db.Node{IP: "127.0.0.1", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM},
			wantErr: "trust domain is required for SPIFFE authentication",
		},
		"new external mTLS node requires its client identity": {
			node:    db.Node{IP: "127.0.0.1", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS, NodeAgentExternalMTLSEnabled: true},
			wantErr: "external mTLS client certificate is unavailable",
		},
		"new TLS JWT node requires a verifier": {
			node:    db.Node{IP: "127.0.0.1", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS, NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true},
			wantErr: "TLS/JWT verifier is unavailable",
		},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			client := &NodeAgentClient{
				transport: &Transport{
					cfg:                       config.Config{SpiffeEndpointSocket: "unix:///no-such-spire-agent.sock", NodeAgent: config.NodeAgentConfig{Port: "15005"}},
					externalClientCertificate: &tls.Certificate{},
				},
			}
			if params.node.NodeAgentExternalMTLSEnabled && !params.node.NodeAgentTLSJWTEnabled {
				client.transport.externalClientCertificate = nil
			}
			_, cleanup, err := client.getGRPCConnection(ctx, params.node)
			if cleanup != nil {
				cleanup()
			}
			require.ErrorContains(t, err, params.wantErr)
		})
	}
}

func TestUnit_PreflightOtherChecksStayNative(t *testing.T) {
	for name, req := range map[string]*nvkeproto.ExecuteNodePreflightRequest{
		"empty":    {},
		"kubeadm":  {KubeadmPreflight: &nvkeproto.KubeadmPreflightRequest{}},
		"combined": {CheckCloudInitStatus: &nvkeproto.CheckCloudInitStatus{}, Checks: []*nvkeproto.NodePreflightCheck{{}}},
		"CAPD":     {Checks: []*nvkeproto.NodePreflightCheck{{Name: "cluster-api-bootstrap-success", Check: &nvkeproto.NodePreflightCheck_FileExists{FileExists: &nvkeproto.FileExistsPreflightCheck{Path: "/run/cluster-api/bootstrap-success.complete"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, code := range []codes.Code{codes.OK, codes.Unimplemented} {
				fixture := &preflightTestServer{code: code}
				resp, err := executeNodePreflight(t.Context(), preflightTestClient(t, fixture), "0.1.7", req)
				require.Equal(t, code, status.Code(err))
				if code == codes.OK {
					require.Equal(t, "ready", resp.GetStatus())
				}
				require.Equal(t, 1, fixture.nativeCalls)
				require.Empty(t, fixture.scripts)
			}
		})
	}
}

func TestUnit_PreflightCloudInitStatFailure(t *testing.T) {
	dir := t.TempDir()
	script := strings.ReplaceAll(cloudInitCompletionScript, "/var/lib/dsxos/.cidata-consumed", filepath.Join(dir, "absent-marker"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte("#!/bin/bash\necho 'Permission denied' >&2\nexit 1\n"), 0o700))
	command := exec.CommandContext(t.Context(), "bash")
	command.Stdin = strings.NewReader(script)
	command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	out, err := command.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "Permission denied")
}

func installPreflightStat(t *testing.T, dir string) {
	t.Helper()
	stat, err := exec.LookPath("stat")
	require.NoError(t, err)
	output, _ := exec.CommandContext(t.Context(), stat, "--version").Output()
	if !strings.Contains(string(output), "GNU") {
		stat, err = exec.LookPath("gstat")
		if err != nil {
			t.Skip("Ubuntu script filesystem tests require GNU stat (gstat on macOS)")
		}
	}
	require.NoError(t, os.Symlink(stat, filepath.Join(dir, "stat")))
}
