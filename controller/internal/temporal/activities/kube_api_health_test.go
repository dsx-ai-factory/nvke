// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestParseReadyzVerbose(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		stdout      string
		wantFailing []string
		wantSawAny  bool
	}{
		{name: "all healthy", stdout: "[+]ping ok\n[+]etcd ok\nreadyz check passed", wantFailing: nil, wantSawAny: true},
		{name: "one failing", stdout: "[+]ping ok\n[-]etcd failed: connection refused\nreadyz check failed", wantFailing: []string{etcdConst}, wantSawAny: true},
		{name: "multiple failing", stdout: "[+]ping\n[-]etcd failed: x\n[-]log stale", wantFailing: []string{etcdConst, "log"}, wantSawAny: true},
		{name: "failing no trailer", stdout: "[-]etcd", wantFailing: []string{etcdConst}, wantSawAny: true},
		{name: "empty fails closed", stdout: "", wantFailing: nil, wantSawAny: false},
		{name: "garbled fails closed", stdout: "Unauthorized\nsome error text", wantFailing: nil, wantSawAny: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			failing, sawAny := parseReadyzVerbose(tc.stdout)
			require.Equal(t, tc.wantSawAny, sawAny)
			require.Equal(t, tc.wantFailing, failing)
		})
	}
}

func TestHealthyRequired(t *testing.T) {
	t.Parallel()
	// Quorum-plus-one headroom, capped at N. Matches SWE's healthyRequired.
	cases := map[int]int{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 4, 6: 5, 7: 5}
	for n, want := range cases {
		require.Equalf(t, want, healthyRequired(n), "healthyRequired(%d)", n)
	}
}

func TestKubectlRawLocalTargetsLocalApiserver(t *testing.T) {
	t.Parallel()
	script := kubectlRawLocal(kubeReadyzPath)
	require.True(t, strings.HasPrefix(script, "#!/bin/sh\n"), "must carry a shebang for agent direct exec")
	require.Contains(t, script, "--server https://localhost:6443")
	require.Contains(t, script, "get --raw '/readyz?verbose'")
}

func readyzResp(healthy bool) *nkeproto.ExecuteScriptResponse {
	if healthy {
		return &nkeproto.ExecuteScriptResponse{Output: "[+]ping ok\n[+]etcd ok\nreadyz check passed", ExitCode: 0}
	}
	return &nkeproto.ExecuteScriptResponse{Output: "[+]ping ok\n[-]etcd failed: refused", ExitCode: 1}
}

func TestProbeControlPlaneAPIHealth(t *testing.T) {
	t.Parallel()
	mkNodes := func(n int) []db.Node {
		nodes := make([]db.Node, 0, n)
		for i := range n {
			nodes = append(nodes, db.Node{ID: uuid.New(), Name: fmt.Sprintf("cp-%d", i)})
		}
		return nodes
	}

	t.Run("all healthy passes", func(t *testing.T) {
		t.Parallel()
		nodes := mkNodes(3)
		fake := &agenttest.FakeClient{ExecuteScriptHook: func(db.Node, string, string) (*nkeproto.ExecuteScriptResponse, error) {
			return readyzResp(true), nil
		}}
		a := &Activities{nodeAgentClient: fake}
		require.NoError(t, a.probeControlPlaneAPIHealth(context.Background(), nodes))
	})

	t.Run("one down on 3-CP fails (threshold 3)", func(t *testing.T) {
		t.Parallel()
		nodes := mkNodes(3)
		fake := &agenttest.FakeClient{ExecuteScriptHook: func(node db.Node, _, _ string) (*nkeproto.ExecuteScriptResponse, error) {
			return readyzResp(node.Name != "cp-0"), nil
		}}
		a := &Activities{nodeAgentClient: fake}
		err := a.probeControlPlaneAPIHealth(context.Background(), nodes)
		require.ErrorContains(t, err, "2/3 control-plane nodes healthy")
		require.ErrorContains(t, err, "cp-0")
	})

	t.Run("one down on 5-CP passes (threshold 4)", func(t *testing.T) {
		t.Parallel()
		nodes := mkNodes(5)
		fake := &agenttest.FakeClient{ExecuteScriptHook: func(node db.Node, _, _ string) (*nkeproto.ExecuteScriptResponse, error) {
			return readyzResp(node.Name != "cp-0"), nil
		}}
		a := &Activities{nodeAgentClient: fake}
		require.NoError(t, a.probeControlPlaneAPIHealth(context.Background(), nodes))
	})

	t.Run("exec error is unhealthy and fails closed", func(t *testing.T) {
		t.Parallel()
		nodes := mkNodes(3)
		fake := &agenttest.FakeClient{ExecuteScriptHook: func(node db.Node, _, _ string) (*nkeproto.ExecuteScriptResponse, error) {
			if node.Name == "cp-1" {
				return nil, fmt.Errorf("dial timeout")
			}
			return readyzResp(true), nil
		}}
		a := &Activities{nodeAgentClient: fake}
		err := a.probeControlPlaneAPIHealth(context.Background(), nodes)
		require.ErrorContains(t, err, "2/3 control-plane nodes healthy")
		require.ErrorContains(t, err, "exec err")
	})
}
