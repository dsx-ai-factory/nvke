package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
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
	assert.Equal(t, "installing", entries[0].Status)
}
