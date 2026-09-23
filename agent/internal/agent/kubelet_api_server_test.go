// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nke/agent/pkg/utils"
)

type kubeletCommandRunner struct {
	calls   []string
	results []error
}

func (r *kubeletCommandRunner) Run(name string, args []string, _ *utils.CommandOptions) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if len(r.results) == 0 {
		return nil, nil
	}
	err := r.results[0]
	r.results = r.results[1:]
	return nil, err
}

func (*kubeletCommandRunner) RunSplitOutput(string, []string, *utils.CommandOptions) (utils.CommandOutput, error) {
	return utils.CommandOutput{}, nil
}

func (*kubeletCommandRunner) RunDetached(string, []string, *utils.CommandOptions) error {
	return nil
}

func TestReconfigureKubeletAPIServerApplied(t *testing.T) {
	server, path := newKubeletAPIServerTestServer(t, "https://10.0.0.1:6443", nil)

	response, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{
		Server: "https://api.example.com:6443",
	})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureKubeletAPIServerResponse_OUTCOME_APPLIED, response.GetOutcome())
	assert.Equal(t, "https://api.example.com:6443", readKubeletAPIServer(t, path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, []string{
		"systemctl restart kubelet",
		"systemctl is-active --quiet kubelet",
	}, server.Runner.(*kubeletCommandRunner).calls)
}

func TestReconfigureKubeletAPIServerUnchangedAvoidsRewrite(t *testing.T) {
	server, _ := newKubeletAPIServerTestServer(t, "https://api.example.com:6443", nil)
	writes := 0
	server.kubeletConfigAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		writes++
		return atomicWriteFile(path, data, mode)
	}

	response, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{
		Server: "https://api.example.com:6443",
	})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureKubeletAPIServerResponse_OUTCOME_UNCHANGED, response.GetOutcome())
	assert.Zero(t, writes)
	assert.Equal(t, []string{
		"systemctl restart kubelet",
		"systemctl is-active --quiet kubelet",
	}, server.Runner.(*kubeletCommandRunner).calls)
}

func TestReconfigureKubeletAPIServerRejectsInvalidEndpoint(t *testing.T) {
	tests := []string{
		"",
		"http://api.example.com:6443",
		"https://api.example.com:443",
		"https://api.example.com:6443/",
		"https://user@api.example.com:6443",
		"https://bad_host:6443",
		"https://api.example.com:6443?query=true",
	}

	for _, endpoint := range tests {
		t.Run(endpoint, func(t *testing.T) {
			server, _ := newKubeletAPIServerTestServer(t, "https://10.0.0.1:6443", nil)

			_, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{Server: endpoint})

			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, server.Runner.(*kubeletCommandRunner).calls)
		})
	}
}

func TestValidateKubeletAPIServerAcceptsHostAndIP(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.example.com:6443",
		"https://10.0.0.1:6443",
		"https://[2001:db8::1]:6443",
	} {
		t.Run(endpoint, func(t *testing.T) {
			assert.NoError(t, validateKubeletAPIServer(endpoint))
		})
	}
}

func TestReconfigureKubeletAPIServerRejectsMissingOrMalformedConfig(t *testing.T) {
	tests := []struct {
		name     string
		contents []byte
	}{
		{name: "missing"},
		{name: "malformed", contents: []byte("not: [valid")},
		{name: "missing current context", contents: []byte("apiVersion: v1\nkind: Config\n")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.contents != nil {
				path := filepath.Join(root, strings.TrimPrefix(kubeletKubeConfigPath, "/"))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, test.contents, 0o600))
			}
			server := &NodeAgentServer{RootPath: root, Runner: &kubeletCommandRunner{}}

			_, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{
				Server: "https://api.example.com:6443",
			})

			require.Error(t, err)
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))
			assert.Empty(t, server.Runner.(*kubeletCommandRunner).calls)
		})
	}
}

func TestReconfigureKubeletAPIServerAtomicWriteFailureLeavesOriginal(t *testing.T) {
	server, path := newKubeletAPIServerTestServer(t, "https://10.0.0.1:6443", nil)
	server.kubeletConfigAtomicWrite = func(string, []byte, os.FileMode) error {
		return errors.New("write failed")
	}

	_, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{
		Server: "https://api.example.com:6443",
	})

	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Equal(t, "https://10.0.0.1:6443", readKubeletAPIServer(t, path))
	assert.Empty(t, server.Runner.(*kubeletCommandRunner).calls)
}

func TestReconfigureKubeletAPIServerRestartFailureRestoresOriginal(t *testing.T) {
	runner := &kubeletCommandRunner{results: []error{errors.New("restart failed"), nil, nil, nil}}
	server, path := newKubeletAPIServerTestServer(t, "https://10.0.0.1:6443", runner)

	response, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{
		Server: "https://api.example.com:6443",
	})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_RESTORED, response.GetOutcome())
	assert.Contains(t, response.GetErrorMessage(), "restart failed")
	assert.Equal(t, "https://10.0.0.1:6443", readKubeletAPIServer(t, path))
	assert.Equal(t, []string{
		"systemctl restart kubelet",
		"systemctl is-active --quiet kubelet",
		"systemctl restart kubelet",
		"systemctl is-active --quiet kubelet",
	}, runner.calls)
}

func TestReconfigureKubeletAPIServerVerificationFailureRestoresOriginal(t *testing.T) {
	runner := &kubeletCommandRunner{results: []error{nil, errors.New("kubelet inactive"), nil, nil}}
	server, path := newKubeletAPIServerTestServer(t, "https://10.0.0.1:6443", runner)

	response, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{
		Server: "https://api.example.com:6443",
	})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_RESTORED, response.GetOutcome())
	assert.Contains(t, response.GetErrorMessage(), "kubelet inactive")
	assert.Equal(t, "https://10.0.0.1:6443", readKubeletAPIServer(t, path))
}

func TestReconfigureKubeletAPIServerRollbackFailureIsReported(t *testing.T) {
	runner := &kubeletCommandRunner{results: []error{errors.New("restart failed"), nil, nil, nil}}
	server, path := newKubeletAPIServerTestServer(t, "https://10.0.0.1:6443", runner)
	writes := 0
	server.kubeletConfigAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		writes++
		if writes == 2 {
			return errors.New("rollback write failed")
		}
		return atomicWriteFile(path, data, mode)
	}

	response, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{
		Server: "https://api.example.com:6443",
	})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_NOT_RESTORED, response.GetOutcome())
	assert.Contains(t, response.GetErrorMessage(), "rollback write failed")
	assert.Equal(t, "https://api.example.com:6443", readKubeletAPIServer(t, path))
	assert.Equal(t, []string{
		"systemctl restart kubelet",
		"systemctl is-active --quiet kubelet",
		"systemctl restart kubelet",
		"systemctl is-active --quiet kubelet",
	}, runner.calls)
}

func TestReconfigureKubeletAPIServerRollbackServiceFailureIsReported(t *testing.T) {
	runner := &kubeletCommandRunner{results: []error{errors.New("restart failed"), nil, errors.New("restore restart failed"), nil}}
	server, path := newKubeletAPIServerTestServer(t, "https://10.0.0.1:6443", runner)

	response, err := server.ReconfigureKubeletAPIServer(context.Background(), &pb.ReconfigureKubeletAPIServerRequest{
		Server: "https://api.example.com:6443",
	})

	require.NoError(t, err)
	assert.Equal(t, pb.ReconfigureKubeletAPIServerResponse_OUTCOME_FAILED_NOT_RESTORED, response.GetOutcome())
	assert.Contains(t, response.GetErrorMessage(), "restore restart failed")
	assert.Equal(t, "https://10.0.0.1:6443", readKubeletAPIServer(t, path))
}

func newKubeletAPIServerTestServer(
	t *testing.T,
	endpoint string,
	runner *kubeletCommandRunner,
) (*NodeAgentServer, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, strings.TrimPrefix(kubeletKubeConfigPath, "/"))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	contents, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"cluster": {Server: endpoint},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"context": {Cluster: "cluster", AuthInfo: "kubelet"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"kubelet": {},
		},
		CurrentContext: "context",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, contents, 0o644))

	if runner == nil {
		runner = &kubeletCommandRunner{}
	}
	return &NodeAgentServer{RootPath: root, Runner: runner}, path
}

func readKubeletAPIServer(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	config, cluster, err := loadKubeletConfig(contents)
	require.NoError(t, err)
	require.NotNil(t, config)
	return cluster.Server
}
