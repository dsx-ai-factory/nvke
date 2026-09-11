// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

func TestInstallPackagesAsyncReturnsBeforeCompletion(t *testing.T) {
	s := &NodeAgentServer{
		NodeStatus: &NodeStatus{},
		Persister:  &MockPersister{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := s.InstallPackages(ctx, &pb.InstallPackagesRequest{
		Packages: []string{"nonexistent-test-pkg-async"},
		Async:    true,
	})
	// Handler returns immediately; install may still be running or fail in background.
	require.NoError(t, err)
}

func TestInstallPackagesSyncRequiresPackages(t *testing.T) {
	s := &NodeAgentServer{
		NodeStatus: &NodeStatus{},
		Persister:  &MockPersister{},
	}

	_, err := s.InstallPackages(context.Background(), &pb.InstallPackagesRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no packages specified")
}

func TestInstallPackagesRejectsNodeAgentReplacement(t *testing.T) {
	tests := []struct {
		name string
		pkg  string
	}{
		{name: "named package", pkg: "nke-agent=v0.1.2"},
		{name: "architecture-qualified package", pkg: "nke-agent:arm64=0.1.2"},
		{name: "suite-qualified package", pkg: "nke-agent/stable"},
		{name: "local deb", pkg: "/tmp/nke-agent_0.1.2_arm64.deb"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &NodeAgentServer{NodeStatus: &NodeStatus{}, Persister: &MockPersister{}}
			_, err := s.InstallPackages(context.Background(), &pb.InstallPackagesRequest{Packages: []string{test.pkg}})
			require.Error(t, err)
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		})
	}
}

func TestInstallPackagesAsyncRecordsRequestID(t *testing.T) {
	s := &NodeAgentServer{
		NodeStatus: &NodeStatus{},
		Persister:  &MockPersister{},
	}

	resp, err := s.InstallPackages(context.Background(), &pb.InstallPackagesRequest{
		Packages: []string{"curl"},
		Async:    true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetRequestId())

	entries := s.GetBundleStatusByRequestID(resp.GetRequestId())
	require.Len(t, entries, 1)
	assert.Equal(t, "deb-packages", entries[0].Name)
}
