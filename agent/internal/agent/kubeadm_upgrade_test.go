// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
	nkeutils "github.com/dsx-ai-factory/nke/agent/pkg/utils"
)

func TestKubeadmUpgradeApplyValidatesRequest(t *testing.T) {
	tests := []struct {
		name    string
		server  *NodeAgentServer
		version string
		code    codes.Code
	}{
		{
			name:    "empty target version",
			server:  &NodeAgentServer{NodeStatus: &NodeStatus{Role: ControlPlaneRole}},
			version: "",
			code:    codes.InvalidArgument,
		},
		{
			name:    "invalid target version",
			server:  &NodeAgentServer{NodeStatus: &NodeStatus{Role: ControlPlaneRole}},
			version: "1.34",
			code:    codes.InvalidArgument,
		},
		{
			name:    "leading v is rejected",
			server:  &NodeAgentServer{NodeStatus: &NodeStatus{Role: ControlPlaneRole}},
			version: "v1.34.2",
			code:    codes.InvalidArgument,
		},
		{
			name:    "worker node",
			server:  &NodeAgentServer{NodeStatus: &NodeStatus{Role: WorkerRole}},
			version: "1.34.2",
			code:    codes.FailedPrecondition,
		},
		{
			name:    "missing node status",
			server:  &NodeAgentServer{},
			version: "1.34.2",
			code:    codes.FailedPrecondition,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := test.server.KubeadmUpgradeApply(context.Background(), &pb.KubeadmUpgradeApplyRequest{
				TargetVersion: test.version,
			})

			require.Error(t, err)
			assert.Nil(t, response)
			assert.Equal(t, test.code, status.Code(err))
		})
	}
}

func TestKubeadmUpgradeApplyRunsPlanAndApply(t *testing.T) {
	type command struct {
		name string
		args []string
	}
	var calls []command
	runner := &MockRunner{Runners: []runner{
		func(name string, args []string, _ *nkeutils.CommandOptions) (bool, []byte, error) {
			calls = append(calls, command{name: name, args: slices.Clone(args)})
			switch {
			case slices.Equal(args, []string{"upgrade", "plan"}):
				return true, []byte("plan output"), nil
			case slices.Equal(args, []string{"upgrade", "apply", "v1.34.2", "--patches=/etc/kubernetes/patches", "-y"}):
				return true, []byte("apply output"), nil
			default:
				return true, nil, errors.New("unexpected command")
			}
		},
	}}
	server := &NodeAgentServer{
		Runner:     runner,
		NodeStatus: &NodeStatus{Role: ControlPlaneRole},
		RootPath:   t.TempDir(),
	}

	response, err := server.KubeadmUpgradeApply(context.Background(), &pb.KubeadmUpgradeApplyRequest{
		TargetVersion: "1.34.2",
	})

	require.NoError(t, err)
	assert.Equal(t, pb.KubeadmUpgradeApplyResponse_OUTCOME_APPLIED, response.GetOutcome())
	assert.Equal(t, "v1.34.2", response.GetTargetVersion())
	assert.Equal(t, "plan output", response.GetPlanOutput())
	assert.Equal(t, "apply output", response.GetApplyOutput())
	info, statErr := os.Stat(filepath.Join(server.RootPath, "etc", "kubernetes", "patches"))
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())
	require.Len(t, calls, 2)
	assert.Equal(t, command{name: "kubeadm", args: []string{"upgrade", "plan"}}, calls[0])
	assert.Equal(t, command{name: "kubeadm", args: []string{"upgrade", "apply", "v1.34.2", "--patches=/etc/kubernetes/patches", "-y"}}, calls[1])
}

func TestKubeadmUpgradeApplyReportsCommandFailures(t *testing.T) {
	tests := []struct {
		name          string
		runners       []runner
		wantRunCalls  int
		wantErrString string
	}{
		{
			name: "plan failure",
			runners: []runner{runForCommand("kubeadm", []string{"upgrade", "plan"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
				return []byte("plan failed"), errors.New("plan command failed")
			})},
			wantRunCalls:  1,
			wantErrString: "kubeadm upgrade plan failed",
		},
		{
			name: "apply failure",
			runners: []runner{
				runForCommand("kubeadm", []string{"upgrade", "plan"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
					return []byte("plan succeeded"), nil
				}),
				runForCommand("kubeadm", []string{"upgrade", "apply", "v1.34.2", "--patches=/etc/kubernetes/patches", "-y"}, func(string, []string, *nkeutils.CommandOptions) ([]byte, error) {
					return []byte("apply failed"), errors.New("apply command failed")
				}),
			},
			wantRunCalls:  2,
			wantErrString: "kubeadm upgrade apply v1.34.2 failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &MockRunner{Runners: test.runners}
			server := &NodeAgentServer{
				Runner:     runner,
				NodeStatus: &NodeStatus{Role: ControlPlaneRole},
				RootPath:   t.TempDir(),
			}

			response, err := server.KubeadmUpgradeApply(context.Background(), &pb.KubeadmUpgradeApplyRequest{
				TargetVersion: "1.34.2",
			})

			require.Error(t, err)
			assert.Nil(t, response)
			assert.Equal(t, codes.Internal, status.Code(err))
			require.ErrorContains(t, err, test.wantErrString)
			assert.Equal(t, test.wantRunCalls, runner.RunCalls)
		})
	}
}

func TestKubeadmUpgradeApplyRetriesSameTarget(t *testing.T) {
	runner := &MockRunner{Output: []byte("success")}
	server := &NodeAgentServer{
		Runner:     runner,
		NodeStatus: &NodeStatus{Role: ControlPlaneRole},
		RootPath:   t.TempDir(),
	}

	for range 2 {
		response, err := server.KubeadmUpgradeApply(context.Background(), &pb.KubeadmUpgradeApplyRequest{
			TargetVersion: "1.34.2",
		})
		require.NoError(t, err)
		assert.Equal(t, pb.KubeadmUpgradeApplyResponse_OUTCOME_APPLIED, response.GetOutcome())
	}
	assert.Equal(t, 4, runner.RunCalls)
}

func TestKubeadmUpgradeApplyReportsPatchesDirectoryFailure(t *testing.T) {
	rootPath := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(rootPath, "etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootPath, "etc", "kubernetes"), []byte("not a directory"), 0o644))
	runner := &MockRunner{}
	server := &NodeAgentServer{
		Runner:     runner,
		NodeStatus: &NodeStatus{Role: ControlPlaneRole},
		RootPath:   rootPath,
	}

	response, err := server.KubeadmUpgradeApply(context.Background(), &pb.KubeadmUpgradeApplyRequest{
		TargetVersion: "1.34.2",
	})

	require.Error(t, err)
	assert.Nil(t, response)
	assert.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(t, err, "create kubeadm patches directory")
	assert.Zero(t, runner.RunCalls)
}
