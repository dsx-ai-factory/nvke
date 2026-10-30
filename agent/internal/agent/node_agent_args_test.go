// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

func envFilePath(root string) string {
	return filepath.Join(root, "etc", "default", "nvke-agent")
}

func writeEnvFile(t *testing.T, root, content string) string {
	t.Helper()
	path := envFilePath(root)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// The rewrite is what the controller's bootstrap renderer would have produced,
// so the file a reconfigured node ends up with is byte-identical to the file a
// freshly provisioned one gets.
func TestReconfigureNodeAgentArgs_RewritesTheAssignment(t *testing.T) {
	root := t.TempDir()
	path := writeEnvFile(t, root, "AGENT_ARGS=\"--secure-server-port 15005\"\n")
	srv := &NodeAgentServer{RootPath: root}

	resp, err := srv.ReconfigureNodeAgentArgs(context.Background(), &pb.ReconfigureNodeAgentArgsRequest{
		Args: []string{"--secure-server-port", "15005", "--controller-audience=nvke-controller"},
	})
	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureNodeAgentArgsResponse_OUTCOME_APPLIED, resp.GetOutcome())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "AGENT_ARGS=\"--secure-server-port 15005 --controller-audience=nvke-controller\"\n", string(got))
}

// Idempotency is what makes the call safe to repeat from a reconcile loop.
func TestReconfigureNodeAgentArgs_UnchangedWritesNothing(t *testing.T) {
	root := t.TempDir()
	path := writeEnvFile(t, root, "AGENT_ARGS=\"--secure-server-port 15005\"\n")
	before, err := os.Stat(path)
	require.NoError(t, err)

	srv := &NodeAgentServer{RootPath: root}
	resp, err := srv.ReconfigureNodeAgentArgs(context.Background(), &pb.ReconfigureNodeAgentArgsRequest{
		Args: []string{"--secure-server-port", "15005"},
	})
	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureNodeAgentArgsResponse_OUTCOME_UNCHANGED, resp.GetOutcome())

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime(), "the file was rewritten despite an unchanged value")
}

// restart_pending is the whole point of the response: the write alone changes
// nothing about the running process.
func TestReconfigureNodeAgentArgs_ReportsRestartPending(t *testing.T) {
	tests := map[string]struct {
		running     []string
		desired     []string
		wantPending bool
	}{
		"running on the old arguments": {
			running:     []string{"--secure-server-port", "15005"},
			desired:     []string{"--secure-server-port", "15005", "--controller-audience=nvke-controller"},
			wantPending: true,
		},
		"already running what was asked for": {
			running:     []string{"--secure-server-port", "15005"},
			desired:     []string{"--secure-server-port", "15005"},
			wantPending: false,
		},
		"same arguments in a different order still need a restart": {
			running:     []string{"15005", "--secure-server-port"},
			desired:     []string{"--secure-server-port", "15005"},
			wantPending: true,
		},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeEnvFile(t, root, "AGENT_ARGS=\"--secure-server-port 15005\"\n")
			srv := &NodeAgentServer{RootPath: root, runningArgs: params.running}

			resp, err := srv.ReconfigureNodeAgentArgs(context.Background(), &pb.ReconfigureNodeAgentArgsRequest{
				Args: params.desired,
			})
			require.NoError(t, err)
			assert.Equal(t, params.wantPending, resp.GetRestartPending())
			assert.Equal(t, params.running, resp.GetRunningArgs())
		})
	}
}

// Anything else in the file belongs to whoever put it there.
func TestReconfigureNodeAgentArgs_PreservesOtherLines(t *testing.T) {
	root := t.TempDir()
	path := writeEnvFile(t, root, "# managed by the controller\nHTTPS_PROXY=http://proxy.example.test:3128\nAGENT_ARGS=\"--secure-server-port 15005\"\n")
	srv := &NodeAgentServer{RootPath: root}

	_, err := srv.ReconfigureNodeAgentArgs(context.Background(), &pb.ReconfigureNodeAgentArgsRequest{
		Args: []string{"--secure-server-port", "15006"},
	})
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "# managed by the controller\n")
	assert.Contains(t, string(got), "HTTPS_PROXY=http://proxy.example.test:3128\n")
	assert.Contains(t, string(got), "AGENT_ARGS=\"--secure-server-port 15006\"\n")
}

// A duplicate assignment is the dangerous case: the last one wins at expansion
// time, so leaving a stale one behind would quietly undo the rewrite.
func TestReconfigureNodeAgentArgs_DropsADuplicateAssignment(t *testing.T) {
	root := t.TempDir()
	path := writeEnvFile(t, root, "AGENT_ARGS=\"--secure-server-port 15005\"\nAGENT_ARGS=\"--disable-mtls\"\n")
	srv := &NodeAgentServer{RootPath: root}

	_, err := srv.ReconfigureNodeAgentArgs(context.Background(), &pb.ReconfigureNodeAgentArgsRequest{
		Args: []string{"--secure-server-port", "15005"},
	})
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(got), "AGENT_ARGS="), "a stale assignment survived and would win at expansion")
	assert.NotContains(t, string(got), "--disable-mtls")
}

// A node whose env file went missing still converges rather than failing.
func TestReconfigureNodeAgentArgs_CreatesAMissingFile(t *testing.T) {
	root := t.TempDir()
	srv := &NodeAgentServer{RootPath: root}

	resp, err := srv.ReconfigureNodeAgentArgs(context.Background(), &pb.ReconfigureNodeAgentArgsRequest{
		Args: []string{"--secure-server-port", "15005"},
	})
	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureNodeAgentArgsResponse_OUTCOME_APPLIED, resp.GetOutcome())

	got, err := os.ReadFile(envFilePath(root))
	require.NoError(t, err)
	assert.Equal(t, "AGENT_ARGS=\"--secure-server-port 15005\"\n", string(got))
}

func TestReconfigureNodeAgentArgs_Validation(t *testing.T) {
	root := t.TempDir()
	writeEnvFile(t, root, "AGENT_ARGS=\"--secure-server-port 15005\"\n")
	srv := &NodeAgentServer{RootPath: root}

	tests := map[string]*pb.ReconfigureNodeAgentArgsRequest{
		"no arguments at all would strip the agent bare": {},
		"an empty list is refused for the same reason":   {Args: []string{}},
		"a newline would split the file":                 {Args: []string{"--secure-server-port", "15005\nAGENT_ARGS=\"--disable-mtls\""}},
		"more arguments than the cap allows":             {Args: make([]string, maxNodeAgentArgs+1)},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := srv.ReconfigureNodeAgentArgs(context.Background(), req)
			assert.Error(t, err)
		})
	}

	// None of the rejections touched the file.
	got, err := os.ReadFile(envFilePath(root))
	require.NoError(t, err)
	assert.Equal(t, "AGENT_ARGS=\"--secure-server-port 15005\"\n", string(got))
}

func TestReconfigureNodeAgentArgs_NilRequest(t *testing.T) {
	srv := &NodeAgentServer{RootPath: t.TempDir()}
	_, err := srv.ReconfigureNodeAgentArgs(context.Background(), nil)
	assert.Error(t, err)
}
