package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/node-agent/pkg/utils"
)

func TestExecuteNodePreflight(t *testing.T) {
	tmp := t.TempDir()
	sentinelPath := filepath.Join(tmp, "run", "cloud-init.done")
	require.NoError(t, os.MkdirAll(filepath.Dir(sentinelPath), 0o755))
	require.NoError(t, os.WriteFile(sentinelPath, []byte("done\n"), 0o644))

	server := &NodeAgentServer{RootPath: tmp}

	t.Run("file exists passes", func(t *testing.T) {
		resp, err := server.ExecuteNodePreflight(context.Background(), &pb.ExecuteNodePreflightRequest{
			Checks: []*pb.NodePreflightCheck{
				{
					Name: "cloud-init-done",
					Check: &pb.NodePreflightCheck_FileExists{
						FileExists: &pb.FileExistsPreflightCheck{Path: "/run/cloud-init.done"},
					},
				},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, ReadyStatus, resp.GetStatus())
		assert.Empty(t, resp.GetErrorMessage())
		require.Len(t, resp.GetResults(), 1)
		assert.True(t, resp.GetResults()[0].GetPassed())
	})

	t.Run("missing file fails", func(t *testing.T) {
		resp, err := server.ExecuteNodePreflight(context.Background(), &pb.ExecuteNodePreflightRequest{
			Checks: []*pb.NodePreflightCheck{
				{
					Check: &pb.NodePreflightCheck_FileExists{
						FileExists: &pb.FileExistsPreflightCheck{Path: "/run/missing.done"},
					},
				},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, NotReadyStatus, resp.GetStatus())
		assert.Contains(t, resp.GetErrorMessage(), "file does not exist")
	})

	t.Run("check cloud init status done passes", func(t *testing.T) {
		server := &NodeAgentServer{
			Runner: &MockRunner{
				Runners: []runner{
					func(name string, args []string, _ *nkeutils.CommandOptions) (bool, []byte, error) {
						if name == "cloud-init" && len(args) == 1 && args[0] == "status" {
							return true, []byte("status: done\n"), nil
						}
						return false, nil, nil
					},
				},
			},
		}

		resp, err := server.ExecuteNodePreflight(context.Background(), &pb.ExecuteNodePreflightRequest{
			CheckCloudInitStatus: &pb.CheckCloudInitStatus{},
		})
		require.NoError(t, err)
		assert.Equal(t, ReadyStatus, resp.GetStatus())
		require.Len(t, resp.GetResults(), 1)
		assert.Equal(t, defaultCloudInitStatusCheckName, resp.GetResults()[0].GetName())
		assert.True(t, resp.GetResults()[0].GetPassed())
	})

	t.Run("check cloud init status running is notready", func(t *testing.T) {
		server := &NodeAgentServer{
			Runner: &MockRunner{
				Runners: []runner{
					func(name string, args []string, _ *nkeutils.CommandOptions) (bool, []byte, error) {
						if name == "cloud-init" && len(args) == 1 && args[0] == "status" {
							return true, []byte("status: running\n"), nil
						}
						return false, nil, nil
					},
				},
			},
		}

		resp, err := server.ExecuteNodePreflight(context.Background(), &pb.ExecuteNodePreflightRequest{
			CheckCloudInitStatus: &pb.CheckCloudInitStatus{},
		})
		require.NoError(t, err)
		assert.Equal(t, NotReadyStatus, resp.GetStatus())
		assert.False(t, resp.GetResults()[0].GetPassed())
	})

	t.Run("check cloud init status error returns error response", func(t *testing.T) {
		server := &NodeAgentServer{
			Runner: &MockRunner{
				Runners: []runner{
					func(name string, args []string, _ *nkeutils.CommandOptions) (bool, []byte, error) {
						if name == "cloud-init" && len(args) == 1 && args[0] == "status" {
							return true, []byte("status: error\n"), nil
						}
						return false, nil, nil
					},
				},
			},
		}

		resp, err := server.ExecuteNodePreflight(context.Background(), &pb.ExecuteNodePreflightRequest{
			CheckCloudInitStatus: &pb.CheckCloudInitStatus{},
		})
		require.NoError(t, err)
		assert.Equal(t, ErrorStatus, resp.GetStatus())
		assert.Contains(t, resp.GetErrorMessage(), "cloud-init error status")
	})

	t.Run("empty request returns error response", func(t *testing.T) {
		resp, err := server.ExecuteNodePreflight(context.Background(), &pb.ExecuteNodePreflightRequest{})
		require.NoError(t, err)
		assert.Equal(t, ErrorStatus, resp.GetStatus())
		assert.Contains(t, resp.GetErrorMessage(), "at least one node check, check_cloud_init_status, or kubeadm_preflight is required")
	})

	t.Run("missing file path returns error response", func(t *testing.T) {
		resp, err := server.ExecuteNodePreflight(context.Background(), &pb.ExecuteNodePreflightRequest{
			Checks: []*pb.NodePreflightCheck{
				{
					Check: &pb.NodePreflightCheck_FileExists{
						FileExists: &pb.FileExistsPreflightCheck{},
					},
				},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, ErrorStatus, resp.GetStatus())
		assert.Contains(t, resp.GetErrorMessage(), "file_exists.path is required")
	})

	t.Run("relative path returns error response", func(t *testing.T) {
		resp, err := server.ExecuteNodePreflight(context.Background(), &pb.ExecuteNodePreflightRequest{
			Checks: []*pb.NodePreflightCheck{
				{
					Check: &pb.NodePreflightCheck_FileExists{
						FileExists: &pb.FileExistsPreflightCheck{Path: "run/cloud-init.done"},
					},
				},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, ErrorStatus, resp.GetStatus())
		assert.Contains(t, resp.GetErrorMessage(), "path must be absolute")
	})
}

func TestEvaluateCloudInitStatus(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		passed, message, err := evaluateCloudInitStatus("status: done\n")
		require.NoError(t, err)
		assert.True(t, passed)
		assert.Contains(t, message, "cloud-init done")
	})

	t.Run("running", func(t *testing.T) {
		passed, message, err := evaluateCloudInitStatus("status: running\n")
		require.NoError(t, err)
		assert.False(t, passed)
		assert.Contains(t, message, "cloud-init not done")
	})

	t.Run("error", func(t *testing.T) {
		_, _, err := evaluateCloudInitStatus("status: error\n")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cloud-init error status")
	})
}
