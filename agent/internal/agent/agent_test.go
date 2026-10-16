// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v2"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

var initOutput = `Your Kubernetes control-plane has initialized successfully!

To start using your cluster, you need to run the following as a regular user:

  mkdir -p $HOME/.kube
  sudo cp -i /etc/kubernetes/admin.conf $HOME/.kube/config
  sudo chown $(id -u):$(id -g) $HOME/.kube/config

You should now deploy a pod network to the cluster.
Run "kubectl apply -f [podnetwork].yaml" with one of the options listed at:
  https://kubernetes.io/docs/concepts/cluster-administration/networking/

Then you can join any number of worker nodes by running the following on each as root:

  kubeadm join 10.128.0.1:6443 --token abcdef.0123456789abcdef \
    --discovery-token-ca-cert-hash sha256:11223344556677889900aabbccddeeff...

To add another control plane node, run the following on the new control plane node:

  kubeadm join 10.128.0.1:6443 --token abcdef.0123456789abcdef \
    --discovery-token-ca-cert-hash sha256:11223344556677889900aabbccddeeff... \
    --control-plane --certificate-key a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6
`

type MockRunner struct {
	Output   []byte
	Stderr   []byte
	Err      error
	Runners  []runner
	RunCalls int
}

type concurrentScriptRunner struct {
	started chan struct{}
	release <-chan struct{}

	mu    sync.Mutex
	paths []string
}

func (r *concurrentScriptRunner) Run(
	name string,
	args []string,
	opts *nvkeutils.CommandOptions,
) ([]byte, error) {
	r.mu.Lock()
	r.paths = append(r.paths, name)
	r.mu.Unlock()

	r.started <- struct{}{}
	<-r.release

	return (&nvkeutils.ExecRunner{}).Run(name, args, opts)
}

func (r *concurrentScriptRunner) RunSplitOutput(
	name string,
	args []string,
	opts *nvkeutils.CommandOptions,
) (nvkeutils.CommandOutput, error) {
	output, err := r.Run(name, args, opts)
	return nvkeutils.CommandOutput{Stdout: output}, err
}

func (r *concurrentScriptRunner) RunDetached(
	string,
	[]string,
	*nvkeutils.CommandOptions,
) error {
	return errors.New("detached execution is not supported")
}

func (r *concurrentScriptRunner) Paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

type (
	runner  func(string, []string, *nvkeutils.CommandOptions) (match bool, output []byte, err error)
	runFunc func(string, []string, *nvkeutils.CommandOptions) ([]byte, error)
)

func (m *MockRunner) Run(
	name string,
	args []string,
	opts *nvkeutils.CommandOptions,
) ([]byte, error) { //nolint:ireturntring
	m.RunCalls++
	for _, runner := range m.Runners {
		match, output, err := runner(name, args, opts)
		if match {
			return output, err
		}
	}
	return m.Output, m.Err
}

func (m *MockRunner) RunSplitOutput(
	name string,
	args []string,
	opts *nvkeutils.CommandOptions,
) (nvkeutils.CommandOutput, error) {
	output, err := m.Run(name, args, opts)
	return nvkeutils.CommandOutput{Stdout: output, Stderr: m.Stderr}, err
}

func (m *MockRunner) RunDetached(
	name string,
	args []string,
	opts *nvkeutils.CommandOptions,
) error { //nolint:ireturntring
	return m.Err
}

func runForCommand(wantName string, wantArgs []string, run runFunc) runner {
	return func(name string, args []string, opts *nvkeutils.CommandOptions) (bool, []byte, error) {
		if name != wantName {
			return false, nil, nil
		}

		if slices.Equal(args, wantArgs) {
			output, err := run(name, args, opts)
			return true, output, err
		}
		return false, nil, nil
	}
}

type MockFileWriter struct {
	Err          error
	WrittenPath  string
	WrittenData  []byte
	WrittenPerms os.FileMode
}

func (m *MockFileWriter) WriteFile(filename string, data []byte, perm os.FileMode) error {
	m.WrittenPath = filename
	m.WrittenData = data
	m.WrittenPerms = perm
	return m.Err
}

type MockPersister struct {
	mu      sync.Mutex
	Called  bool
	Errored bool
}

func (m *MockPersister) Persist(status *NodeStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Called = true
	if m.Errored {
		return errors.New("persist failed")
	}
	return nil
}

func (m *MockPersister) WasCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Called
}

func (m *MockPersister) LoadFromStateFile() (*NodeStatus, error) {
	return &NodeStatus{}, nil
}

func TestExecuteKubeadmJoin(t *testing.T) {
	tests := []struct {
		name                   string
		nodeStatus             *NodeStatus
		kubeadmRequest         *pb.KubeadmJoinRequest
		mockRunner             *MockRunner
		mockWriter             *MockFileWriter
		mockPersister          *MockPersister
		expectedStatus         string
		expectedResponseStatus string
		expectedPhase          string
		persisterCalled        bool
	}{
		{
			name:       "Error - Node join pending",
			nodeStatus: &NodeStatus{Status: PendingStatus},
			kubeadmRequest: &pb.KubeadmJoinRequest{
				KubeadmJoinString: "kubeadm join 1.2.3.4:6443 --token abcdef",
			},
			mockRunner:             &MockRunner{Output: []byte("joined"), Err: nil},
			mockWriter:             &MockFileWriter{},
			mockPersister:          &MockPersister{},
			expectedResponseStatus: "pending",
			expectedPhase:          JoinPhase,
			persisterCalled:        true,
		},
		{
			name:       "Already joined - Error join ready",
			nodeStatus: &NodeStatus{Phase: JoinPhase, Status: ReadyStatus},
			kubeadmRequest: &pb.KubeadmJoinRequest{
				KubeadmJoinString: "kubeadm join 1.2.3.4:6443 --token abcdef",
			},
			mockRunner:             &MockRunner{Output: []byte("joined"), Err: nil},
			mockWriter:             &MockFileWriter{},
			mockPersister:          &MockPersister{},
			expectedStatus:         ReadyStatus,
			expectedPhase:          JoinPhase,
			expectedResponseStatus: "error",
			persisterCalled:        true, // We've moved persist into the defer so it should ALWAYS be called
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &NodeAgentServer{
				Runner:     tt.mockRunner,
				FileWriter: tt.mockWriter,
				Persister:  tt.mockPersister,
				NodeStatus: tt.nodeStatus,
			}

			resp, _ := server.ExecuteKubeadmJoin(context.Background(), tt.kubeadmRequest)
			fmt.Printf("DEBUG: resp: %+v\n", resp)
			assert.Equal(t, tt.expectedResponseStatus, resp.Status)
		})
	}
}

func TestExecuteKubeadmInit(t *testing.T) {
	tests := []struct {
		name                 string
		nodeStatus           *NodeStatus
		req                  *pb.KubeadmInitRequest
		writerErr            error
		expectResponseStatus string
		mockRunner           *MockRunner
	}{
		{
			name:                 "success with standard args",
			expectResponseStatus: "pending",
			nodeStatus: &NodeStatus{
				Status: NotReadyStatus,
				Phase:  "",
			},
			req: &pb.KubeadmInitRequest{
				PodNetworkCidr:       "10.244.0.0/16",
				ControlPlaneEndpoint: "192.168.0.1:6443",
				NoCniInstall:         true,
			},
			mockRunner: &MockRunner{Output: []byte(initOutput), Err: nil},
		},
		{
			name:                 "missing pod network CIDR",
			expectResponseStatus: "error",
			nodeStatus: &NodeStatus{
				Status: NotReadyStatus,
				Phase:  "",
			},
			req: &pb.KubeadmInitRequest{
				ControlPlaneEndpoint: "192.168.0.1:6443",
				PodNetworkCidr:       "",
			},
			mockRunner: &MockRunner{Output: []byte(initOutput), Err: nil},
		},
		{
			name:                 "missing control plane endpoint",
			expectResponseStatus: "error",
			nodeStatus: &NodeStatus{
				Status: NotReadyStatus,
				Phase:  "",
			},
			req: &pb.KubeadmInitRequest{
				PodNetworkCidr:       "10.244.0.0/16",
				ControlPlaneEndpoint: "",
			},
			mockRunner: &MockRunner{Output: []byte("joined"), Err: nil},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockWriter := &MockFileWriter{Err: tc.writerErr}
			mockPersister := &MockPersister{}

			server := &NodeAgentServer{
				Runner:     tc.mockRunner,
				FileWriter: mockWriter,
				Persister:  mockPersister,
				NodeStatus: tc.nodeStatus,
			}

			resp, _ := server.ExecuteKubeadmInit(context.Background(), tc.req)
			assert.Equal(t, tc.expectResponseStatus, resp.Status)
			// assert.True(t, mockPersister.Called)
		})
	}
}

func TestCanonicalKubernetesVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"  ", ""},
		{"v1.31.9", "v1.31.9"},
		{"1.31.9", "v1.31.9"},
		{" v1.28.0 ", "v1.28.0"},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d_%q", i, tc.in), func(t *testing.T) {
			assert.Equal(t, tc.want, canonicalKubernetesVersion(tc.in))
		})
	}
}

func TestExtraArgsContainKubernetesVersion(t *testing.T) {
	assert.False(t, extraArgsContainKubernetesVersion(nil))
	assert.False(t, extraArgsContainKubernetesVersion([]string{"--foo=bar"}))
	assert.True(t, extraArgsContainKubernetesVersion([]string{"  --kubernetes-version=v1.2.3  "}))
}

func TestProcessKubeadmInit(t *testing.T) {
	tests := []struct {
		name             string
		initConfig       string
		writeErr         error
		cmdOutput        string
		cmdErr           error
		expectStatus     string
		expectJoinParsed bool
	}{
		{
			name:             "successful init with parsed join strings",
			cmdOutput:        initOutput,
			cmdErr:           nil,
			expectStatus:     ReadyStatus,
			expectJoinParsed: true,
		},
		{
			name:         "init already initialized - error detected in output",
			cmdOutput:    "error execution phase preflight: [preflight] Some files are available: /etc/kubernetes/admin.conf is in use",
			cmdErr:       errors.New("already initialized"),
			expectStatus: ErrorStatus,
		},
		{
			name:         "failure to write config file",
			initConfig:   "invalid",
			writeErr:     errors.New("write failed"),
			expectStatus: ErrorStatus,
		},
		{
			name:         "output parse failure",
			cmdOutput:    "random output without join string",
			cmdErr:       nil,
			expectStatus: ErrorStatus,
		},
		{
			name:         "generic kubeadm failure",
			cmdOutput:    "some error occurred",
			cmdErr:       errors.New("failure"),
			expectStatus: ErrorStatus,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockWriter := &MockFileWriter{Err: tc.writeErr}
			mockPersister := &MockPersister{}
			mockRunner := &MockRunner{Output: []byte(tc.cmdOutput), Err: tc.cmdErr}

			server := &NodeAgentServer{
				Runner:     mockRunner,
				FileWriter: mockWriter,
				Persister:  mockPersister,
				NodeStatus: &NodeStatus{
					Status: PendingStatus,
					Phase:  InitPhase,
				},
			}

			server.processKubeadmInit([]string{}, &pb.KubeadmInitRequest{
				InitConfig:   tc.initConfig,
				NoCniInstall: true,
			})

			assert.Equal(t, tc.expectStatus, server.NodeStatus.Status)

			if tc.expectJoinParsed {
				assert.NotEmpty(t, server.NodeStatus.JoinInfo.ControlPlaneJoinString)
				assert.NotEmpty(t, server.NodeStatus.JoinInfo.WorkerJoinString)
			}
		})
	}
}

func TestInstallCNIPluginRequestFromKubeadmInit(t *testing.T) {
	t.Run("defaults to ovn with default version", func(t *testing.T) {
		out := installCNIPluginRequestFromKubeadmInit(&pb.KubeadmInitRequest{
			PodNetworkCidr: "10.0.0.0/16",
			ServiceCidr:    "10.96.0.0/12",
		})
		assert.Equal(t, "ovn", out.GetPluginName())
		assert.Equal(t, defaultOvnCniChartStream, out.GetPluginVersion())
		assert.Equal(t, "10.0.0.0/16", out.GetPodNetworkCidr())
		assert.Equal(t, "10.96.0.0/12", out.GetServiceCidr())
	})
	t.Run("ovn uses ovn_version", func(t *testing.T) {
		out := installCNIPluginRequestFromKubeadmInit(&pb.KubeadmInitRequest{
			CniPluginName: "ovn",
			OvnVersion:    "1.2.3",
		})
		assert.Equal(t, "ovn", out.GetPluginName())
		assert.Equal(t, "1.2.3", out.GetPluginVersion())
	})
	t.Run("empty plugin keeps an explicit ovn version", func(t *testing.T) {
		out := installCNIPluginRequestFromKubeadmInit(&pb.KubeadmInitRequest{
			OvnVersion: "1.2.3",
		})
		assert.Equal(t, "ovn", out.GetPluginName())
		assert.Equal(t, "1.2.3", out.GetPluginVersion())
	})
	t.Run("tigera uses tigera_version", func(t *testing.T) {
		out := installCNIPluginRequestFromKubeadmInit(&pb.KubeadmInitRequest{
			CniPluginName: "tigera",
			TigeraVersion: "3.29.0",
		})
		assert.Equal(t, "3.29.0", out.GetPluginVersion())
	})
}

func TestProcessKubeadmInit_FilesWrittenBeforeKubeadm(t *testing.T) {
	server := newKubeadmFileTestServer(t)
	server.NodeStatus = &NodeStatus{
		Status: PendingStatus,
		Phase:  InitPhase,
		Role:   ControlPlaneRole,
	}

	expectedPath := filepath.Join(server.RootPath, "etc", "kubernetes", "prereq.yaml")
	expectedContent := "apiVersion: v1\nkind: ConfigMap\n"
	secondExpectedPath := filepath.Join(server.RootPath, "etc", "kubernetes", "oidc", "auth-config.yaml")
	secondExpectedContent := "issuer: https://issuer.example.com\n"
	server.Runner = &MockRunner{
		Runners: []runner{
			runForCommand("kubeadm", []string{"init", "--upload-certs"}, func(string, []string, *nvkeutils.CommandOptions) ([]byte, error) {
				data, err := os.ReadFile(expectedPath)
				require.NoError(t, err)
				require.Equal(t, expectedContent, string(data))

				data, err = os.ReadFile(secondExpectedPath)
				require.NoError(t, err)
				require.Equal(t, secondExpectedContent, string(data))

				return []byte(initOutput), nil
			}),
		},
	}

	server.processKubeadmInit([]string{"init", "--upload-certs"}, &pb.KubeadmInitRequest{
		NoCniInstall: true,
		Files: []*pb.File{
			{
				Path:    "/etc/kubernetes/prereq.yaml",
				Content: []byte(expectedContent),
				Mode:    0o644,
				Owner:   "root:root",
			},
			{
				Path:    "/etc/kubernetes/oidc/auth-config.yaml",
				Content: []byte(secondExpectedContent),
				Mode:    0o644,
				Owner:   "root:root",
			},
		},
	})

	assert.Equal(t, ReadyStatus, server.NodeStatus.Status)
	assert.Equal(t, "init succeeded", server.NodeStatus.KubeadmStatus)
}

func TestProcessKubeadmJoin_FilesWrittenBeforeKubeadm(t *testing.T) {
	server := newKubeadmFileTestServer(t)
	server.NodeStatus = &NodeStatus{
		Status: PendingStatus,
		Phase:  JoinPhase,
		Role:   WorkerRole,
	}

	expectedPath := filepath.Join(server.RootPath, "etc", "kubernetes", "join-prereq.yaml")
	expectedContent := "discovery: enabled\n"
	server.Runner = &MockRunner{
		Runners: []runner{
			runForCommand("kubeadm", []string{"join", "10.0.0.1:6443"}, func(string, []string, *nvkeutils.CommandOptions) ([]byte, error) {
				data, err := os.ReadFile(expectedPath)
				require.NoError(t, err)
				require.Equal(t, expectedContent, string(data))

				return []byte("joined"), nil
			}),
		},
	}

	server.processKubeadmJoin([]string{"join", "10.0.0.1:6443"}, &pb.KubeadmJoinRequest{
		Files: []*pb.File{{
			Path:    "/etc/kubernetes/join-prereq.yaml",
			Content: []byte(expectedContent),
			Mode:    0o644,
			Owner:   "root:root",
		}},
	})

	assert.Equal(t, ReadyStatus, server.NodeStatus.Status)
	assert.Equal(t, "join succeeded", server.NodeStatus.KubeadmStatus)
}

func TestProcessKubeadmJoin_ResetsBeforeRetry(t *testing.T) {
	tests := []struct {
		name          string
		kubeadmStatus string
		wantCommands  []string
	}{
		{name: "retry after a failed join resets first", kubeadmStatus: joinFailedStatus, wantCommands: []string{"reset", "join"}},
		{name: "first join does not reset", kubeadmStatus: "", wantCommands: []string{"join"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newKubeadmFileTestServer(t)
			server.NodeStatus = &NodeStatus{
				Status:        PendingStatus,
				Phase:         JoinPhase,
				Role:          WorkerRole,
				KubeadmStatus: tc.kubeadmStatus,
			}

			var commands []string
			record := func(_ string, args []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
				commands = append(commands, args[0])
				return []byte("ok"), nil
			}
			server.Runner = &MockRunner{
				Runners: []runner{
					runForCommand("kubeadm", []string{"reset", "-f"}, record),
					runForCommand("kubeadm", []string{"join", "10.0.0.1:6443"}, record),
				},
			}

			server.processKubeadmJoin([]string{"join", "10.0.0.1:6443"}, &pb.KubeadmJoinRequest{})

			assert.Equal(t, tc.wantCommands, commands)
			assert.Equal(t, ReadyStatus, server.NodeStatus.Status)
			assert.Equal(t, "join succeeded", server.NodeStatus.KubeadmStatus)
		})
	}
}

func TestProcessKubeadmInit_FilesWriteFailurePreventsKubeadm(t *testing.T) {
	server := newKubeadmFileTestServer(t)
	server.NodeStatus = &NodeStatus{
		Status: PendingStatus,
		Phase:  InitPhase,
		Role:   ControlPlaneRole,
	}

	runner := &MockRunner{
		Runners: []runner{
			runForCommand("kubeadm", []string{"init", "--upload-certs"}, func(string, []string, *nvkeutils.CommandOptions) ([]byte, error) {
				t.Fatalf("kubeadm should not run when prerequisite files fail")
				return nil, nil
			}),
		},
	}
	server.Runner = runner

	server.processKubeadmInit([]string{"init", "--upload-certs"}, &pb.KubeadmInitRequest{
		NoCniInstall: true,
		Files: []*pb.File{{
			Path: "etc/kubernetes/prereq.yaml", // invalid path, not absolute, it should fail validation
		}},
	})

	assert.Equal(t, ErrorStatus, server.NodeStatus.Status)
	assert.Contains(t, server.NodeStatus.ErrorMessage, "failed to write kubeadm prerequisite files")
	assert.Equal(t, 0, runner.RunCalls)

	entries, err := os.ReadDir(server.RootPath)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestProcessKubeadmJoin_FileWriteFailsBeforeKubeadm(t *testing.T) {
	server := newKubeadmFileTestServer(t)
	server.NodeStatus = &NodeStatus{
		Status: PendingStatus,
		Phase:  JoinPhase,
		Role:   WorkerRole,
	}

	runner := &MockRunner{
		Runners: []runner{
			func(name string, args []string, opts *nvkeutils.CommandOptions) (bool, []byte, error) {
				t.Fatalf("kubeadm should not run when prerequisite files fail")
				return true, nil, nil
			},
		},
	}
	server.Runner = runner

	server.processKubeadmJoin([]string{"join", "10.0.0.1:6443"}, &pb.KubeadmJoinRequest{
		Files: []*pb.File{{
			Path: "etc/kubernetes/prereq.yaml", // invalid path, not absolute, it should fail validation
		}},
	})

	assert.Equal(t, ErrorStatus, server.NodeStatus.Status)
	assert.Contains(t, server.NodeStatus.ErrorMessage, "Failed to write kubeadm prerequisite files")
	assert.Equal(t, 0, runner.RunCalls)
}

func TestGetBundleStatus(t *testing.T) {
	server := &NodeAgentServer{
		NodeStatus: &NodeStatus{
			Bundles: []Bundle{
				{
					RequestID:   "req-123",
					Name:        "bundle-a",
					Version:     semver.MustParse("1.0.0"),
					Status:      "success",
					Error:       "",
					Output:      "installation complete",
					ExitCode:    0,
					InstallTime: "2024-01-01T10:00:00Z",
					Source:      "https://example.com/bundle-a",
				},
				{
					RequestID:   "req-456",
					Name:        "bundle-b",
					Version:     semver.MustParse("2.0.0"),
					Status:      "error",
					Error:       "installation failed",
					Output:      "error output",
					ExitCode:    1,
					InstallTime: "2024-01-01T11:00:00Z",
					Source:      "https://example.com/bundle-b",
				},
			},
		},
	}

	resp, err := server.GetBundleStatus(context.Background(), &pb.GetBundleStatusRequest{
		RequestId: "req-123",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 1, len(resp.Statuses))

	status := resp.Statuses[0]
	assert.Equal(t, "req-123", status.RequestId)
	assert.Equal(t, "bundle-a", status.Name)
	assert.Equal(t, "1.0.0", status.Version)
	assert.Equal(t, "success", status.Status)
	assert.Equal(t, "", status.Error)
	assert.Equal(t, "installation complete", status.Output)
	assert.Equal(t, "0", status.ExitCode)
}

func TestExecuteKubeadmPreflight(t *testing.T) {
	sampleConfig := "apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"

	tests := []struct {
		name           string
		req            *pb.KubeadmPreflightRequest
		mockRunner     *MockRunner
		mockWriter     *MockFileWriter
		expectedStatus string
		expectedErr    string
		expectResults  int
	}{
		{
			name: "missing config",
			req: &pb.KubeadmPreflightRequest{
				Config: "",
				Role:   pb.KubeadmPreflightRole_KUBEADM_PREFLIGHT_ROLE_INIT,
			},
			mockRunner:     &MockRunner{},
			mockWriter:     &MockFileWriter{},
			expectedStatus: ErrorStatus,
			expectedErr:    "config is required",
		},
		{
			name: "config file write failure",
			req: &pb.KubeadmPreflightRequest{
				Config: sampleConfig,
				Role:   pb.KubeadmPreflightRole_KUBEADM_PREFLIGHT_ROLE_INIT,
			},
			mockRunner:     &MockRunner{},
			mockWriter:     &MockFileWriter{Err: errors.New("disk full")},
			expectedStatus: ErrorStatus,
			expectedErr:    "failed to write preflight config file",
		},
		{
			name: "successful init preflight",
			req: &pb.KubeadmPreflightRequest{
				Config: sampleConfig,
				Role:   pb.KubeadmPreflightRole_KUBEADM_PREFLIGHT_ROLE_INIT,
			},
			mockRunner:     &MockRunner{Output: []byte("[preflight] Running pre-flight checks\n[preflight] Pulling images...\n"), Err: nil},
			mockWriter:     &MockFileWriter{},
			expectedStatus: ReadyStatus,
		},
		{
			name: "successful join preflight",
			req: &pb.KubeadmPreflightRequest{
				Config: sampleConfig,
				Role:   pb.KubeadmPreflightRole_KUBEADM_PREFLIGHT_ROLE_JOIN,
			},
			mockRunner:     &MockRunner{Output: []byte("[preflight] Running pre-flight checks\n"), Err: nil},
			mockWriter:     &MockFileWriter{},
			expectedStatus: ReadyStatus,
		},
		{
			name: "preflight with warnings",
			req: &pb.KubeadmPreflightRequest{
				Config: sampleConfig,
				Role:   pb.KubeadmPreflightRole_KUBEADM_PREFLIGHT_ROLE_INIT,
			},
			mockRunner: &MockRunner{
				Output: []byte("[preflight] Running pre-flight checks\n[WARNING Firewalld]: firewalld is active\n"),
				Err:    nil,
			},
			mockWriter:     &MockFileWriter{},
			expectedStatus: ReadyStatus,
			expectResults:  1,
		},
		{
			name: "preflight with errors",
			req: &pb.KubeadmPreflightRequest{
				Config: sampleConfig,
				Role:   pb.KubeadmPreflightRole_KUBEADM_PREFLIGHT_ROLE_INIT,
			},
			mockRunner: &MockRunner{
				Output: []byte("[preflight] Running pre-flight checks\n[ERROR FileAvailable--etc-kubernetes-manifests-kube-apiserver.yaml]: /etc/kubernetes/manifests/kube-apiserver.yaml already exists\n[ERROR Port-6443]: Port 6443 is in use\n"),
				Err:    errors.New("exit status 1"),
			},
			mockWriter:     &MockFileWriter{},
			expectedStatus: ErrorStatus,
			expectedErr:    "kubeadm init phase preflight failed",
			expectResults:  2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := &NodeAgentServer{
				Runner:     tc.mockRunner,
				FileWriter: tc.mockWriter,
				Persister:  &MockPersister{},
				NodeStatus: &NodeStatus{},
			}

			resp, err := server.ExecuteKubeadmPreflight(context.Background(), tc.req)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedStatus, resp.Status)

			if tc.expectedErr != "" {
				assert.Contains(t, resp.ErrorMessage, tc.expectedErr)
			}

			if tc.expectResults > 0 {
				assert.Len(t, resp.Results, tc.expectResults)
			}
		})
	}
}

func TestParsePreflightOutput(t *testing.T) {
	t.Run("bracketed warnings and errors", func(t *testing.T) {
		// Matches actual kubeadm v1.34 output format: tab-prefixed [WARNING]/[ERROR]
		// lines, with errors inside the "Some fatal errors occurred" block.
		output := "[preflight] Running pre-flight checks\n" +
			"\t[WARNING Firewalld]: firewalld is active, please ensure ports are open\n" +
			"\t[WARNING Service-Docker]: docker service is not enabled\n" +
			"[preflight] Some fatal errors occurred:\n" +
			"\t[ERROR FileAvailable--etc-kubernetes-manifests-kube-apiserver.yaml]: /etc/kubernetes/manifests/kube-apiserver.yaml already exists\n" +
			"\t[ERROR Port-6443]: Port 6443 is in use\n" +
			"[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`\n"

		results := parsePreflightOutput(output)
		assert.Len(t, results, 4)

		assert.Equal(t, "Firewalld", results[0].Name)
		assert.Equal(t, "warning", results[0].Severity)
		assert.Contains(t, results[0].Message, "firewalld is active")

		assert.Equal(t, "Service-Docker", results[1].Name)
		assert.Equal(t, "warning", results[1].Severity)

		assert.Equal(t, "FileAvailable--etc-kubernetes-manifests-kube-apiserver.yaml", results[2].Name)
		assert.Equal(t, "error", results[2].Severity)

		assert.Equal(t, "Port-6443", results[3].Name)
		assert.Equal(t, "error", results[3].Severity)
		assert.Contains(t, results[3].Message, "Port 6443 is in use")
	})

	t.Run("klog CRI connection warning", func(t *testing.T) {
		// When the CRI socket can't connect, kubeadm emits a klog.Warningf
		// with the format: [preflight] WARNING: Couldn't create the interface ...
		// This happens before checks run, so the check is never registered.
		output := "[preflight] Running pre-flight checks\n" +
			"[preflight] WARNING: Couldn't create the interface used for talking to the container runtime: " +
			"validate service connection: validate CRI v1 runtime API for endpoint " +
			"\"unix:///var/run/containerd/containerd.sock\": rpc error: code = Unimplemented\n" +
			"[preflight] Some fatal errors occurred:\n" +
			"\t[ERROR CRI]: container runtime is not running\n" +
			"[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`\n"

		results := parsePreflightOutput(output)
		assert.Len(t, results, 2)

		assert.Equal(t, "Preflight", results[0].Name)
		assert.Equal(t, "warning", results[0].Severity)
		assert.Contains(t, results[0].Message, "Couldn't create the interface")

		assert.Equal(t, "CRI", results[1].Name)
		assert.Equal(t, "error", results[1].Severity)
	})

	t.Run("multiple errors in fatal block", func(t *testing.T) {
		output := "[preflight] Running pre-flight checks\n" +
			"\t[WARNING Swap]: swap is enabled\n" +
			"[preflight] Some fatal errors occurred:\n" +
			"\t[ERROR Port-6443]: Port 6443 is in use\n" +
			"\t[ERROR Port-10259]: Port 10259 is in use\n" +
			"\t[ERROR FileAvailable--etc-kubernetes-manifests-kube-apiserver.yaml]: /etc/kubernetes/manifests/kube-apiserver.yaml already exists\n" +
			"[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`\n"

		results := parsePreflightOutput(output)
		assert.Len(t, results, 4)
		assert.Equal(t, "warning", results[0].Severity)
		assert.Equal(t, "Swap", results[0].Name)
		assert.Equal(t, "Port-6443", results[1].Name)
		assert.Equal(t, "Port-10259", results[2].Name)
		assert.Equal(t, "FileAvailable--etc-kubernetes-manifests-kube-apiserver.yaml", results[3].Name)
	})

	t.Run("clean output produces no results", func(t *testing.T) {
		output := "[preflight] Running pre-flight checks\n" +
			"[preflight] Pulling images required for setting up a Kubernetes cluster\n" +
			"[preflight] This might take a minute or two, depending on the speed of your internet connection\n"

		results := parsePreflightOutput(output)
		assert.Len(t, results, 0)
	})

	t.Run("suggestion in warning", func(t *testing.T) {
		// InPathCheck appends "\nSuggestion: ..." to the warning message.
		// Since it's part of the %v format arg, the newline is inside the message.
		output := "\t[WARNING FileExisting-foobar]: foobar not found in system path\n" +
			"Suggestion: install foobar\n"

		results := parsePreflightOutput(output)
		assert.Len(t, results, 1)
		assert.Equal(t, "FileExisting-foobar", results[0].Name)
		assert.Equal(t, "warning", results[0].Severity)
		assert.Equal(t, "foobar not found in system path", results[0].Message)
	})
}

// mockStreamServer collects chunks sent via StreamExecuteScript so we can
// inspect them in tests.
type mockStreamServer struct {
	chunks  []*pb.ScriptOutputChunk
	ctxFunc func() context.Context
}

func (m *mockStreamServer) Send(chunk *pb.ScriptOutputChunk) error {
	m.chunks = append(m.chunks, chunk)
	return nil
}

func (m *mockStreamServer) Context() context.Context     { return m.ctxFunc() }
func (m *mockStreamServer) SetHeader(metadata.MD) error  { return nil }
func (m *mockStreamServer) SendHeader(metadata.MD) error { return nil }
func (m *mockStreamServer) SetTrailer(metadata.MD)       {}
func (m *mockStreamServer) SendMsg(interface{}) error    { return nil }
func (m *mockStreamServer) RecvMsg(interface{}) error    { return nil }

func TestScriptResolution(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "isolates concurrent inline script contents",
			run: func(t *testing.T) {
				tmpDir := t.TempDir()
				started := make(chan struct{}, 2)
				release := make(chan struct{})
				released := false
				defer func() {
					if !released {
						close(release)
					}
				}()

				runner := &concurrentScriptRunner{
					started: started,
					release: release,
				}
				server := &NodeAgentServer{
					Runner:     runner,
					Persister:  &MockPersister{},
					NodeStatus: &NodeStatus{},
				}

				type executionResult struct {
					want     string
					response *pb.ExecuteScriptResponse
					err      error
				}
				results := make(chan executionResult, 2)
				for _, nodeName := range []string{"node-a", "node-b"} {
					nodeName := nodeName
					go func() {
						response, err := server.ExecuteScript(context.Background(), &pb.ExecuteScriptRequest{
							ScriptName: "configure-node",
							ScriptPath: tmpDir,
							ScriptContents: fmt.Sprintf(
								"#!/bin/sh\nNODE_NAME=%s\nprintf '%%s\\n' \"$NODE_NAME\"\n",
								nodeName,
							),
						})
						results <- executionResult{want: nodeName, response: response, err: err}
					}()
				}

				for range 2 {
					select {
					case <-started:
					case <-time.After(5 * time.Second):
						t.Fatal("timed out waiting for concurrent scripts to start")
					}
				}
				close(release)
				released = true

				for range 2 {
					result := <-results
					require.NoError(t, result.err)
					require.NotNil(t, result.response)
					assert.Equal(t, result.want+"\n", result.response.GetOutput())
					assert.Equal(t, SuccessStatus, result.response.GetStatus())
				}

				paths := runner.Paths()
				require.Len(t, paths, 2)
				assert.NotEqual(t, paths[0], paths[1])
				for _, path := range paths {
					assert.NoFileExists(t, path)
				}

				require.Len(t, server.NodeStatus.Scripts, 2)
				for _, script := range server.NodeStatus.Scripts {
					assert.Equal(t, "configure-node", script.Name)
				}
			},
		},
		{
			name: "preserves named on-host script path",
			run: func(t *testing.T) {
				tmpDir := t.TempDir()
				scriptPath := filepath.Join(tmpDir, "existing-script")
				require.NoError(t, os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755))

				resolvedPath, cleanup, err := resolveScript(&pb.ExecuteScriptRequest{
					ScriptName: filepath.Base(scriptPath),
					ScriptPath: tmpDir,
				}, "request-id")
				require.NoError(t, err)
				assert.Equal(t, scriptPath, resolvedPath)

				cleanup()
				assert.FileExists(t, scriptPath)
			},
		},
		{
			name: "records inline script resolution failure in history",
			run: func(t *testing.T) {
				scriptDir := filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(t, os.WriteFile(scriptDir, []byte("contents"), 0o600))

				runner := &MockRunner{}
				server := &NodeAgentServer{
					Runner:     runner,
					Persister:  &MockPersister{},
					NodeStatus: &NodeStatus{},
				}
				response, err := server.ExecuteScript(context.Background(), &pb.ExecuteScriptRequest{
					ScriptName:     "inline-script",
					ScriptPath:     scriptDir,
					ScriptContents: "#!/bin/sh\n",
				})
				require.NoError(t, err)
				require.NotNil(t, response)
				assert.Equal(t, ErrorStatus, response.GetStatus())
				assert.Contains(t, response.GetErrorMessage(), "failed to create script directory")
				assert.Zero(t, runner.RunCalls)

				require.Len(t, server.NodeStatus.Scripts, 1)
				assert.Equal(t, "inline-script", server.NodeStatus.Scripts[0].Name)
				assert.Equal(t, "Error", server.NodeStatus.Scripts[0].Status)
				assert.Equal(t, response.GetErrorMessage(), server.NodeStatus.Scripts[0].Error)
			},
		},
		{
			name: "streams inline contents and removes temporary script",
			run: func(t *testing.T) {
				tmpDir := t.TempDir()
				server := &NodeAgentServer{}
				stream := &mockStreamServer{ctxFunc: context.Background}
				err := server.StreamExecuteScript(&pb.ExecuteScriptRequest{
					ScriptName:     "stream-inline",
					ScriptPath:     tmpDir,
					ScriptContents: "#!/bin/sh\nprintf 'streamed\\n'\n",
				}, stream)
				require.NoError(t, err)

				var output string
				for _, chunk := range stream.chunks {
					output += chunk.GetOutput()
				}
				assert.Contains(t, output, "streamed")

				entries, err := os.ReadDir(tmpDir)
				require.NoError(t, err)
				assert.Empty(t, entries)
			},
		},
		{
			name: "returns internal status for streaming resolution failure",
			run: func(t *testing.T) {
				scriptDir := filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(t, os.WriteFile(scriptDir, []byte("contents"), 0o600))

				server := &NodeAgentServer{}
				stream := &mockStreamServer{ctxFunc: context.Background}
				err := server.StreamExecuteScript(&pb.ExecuteScriptRequest{
					ScriptName:     "inline-script",
					ScriptPath:     scriptDir,
					ScriptContents: "#!/bin/sh\n",
				}, stream)
				require.Error(t, err)
				assert.Equal(t, codes.Internal, status.Code(err))
				assert.Contains(t, err.Error(), "failed to resolve script")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

// TestStreamExecuteScript_ExitCodeRace verifies that StreamExecuteScript
// returns the correct non-zero exit code. The bug: cmd.Wait() is called
// concurrently with pipe reads (violating Go's StdoutPipe contract), so
// Wait can close the pipes before the scanners finish. The resulting
// channel-close events race with doneChan in the select loop, and the
// exit code can be swallowed as 0.
//
// We run multiple iterations with varying output volumes because the race
// is timing-dependent.
func TestStreamExecuteScript_ExitCodeRace(t *testing.T) {
	tmpDir := t.TempDir()

	subtests := []struct {
		name   string
		script string
		iters  int
	}{
		{
			name:   "no output",
			script: "#!/bin/bash\nexit 7\n",
			iters:  30,
		},
		{
			name:   "small output",
			script: "#!/bin/bash\nfor i in $(seq 1 50); do echo \"line $i\"; done\nexit 7\n",
			iters:  30,
		},
		{
			name:   "large output with stderr",
			script: "#!/bin/bash\nfor i in $(seq 1 5000); do echo \"line $i\"; done\necho FINAL >&2\nexit 7\n",
			iters:  20,
		},
	}

	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			for i := range st.iters {
				scriptPath := filepath.Join(tmpDir, fmt.Sprintf("race-%s-%d.sh", st.name, i))
				require.NoError(t, os.WriteFile(scriptPath, []byte(st.script), 0o755))

				server := &NodeAgentServer{
					Runner:                &MockRunner{},
					FileWriter:            &MockFileWriter{},
					Persister:             &MockPersister{},
					NodeStatus:            &NodeStatus{},
					ScriptExecutionLogger: slog.Default(),
				}

				stream := &mockStreamServer{ctxFunc: context.Background}
				err := server.StreamExecuteScript(
					&pb.ExecuteScriptRequest{
						ScriptName: filepath.Base(scriptPath),
						ScriptPath: tmpDir,
					},
					stream,
				)
				require.NoError(t, err, "iteration %d", i)

				// Find the final chunk
				var final *pb.ScriptOutputChunk
				for _, c := range stream.chunks {
					if c.IsComplete {
						final = c
						break
					}
				}
				require.NotNil(t, final, "iteration %d: no final chunk sent", i)
				assert.Equal(t, int32(7), final.ExitCode,
					"iteration %d: exit code was swallowed (got %d, want 7)", i, final.ExitCode)
			}
		})
	}
}

func TestWriteNodeAgentConfig_AtomicReplace(t *testing.T) {
	tmp := t.TempDir()
	srv := &NodeAgentServer{RootPath: tmp}

	body := []byte("labels:\n  role: worker\n")
	resp, err := srv.WriteNodeAgentConfig(context.Background(), &pb.WriteNodeAgentConfigRequest{
		ConfigYaml: body,
	})
	assert.NoError(t, err)
	assert.Equal(t, int32(200), resp.GetCode())

	path := filepath.Join(tmp, "etc", "agent", "config.yaml")
	got, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.Equal(t, body, got)

	next := []byte("kubeadm:\n  version: v1.29.0\n")
	_, err = srv.WriteNodeAgentConfig(context.Background(), &pb.WriteNodeAgentConfigRequest{ConfigYaml: next})
	assert.NoError(t, err)
	got, err = os.ReadFile(path)
	assert.NoError(t, err)
	assert.Equal(t, next, got)
}

func TestWriteNodeAgentConfig_Validation(t *testing.T) {
	srv := &NodeAgentServer{RootPath: t.TempDir()}
	_, err := srv.WriteNodeAgentConfig(context.Background(), &pb.WriteNodeAgentConfigRequest{})
	assert.Error(t, err)
	_, err = srv.WriteNodeAgentConfig(context.Background(), &pb.WriteNodeAgentConfigRequest{
		ConfigYaml: make([]byte, maxNodeAgentConfigBytes+1),
	})
	assert.Error(t, err)
}

func TestDisableKubeletSelfRegistration(t *testing.T) {
	sampleKubeletConfig := `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
registerNode: true
clusterDNS:
- 10.96.0.10
clusterDomain: cluster.local
`

	t.Run("sets registerNode to false", func(t *testing.T) {
		tmp := t.TempDir()
		configDir := filepath.Join(tmp, "var", "lib", "kubelet")
		require.NoError(t, os.MkdirAll(configDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(sampleKubeletConfig), 0o644))

		writer := &MockFileWriter{}
		server := &NodeAgentServer{
			RootPath:   tmp,
			FileWriter: writer,
		}

		err := server.disableKubeletSelfRegistration()
		require.NoError(t, err)

		var written map[string]interface{}
		require.NoError(t, yaml.Unmarshal(writer.WrittenData, &written))
		assert.Equal(t, false, written["registerNode"])
		assert.Equal(t, "kubelet.config.k8s.io/v1beta1", written["apiVersion"])
		assert.Equal(t, "KubeletConfiguration", written["kind"])
	})

	t.Run("idempotent when already false", func(t *testing.T) {
		tmp := t.TempDir()
		configDir := filepath.Join(tmp, "var", "lib", "kubelet")
		require.NoError(t, os.MkdirAll(configDir, 0o755))

		alreadyDisabled := `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
registerNode: false
`
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(alreadyDisabled), 0o644))

		writer := &MockFileWriter{}
		server := &NodeAgentServer{
			RootPath:   tmp,
			FileWriter: writer,
		}

		err := server.disableKubeletSelfRegistration()
		require.NoError(t, err)

		var written map[string]interface{}
		require.NoError(t, yaml.Unmarshal(writer.WrittenData, &written))
		assert.Equal(t, false, written["registerNode"])
	})

	t.Run("adds registerNode when not present", func(t *testing.T) {
		tmp := t.TempDir()
		configDir := filepath.Join(tmp, "var", "lib", "kubelet")
		require.NoError(t, os.MkdirAll(configDir, 0o755))

		noRegisterField := `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
clusterDNS:
- 10.96.0.10
`
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(noRegisterField), 0o644))

		writer := &MockFileWriter{}
		server := &NodeAgentServer{
			RootPath:   tmp,
			FileWriter: writer,
		}

		err := server.disableKubeletSelfRegistration()
		require.NoError(t, err)

		var written map[string]interface{}
		require.NoError(t, yaml.Unmarshal(writer.WrittenData, &written))
		assert.Equal(t, false, written["registerNode"])
	})

	t.Run("returns error when config file missing", func(t *testing.T) {
		server := &NodeAgentServer{
			RootPath:   t.TempDir(),
			FileWriter: &MockFileWriter{},
		}

		err := server.disableKubeletSelfRegistration()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "read kubelet config")
	})

	t.Run("returns error when write fails", func(t *testing.T) {
		tmp := t.TempDir()
		configDir := filepath.Join(tmp, "var", "lib", "kubelet")
		require.NoError(t, os.MkdirAll(configDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(sampleKubeletConfig), 0o644))

		server := &NodeAgentServer{
			RootPath:   tmp,
			FileWriter: &MockFileWriter{Err: errors.New("disk full")},
		}

		err := server.disableKubeletSelfRegistration()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "write kubelet config")
	})

	t.Run("returns error on invalid yaml", func(t *testing.T) {
		tmp := t.TempDir()
		configDir := filepath.Join(tmp, "var", "lib", "kubelet")
		require.NoError(t, os.MkdirAll(configDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("{{invalid yaml"), 0o644))

		server := &NodeAgentServer{
			RootPath:   tmp,
			FileWriter: &MockFileWriter{},
		}

		err := server.disableKubeletSelfRegistration()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parse kubelet config")
	})
}

func TestProcessKubeadmJoin_DisablesRegistration(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "var", "lib", "kubelet")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(configDir, "config.yaml"),
		[]byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\n"),
		0o644,
	))

	writer := &MockFileWriter{}
	server := &NodeAgentServer{
		Runner:     &MockRunner{Output: []byte("joined"), Err: nil},
		FileWriter: writer,
		Persister:  &MockPersister{},
		RootPath:   tmp,
		NodeStatus: &NodeStatus{
			Status: PendingStatus,
			Phase:  JoinPhase,
		},
	}

	server.processKubeadmJoin(
		[]string{"join", "1.2.3.4:6443", "--token", "abcdef"},
		&pb.KubeadmJoinRequest{KubeadmJoinString: "kubeadm join 1.2.3.4:6443 --token abcdef"},
	)

	assert.Equal(t, ReadyStatus, server.NodeStatus.Status)
	require.NotEmpty(t, writer.WrittenData, "expected kubelet config to be written")

	var written map[string]interface{}
	require.NoError(t, yaml.Unmarshal(writer.WrittenData, &written))
	assert.Equal(t, false, written["registerNode"])
}

func TestProcessKubeadmInit_DisablesRegistration(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "var", "lib", "kubelet")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(configDir, "config.yaml"),
		[]byte("apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\n"),
		0o644,
	))

	writer := &MockFileWriter{}
	server := &NodeAgentServer{
		Runner:     &MockRunner{Output: []byte(initOutput), Err: nil},
		FileWriter: writer,
		Persister:  &MockPersister{},
		RootPath:   tmp,
		NodeStatus: &NodeStatus{
			Status: NotReadyStatus,
			Phase:  "",
		},
	}

	server.processKubeadmInit(
		[]string{"init", "--pod-network-cidr=10.244.0.0/16", "--control-plane-endpoint=192.168.0.1:6443"},
		&pb.KubeadmInitRequest{
			PodNetworkCidr:       "10.244.0.0/16",
			ControlPlaneEndpoint: "192.168.0.1:6443",
			NoCniInstall:         true,
		},
	)

	assert.Equal(t, ReadyStatus, server.NodeStatus.Status)
	require.NotEmpty(t, writer.WrittenData, "expected kubelet config to be written")

	var written map[string]interface{}
	require.NoError(t, yaml.Unmarshal(writer.WrittenData, &written))
	assert.Equal(t, false, written["registerNode"])
}

func TestSetIndexURLOptionalBundleServer(t *testing.T) {
	t.Run("empty bundle server and empty index leaves index unset", func(t *testing.T) {
		server := &NodeAgentServer{}
		err := server.setIndexURL("")
		assert.NoError(t, err)
		assert.Empty(t, server.BundleIndexFile)
	})

	t.Run("explicit index url is kept without bundle server", func(t *testing.T) {
		server := &NodeAgentServer{}
		err := server.setIndexURL("https://example.com/custom_index.json")
		assert.NoError(t, err)
		assert.Equal(t, "https://example.com/custom_index.json", server.BundleIndexFile)
	})
}

func TestInstallBundleRequiresIndex(t *testing.T) {
	server := &NodeAgentServer{
		NodeStatus: &NodeStatus{Architecture: "amd64"},
	}

	resp, err := server.InstallBundle(context.Background(), &pb.InstallBundleRequest{
		Name:        "kubeadm",
		Version:     "1.30.0",
		InstallArgs: []string{"./install.sh"},
	})

	assert.Nil(t, resp)
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "bundle index not configured")
}

func TestInstallCollectionJSONRequiresIndex(t *testing.T) {
	server := &NodeAgentServer{
		NodeStatus: &NodeStatus{Architecture: "amd64"},
	}

	resp, err := server.InstallCollection(context.Background(), &pb.InstallCollectionRequest{
		CollectionURL: "https://example.com/collection.json",
	})

	assert.Nil(t, resp)
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "bundle index not configured")
}

func TestExecuteKubeadmInitRefusesWithoutBundleSource(t *testing.T) {
	newServer := func(bundleServerURL, bundleIndexFile string) *NodeAgentServer {
		return &NodeAgentServer{
			Runner:          &MockRunner{Output: []byte(initOutput)},
			FileWriter:      &MockFileWriter{},
			Persister:       &MockPersister{},
			NodeStatus:      &NodeStatus{Status: NotReadyStatus},
			BundleServerURL: bundleServerURL,
			BundleIndexFile: bundleIndexFile,
		}
	}
	validReq := func() *pb.KubeadmInitRequest {
		return &pb.KubeadmInitRequest{
			PodNetworkCidr:       "10.244.0.0/16",
			ControlPlaneEndpoint: "192.168.0.1:6443",
		}
	}

	// A gRPC error, not an ErrorStatus response: the caller discards the
	// response, so a response would be invisible.
	t.Run("no bundle source at all names both flags", func(t *testing.T) {
		_, err := newServer("", "").ExecuteKubeadmInit(context.Background(), validReq())
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, err.Error(), "--bundle-server-url")
		assert.Contains(t, err.Error(), "--index-file-url")
	})

	t.Run("a bundle server whose index did not derive says so", func(t *testing.T) {
		_, err := newServer("https://example.test/", "").ExecuteKubeadmInit(context.Background(), validReq())
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, err.Error(), "/etc/os-release",
			"telling an operator to set a flag they already set sends them in a circle")
		assert.Contains(t, err.Error(), "--index-file-url")
	})

	t.Run("no_cni_install does not need a bundle source", func(t *testing.T) {
		req := validReq()
		req.NoCniInstall = true
		resp, err := newServer("", "").ExecuteKubeadmInit(context.Background(), req)
		assert.NoError(t, err)
		assert.Equal(t, PendingStatus, resp.Status)
	})

	t.Run("a configured index proceeds", func(t *testing.T) {
		resp, err := newServer("https://example.test/", "https://example.test/index.json").
			ExecuteKubeadmInit(context.Background(), validReq())
		assert.NoError(t, err)
		assert.Equal(t, PendingStatus, resp.Status)
	})

	t.Run("argument validation still takes precedence", func(t *testing.T) {
		req := validReq()
		req.PodNetworkCidr = ""
		resp, err := newServer("", "").ExecuteKubeadmInit(context.Background(), req)
		assert.NoError(t, err)
		assert.Equal(t, ErrorStatus, resp.Status)
		assert.Contains(t, resp.ErrorMessage, "PodNetworkCidr")
	})
}

// The status RPC returns CurrentMessage and nothing else about a failure, so a
// failed init that leaves it at "Node is initializing" reports the failure as
// progress. The orchestrator then prints "kubeadm init failed: Node is
// initializing", which names neither the phase nor the reason.
func TestKubeadmFailureMessage(t *testing.T) {
	realOutput := `[init] Using Kubernetes version: v1.34.2
[preflight] Running pre-flight checks
[kubelet-start] Starting the kubelet
error: error execution phase wait-control-plane: cannot obtain client without bootstrap: could not bootstrap the admin user in file admin.conf
To see the stack trace of this error execute with --v=5 or higher
`
	tests := []struct {
		name   string
		err    error
		output string
		want   string
	}{
		{
			name:   "names the phase kubeadm failed in",
			err:    errors.New("exit status 1"),
			output: realOutput,
			want:   "kubeadm init failed: error: error execution phase wait-control-plane: cannot obtain client without bootstrap: could not bootstrap the admin user in file admin.conf",
		},
		{
			name:   "falls back to the process error when kubeadm said nothing",
			err:    errors.New("exit status 1"),
			output: "[init] Using Kubernetes version: v1.34.2\n",
			want:   "kubeadm init failed: exit status 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := kubeadmFailureMessage(tc.err, []byte(tc.output), nil)
			if got != tc.want {
				t.Errorf("kubeadmFailureMessage() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "Node is initializing") {
				t.Error("a failure must not read as progress")
			}
		})
	}

	t.Run("bounded", func(t *testing.T) {
		long := "error: " + strings.Repeat("x", 1000)
		if got := kubeadmFailureMessage(errors.New("boom"), []byte(long), nil); len(got) > 350 {
			t.Errorf("message is %d bytes; status fields should stay bounded", len(got))
		}
	})
}

func TestUpdateAndFetchKubeAdmJoinStrings(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const joinCmd = "kubeadm join 172.31.255.200:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:deadbeef"
	uploaded := "[upload-certs] Storing the certificates in Secret \"kubeadm-certs\" in the \"kube-system\" Namespace\n" +
		"[upload-certs] Using certificate key:\n" + key + "\n"
	// What kubeadm 1.36 prints when the phase runs without --config on a node
	// whose default-route address is not in the apiserver certificate.
	refused := "I0915 02:06:22.985721   10297 version.go:260] remote version is much newer: v1.37.0; falling back to: stable-1.36\n" +
		"error: error execution phase upload-certs: could not bootstrap the admin user in file admin.conf: " +
		"tls: failed to verify certificate: x509: certificate is valid for 172.31.1.11, not 192.168.200.3\n"

	t.Log("Doing: pointing the agent at an init config on disk, as ExecuteKubeadmInit leaves it")
	configPath := filepath.Join(t.TempDir(), "kubeadm-init-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("apiVersion: kubeadm.k8s.io/v1beta4\nkind: InitConfiguration\n"), 0o600))
	uploadArgs := []string{kubeadmCmdInit, kubeadmCmdPhase, "upload-certs", "--upload-certs", "--config", configPath}
	tokenArgs := []string{"token", "create", "--print-join-command"}
	reply := func(out string, err error) runFunc {
		return func(string, []string, *nvkeutils.CommandOptions) ([]byte, error) { return []byte(out), err }
	}

	tests := []struct {
		name    string
		runners []runner
		wantErr string
	}{
		{
			name: "passes the init config to upload-certs and returns the printed key",
			runners: []runner{
				runForCommand("kubeadm", uploadArgs, reply(uploaded, nil)),
				runForCommand("kubeadm", tokenArgs, reply(joinCmd+"\n", nil)),
			},
		},
		{
			name:    "a failed upload-certs is an error, never a join string",
			runners: []runner{runForCommand("kubeadm", uploadArgs, reply(refused, errors.New("exit status 1")))},
			wantErr: "exit status 1",
		},
		{
			name:    "output without a certificate key is an error",
			runners: []runner{runForCommand("kubeadm", uploadArgs, reply(refused, nil))},
			wantErr: "no certificate key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &NodeAgentServer{
				Runner:         &MockRunner{Runners: tt.runners, Err: errors.New("unexpected command")},
				FileWriter:     &MockFileWriter{},
				Persister:      &MockPersister{},
				NodeStatus:     &NodeStatus{Role: ControlPlaneRole},
				initConfigPath: configPath,
			}

			t.Log("Doing: asking for fresh join strings")
			resp, err := server.UpdateAndFetchKubeAdmJoinStrings(context.Background(), &pb.KubeadmJoinStringRequest{})

			if tt.wantErr != "" {
				t.Log("Testing: the RPC fails and no join string is recorded")
				require.ErrorContains(t, err, tt.wantErr)
				require.Nil(t, resp)
				require.Empty(t, server.NodeStatus.JoinInfo.ControlPlaneJoinString)
				return
			}

			t.Log("Testing: the control-plane join string carries the real key and the worker string the plain command")
			require.NoError(t, err)
			require.Equal(t, joinCmd+" --control-plane --certificate-key "+key, resp.GetControlplaneJoinString())
			require.Equal(t, joinCmd, resp.GetWorkerJoinString())
			require.Equal(t, resp.GetControlplaneJoinString(), server.NodeStatus.JoinInfo.ControlPlaneJoinString)
		})
	}
}

func TestExecuteKubeadmInitWithoutConfigRemovesStaleInitConfig(t *testing.T) {
	tests := []struct {
		name      string
		staleFile bool
	}{
		{name: "a config left by an earlier custom init is removed", staleFile: true},
		{name: "no config on disk is not an error", staleFile: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log("Doing: pointing the agent at a config path that may hold a stale file")
			configPath := filepath.Join(t.TempDir(), "kubeadm-init-config.yaml")
			if tt.staleFile {
				require.NoError(t, os.WriteFile(configPath, []byte("kind: InitConfiguration\n"), 0o600))
			}

			server := &NodeAgentServer{
				Runner:         &MockRunner{Output: []byte(initOutput)},
				FileWriter:     &MockFileWriter{},
				Persister:      &MockPersister{},
				NodeStatus:     &NodeStatus{Status: NotReadyStatus},
				initConfigPath: configPath,
			}

			t.Log("Doing: running an argument-based init")
			server.processKubeadmInit([]string{kubeadmCmdInit, "--upload-certs"}, &pb.KubeadmInitRequest{
				PodNetworkCidr:       "10.244.0.0/16",
				ControlPlaneEndpoint: "192.168.0.1:6443",
				NoCniInstall:         true,
			})

			t.Log("Testing: no config is on disk for upload-certs to pick up, and the init succeeded")
			_, err := os.Stat(configPath)
			require.ErrorIs(t, err, fs.ErrNotExist)
			require.Equal(t, ReadyStatus, server.NodeStatus.Status)
		})
	}
}

func TestHardenKubeletFileModes(t *testing.T) {
	// writeAt creates rel under root with mode, making parents as needed.
	writeAt := func(t *testing.T, root, rel string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("x"), mode))
		// WriteFile is subject to umask, so set the mode explicitly.
		require.NoError(t, os.Chmod(path, mode))
		return path
	}

	const (
		etcDropIn    = "/etc/systemd/system/kubelet.service.d/10-kubeadm.conf"
		libUnit      = "/lib/systemd/system/kubelet.service"
		libDropIn    = "/lib/systemd/system/kubelet.service.d/10-kubeadm.conf"
		usrLibUnit   = "/usr/lib/systemd/system/kubelet.service"
		usrLibDropIn = "/usr/lib/systemd/system/kubelet.service.d/10-kubeadm.conf"
	)

	tests := []struct {
		name  string
		files map[string]os.FileMode
	}{
		{
			name:  "tightens the vendor unit kube-bench checks on a kubeadm-distro node",
			files: map[string]os.FileMode{libUnit: 0o644, libDropIn: 0o644, kubeletConfigPath: 0o644},
		},
		{
			name:  "tightens the vendor unit on an upstream pkgs.k8s.io layout under /usr/lib",
			files: map[string]os.FileMode{usrLibUnit: 0o644, usrLibDropIn: 0o644, kubeletConfigPath: 0o644},
		},
		{
			name:  "tightens a drop-in under /etc when a node carries one",
			files: map[string]os.FileMode{etcDropIn: 0o644, libUnit: 0o644, kubeletConfigPath: 0o644},
		},
		{
			name:  "idempotent when everything is already 0600",
			files: map[string]os.FileMode{libUnit: 0o600, libDropIn: 0o600, kubeletConfigPath: 0o600},
		},
		{
			name:  "tightens the config alone when no unit is present",
			files: map[string]os.FileMode{kubeletConfigPath: 0o644},
		},
		{name: "no files yet, before init or join, is not an error"},
		{
			name:  "tightens a mode looser than 0644",
			files: map[string]os.FileMode{libUnit: 0o666, kubeletConfigPath: 0o640},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			paths := make([]string, 0, len(tt.files))
			for rel, mode := range tt.files {
				paths = append(paths, writeAt(t, tmp, rel, mode))
			}

			server := &NodeAgentServer{RootPath: tmp}

			require.NoError(t, server.hardenKubeletFileModes())

			for _, p := range paths {
				info, err := os.Stat(p)
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(kubeletHardenedFileMode), info.Mode().Perm(), "mode of %s", p)
			}

			// Re-running must not change anything or fail: the startup call
			// site invokes this on every agent restart.
			require.NoError(t, server.hardenKubeletFileModes())
			for _, p := range paths {
				info, err := os.Stat(p)
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(kubeletHardenedFileMode), info.Mode().Perm(), "mode of %s after re-run", p)
			}
		})
	}

	// Every path kube-bench can resolve "the kubelet service file" to must be
	// one this hardens, or CIS 4.1.1 can land on a file nothing tightens.
	t.Run("covers every kube-bench candidate for the kubelet service file", func(t *testing.T) {
		for _, candidate := range []string{
			"/etc/systemd/system/kubelet.service.d/10-kubeadm.conf",
			"/etc/systemd/system/kubelet.service",
			"/lib/systemd/system/kubelet.service",
		} {
			assert.Contains(t, kubeletServiceFilePaths, candidate)
		}
	})

	t.Run("reports the error when a file cannot be chmodded", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses the directory permissions this case relies on")
		}
		tmp := t.TempDir()
		writeAt(t, tmp, kubeletConfigPath, 0o644)

		// Make the parent directory non-writable and non-executable so the
		// chmod inside it fails.
		dir := filepath.Dir(filepath.Join(tmp, kubeletConfigPath))
		require.NoError(t, os.Chmod(dir, 0o000))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

		err := (&NodeAgentServer{RootPath: tmp}).hardenKubeletFileModes()
		require.Error(t, err)
	})
}

// TestHardenKubeletFileModesAfterConfigRewrite pins the ordering requirement
// between disableKubeletSelfRegistration and hardenKubeletFileModes.
//
// disableKubeletSelfRegistration rewrites /var/lib/kubelet/config.yaml with
// mode 0644. If hardening ran before it, the rewrite would immediately undo the
// mode and the node would sit at 0644 with nothing reporting it. This asserts
// the recovery direction: whatever leaves the file 0644, a subsequent harden
// brings it back to 0600.
func TestHardenKubeletFileModesAfterConfigRewrite(t *testing.T) {
	const kubeletConfigYAML = `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
registerNode: false
`

	tmp := t.TempDir()
	configPath := filepath.Join(tmp, kubeletConfigPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte(kubeletConfigYAML), 0o600))
	require.NoError(t, os.Chmod(configPath, 0o600))

	server := &NodeAgentServer{RootPath: tmp}

	// Stand in for the 0644 rewrite disableKubeletSelfRegistration performs.
	require.NoError(t, os.WriteFile(configPath, []byte(kubeletConfigYAML), 0o644))
	require.NoError(t, os.Chmod(configPath, 0o644))

	info, err := os.Stat(configPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "precondition: the rewrite loosened the mode")

	require.NoError(t, server.hardenKubeletFileModes())

	info, err = os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(kubeletHardenedFileMode), info.Mode().Perm(),
		"hardening must run after the config rewrite, not before")
}
