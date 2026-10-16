// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	yaml "gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/agent/internal/k8s"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

var (
	testKey1 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x00}, 32))
	testKey2 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, 32))
	testKey3 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x02}, 32))
)

// ── mock runner ───────────────────────────────────────────────────────────────

// MockK8sEncryptionRunner is a configurable mock for encryption RPC tests.
type MockK8sEncryptionRunner struct {
	mu           sync.Mutex
	Outputs      map[string][]byte
	Errors       map[string]error
	CalledWith   [][]string
	DefaultError error
}

func NewMockK8sEncryptionRunner() *MockK8sEncryptionRunner {
	return &MockK8sEncryptionRunner{Outputs: make(map[string][]byte), Errors: make(map[string]error)}
}

func (m *MockK8sEncryptionRunner) Run(name string, args []string, opts *nvkeutils.CommandOptions) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CalledWith = append(m.CalledWith, append([]string{name}, args...))
	key := name
	if len(args) > 0 {
		key = name + " " + args[0]
	}
	if output, ok := m.Outputs[key]; ok {
		return output, m.Errors[key]
	}
	if output, ok := m.Outputs[name]; ok {
		return output, m.Errors[name]
	}
	return nil, m.DefaultError
}

func (m *MockK8sEncryptionRunner) RunDetached(name string, args []string, opts *nvkeutils.CommandOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CalledWith = append(m.CalledWith, append([]string{name}, args...))
	return m.DefaultError
}

func (m *MockK8sEncryptionRunner) RunSplitOutput(name string, args []string, opts *nvkeutils.CommandOptions) (nvkeutils.CommandOutput, error) {
	output, err := m.Run(name, args, opts)
	return nvkeutils.CommandOutput{Stdout: output}, err
}

// metricsSequencer returns successive /metrics responses; once exhausted it
// repeats the last entry.
type metricsSequencer struct {
	mu      sync.Mutex
	values  [][]byte
	callIdx int
	err     error
	onCall  func(idx int) // optional hook (e.g. for WAL ordering checks)
}

func (s *metricsSequencer) fetch() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.callIdx
	if idx >= len(s.values) {
		idx = len(s.values) - 1
	}
	s.callIdx++
	if s.onCall != nil {
		s.onCall(s.callIdx - 1)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.values[idx], nil
}

// ── test helpers ──────────────────────────────────────────────────────────────

func metricsOutput(count int) []byte {
	return []byte(fmt.Sprintf(
		`apiserver_encryption_config_controller_automatic_reloads_total{status="success"} %d`, count,
	))
}

// metricsWithReload: Phase 2 already-confirmed check sees R > baseline → fast exit.
func metricsWithReload(count int) *metricsSequencer {
	return &metricsSequencer{values: [][]byte{metricsOutput(count), metricsOutput(count + 1)}}
}

// metricsWithBlockingWaitPath: Phase 2 sees R == N → Phase 3 runs; first poll confirms.
func metricsWithBlockingWaitPath(count int) *metricsSequencer {
	return &metricsSequencer{values: [][]byte{metricsOutput(count), metricsOutput(count), metricsOutput(count + 1)}}
}

func metricsWithCount(count int) *metricsSequencer {
	return &metricsSequencer{values: [][]byte{metricsOutput(count)}}
}

func metricsWithError(errMsg string) *metricsSequencer {
	return &metricsSequencer{values: [][]byte{metricsOutput(0)}, err: errors.New(errMsg)}
}

type fakeLocalClientRegistry struct {
	localClient *kubernetes.Clientset
	// client, when non-nil, is returned from Client() instead of nil. Used
	// by apiserver_readiness_test.go to fake the kubelet mirror-pod read in
	// waitForAPIServerReadyAfterMutation. Existing tests that leave it nil
	// keep the historical "Client() returns nil" behavior.
	client           kubernetes.Interface
	err              error
	localClientCalls int
}

func (r *fakeLocalClientRegistry) Client() (kubernetes.Interface, error) {
	return r.client, r.err
}

func (r *fakeLocalClientRegistry) DynamicClient() (dynamic.Interface, error) {
	return nil, r.err
}

func (r *fakeLocalClientRegistry) LocalClient() (*kubernetes.Clientset, error) {
	r.localClientCalls++
	return r.localClient, r.err
}

type recordingPersister struct {
	mu        sync.Mutex
	err       error
	snapshots []NodeStatus
}

func (p *recordingPersister) Persist(status *NodeStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.snapshots = append(p.snapshots, *status)
	return nil
}

func (p *recordingPersister) LoadFromStateFile() (*NodeStatus, error) {
	return &NodeStatus{}, nil
}

func (p *recordingPersister) Snapshots() []NodeStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]NodeStatus(nil), p.snapshots...)
}

func writeEncryptionConfigFixture(t *testing.T, dir string, keys []KeyConfig) {
	t.Helper()
	encDir := filepath.Join(dir, "etc/kubernetes/kube-api-encryption")
	require.NoError(t, os.MkdirAll(encDir, 0o755))
	cfg := EncryptionConfiguration{
		ApiVersion: "apiserver.config.k8s.io/v1", Kind: "EncryptionConfiguration",
		Resources: []ResourceConfig{{
			Resources: []string{"secrets"},
			Providers: []ProviderConfig{
				{Secretbox: &SecretboxConfig{Keys: keys}},
				{Identity: &IdentityConfig{}},
			},
		}},
	}
	b, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(encDir, "encryption-config.yaml"), b, 0o644))
}

func newEncryptionServer(t *testing.T, tmpDir, role string, runner nvkeutils.CommandRunner, metrics *metricsSequencer) *NodeAgentServer {
	t.Helper()
	return &NodeAgentServer{
		Runner: runner, Persister: &MockPersister{},
		NodeStatus: &NodeStatus{Role: role}, RootPath: tmpDir,
		reloadPollInterval: 10 * time.Millisecond,
		clientRegistry:     newMetricsRegistry(t, metrics),
	}
}

func newMetricsRegistry(t *testing.T, metrics *metricsSequencer) *fakeLocalClientRegistry {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics", r.URL.Path)
		raw, err := metrics.fetch()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	return &fakeLocalClientRegistry{localClient: newTestClientset(t, server.URL)}
}

func firstSecretboxKeys(t *testing.T, cfg *EncryptionConfiguration) []KeyConfig {
	t.Helper()
	require.NoError(t, cfg.ValidateSecretbox())
	slices := cfg.secretboxKeySlices()
	return *slices[0]
}

func writeWALFixture(t *testing.T, srv *NodeAgentServer, reloadCountBefore int, req *pb.UpdateEncryptionConfigRequest) {
	t.Helper()
	opID, err := encRotationOpID(req)
	require.NoError(t, err)
	require.NoError(t, writeEncRotationWAL(srv.encRotationWALPath(), encRotationWALEntry{
		ReloadCountBefore: reloadCountBefore,
		OpID:              opID,
	}))
}

func walFileExists(srv *NodeAgentServer) bool {
	_, err := os.Stat(srv.encRotationWALPath())
	return err == nil
}

func setupRewriteK8sClients(t *testing.T, dynamicObjects ...runtime.Object) kubernetes.Interface {
	t.Helper()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cp-0",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
	}
	clientset := kubernetesfake.NewSimpleClientset(node)
	k8s.SetClientsetForTest(clientset)
	t.Cleanup(func() { k8s.SetClientsetForTest(nil) })

	dynamicClient := dynamicfake.NewSimpleDynamicClient(clientscheme.Scheme, dynamicObjects...)
	k8s.SetDynamicClientForTest(dynamicClient)
	t.Cleanup(func() { k8s.SetDynamicClientForTest(nil) })
	return clientset
}

func TestRemovePlainTextSecondaryEncryptionEntry_WaitsForReloadMetricWithoutAPIServerRestart(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err)

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: hostname,
			Labels: map[string]string{
				"nke.io/k8s-api-encryption-configured":       "true",
				"nke.io/k8s-api-encryption-rewrite-complete": "true",
			},
		},
	}
	clientset := kubernetesfake.NewSimpleClientset(node)
	k8s.SetClientsetForTest(clientset)
	t.Cleanup(func() { k8s.SetClientsetForTest(nil) })

	runner := NewMockK8sEncryptionRunner()
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, runner, metricsWithReload(0))
	srv.NodeStatus.NodeName = hostname

	_, err = srv.RemovePlainTextSecondaryEncryptionEntry(context.Background(), &pb.RemovePlainTextSecondaryEncryptionEntryRequest{})
	require.NoError(t, err)

	for _, call := range runner.CalledWith {
		require.NotEmpty(t, call)
		assert.NotContains(t, strings.Join(call, " "), "delete pod kube-apiserver-", "remove identity should wait for config reload metric, not kube-apiserver pod restart")
	}
	got, err := clientset.CoreV1().Nodes().Get(context.Background(), hostname, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", got.Labels["nvke.nvidia.com/k8s-api-encryption-enabled"])
	assert.NotContains(t, got.Labels, "nvke.nvidia.com/k8s-api-encryption-configured")
	assert.NotContains(t, got.Labels, "nvke.nvidia.com/k8s-api-encryption-rewrite-complete")
}

func rewriteStateSnapshot(srv *NodeAgentServer) (K8sAPIRewriteOperation, bool) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.NodeStatus.K8sAPIRewrite, srv.rewriteActive
}

func TestStartEtcdEncryptionAtRestObjectsRewriteReturnsExistingActiveOperation(t *testing.T) {
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, NewMockK8sEncryptionRunner(), nil)
	srv.NodeStatus.K8sAPIRewrite = K8sAPIRewriteOperation{
		ID:    "op-1",
		State: K8sAPIRewriteStateRunning,
	}
	srv.rewriteActive = true

	resp, err := srv.StartEtcdEncryptionAtRestObjectsRewrite(context.Background(), &pb.StartEtcdEncryptionAtRestObjectsRewriteRequest{
		OperationId: "op-1",
	})

	require.NoError(t, err)
	require.NotNil(t, resp.GetOperation())
	assert.Equal(t, "op-1", resp.GetOperation().GetOperationId())
	assert.Equal(t, pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_RUNNING, resp.GetOperation().GetState())
}

func TestStartEtcdEncryptionAtRestObjectsRewriteRejectsDifferentRunningOperation(t *testing.T) {
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, NewMockK8sEncryptionRunner(), nil)
	srv.NodeStatus.K8sAPIRewrite = K8sAPIRewriteOperation{
		ID:    "op-1",
		State: K8sAPIRewriteStateRunning,
	}

	_, err := srv.StartEtcdEncryptionAtRestObjectsRewrite(context.Background(), &pb.StartEtcdEncryptionAtRestObjectsRewriteRequest{
		OperationId: "op-2",
	})

	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestStartEtcdEncryptionAtRestObjectsRewriteFailsWhenInitialPersistFails(t *testing.T) {
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, NewMockK8sEncryptionRunner(), nil)
	persister := &recordingPersister{err: errors.New("persist failed")}
	srv.Persister = persister

	resp, err := srv.StartEtcdEncryptionAtRestObjectsRewrite(context.Background(), &pb.StartEtcdEncryptionAtRestObjectsRewriteRequest{
		OperationId: "op-1",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "persist rewrite operation")
	assert.Empty(t, persister.Snapshots())

	rewriteOp, rewriteActive := rewriteStateSnapshot(srv)
	assert.False(t, rewriteActive)
	assert.Empty(t, rewriteOp.ID)
}

func TestRunAsyncRewriteEncryptedObjectsPersistsSucceededOperation(t *testing.T) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns1"},
		StringData: map[string]string{"k": "v"},
	}
	clientset := setupRewriteK8sClients(t, secret)
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, NewMockK8sEncryptionRunner(), nil)
	persister := &recordingPersister{}
	srv.Persister = persister
	srv.NodeStatus.K8sAPIRewrite = newRewriteOperation("op-1")
	srv.rewriteActive = true

	srv.runAsyncRewriteEncryptedObjects("op-1", []string{"secrets"})

	rewriteOp, rewriteActive := rewriteStateSnapshot(srv)
	assert.False(t, rewriteActive)
	assert.Equal(t, K8sAPIRewriteStateSucceeded, rewriteOp.State)
	assert.Empty(t, rewriteOp.ErrorMessage)

	snapshots := persister.Snapshots()
	require.Len(t, snapshots, 1)
	assert.Equal(t, K8sAPIRewriteStateSucceeded, snapshots[0].K8sAPIRewrite.State)

	node, err := clientset.CoreV1().Nodes().Get(context.Background(), "cp-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", node.Labels["nvke.nvidia.com/k8s-api-encryption-rewrite-complete"])
}

func TestRunAsyncRewriteEncryptedObjectsPersistsFailedOperation(t *testing.T) {
	setupRewriteK8sClients(t)
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, NewMockK8sEncryptionRunner(), nil)
	persister := &recordingPersister{}
	srv.Persister = persister
	srv.NodeStatus.K8sAPIRewrite = newRewriteOperation("op-1")
	srv.rewriteActive = true

	srv.runAsyncRewriteEncryptedObjects("op-1", []string{"not-a-plural"})

	rewriteOp, rewriteActive := rewriteStateSnapshot(srv)
	assert.False(t, rewriteActive)
	assert.Equal(t, K8sAPIRewriteStateFailed, rewriteOp.State)
	assert.Contains(t, rewriteOp.ErrorMessage, "unknown encryptable resource")

	snapshots := persister.Snapshots()
	require.Len(t, snapshots, 1)
	assert.Equal(t, K8sAPIRewriteStateFailed, snapshots[0].K8sAPIRewrite.State)
	assert.Contains(t, snapshots[0].K8sAPIRewrite.ErrorMessage, "unknown encryptable resource")
}

func TestDecideStartObjectRewrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		rewriteOp     K8sAPIRewriteOperation
		operationID   string
		rewriteActive bool
		want          rewriteDecision
	}{
		{
			name:        "no known operation starts new work",
			operationID: "op-1",
			want:        start,
		},
		{
			name:          "active same operation returns existing operation",
			rewriteOp:     K8sAPIRewriteOperation{ID: "op-1", State: K8sAPIRewriteStateRunning},
			operationID:   "op-1",
			rewriteActive: true,
			want:          noop,
		},
		{
			name:          "active different operation rejects concurrent work",
			rewriteOp:     K8sAPIRewriteOperation{ID: "op-1", State: K8sAPIRewriteStateRunning},
			operationID:   "op-2",
			rewriteActive: true,
			want:          reject,
		},
		{
			name:        "terminal same operation returns existing operation",
			rewriteOp:   K8sAPIRewriteOperation{ID: "op-1", State: K8sAPIRewriteStateSucceeded},
			operationID: "op-1",
			want:        noop,
		},
		{
			name:        "inactive nonterminal same operation restarts after agent restart",
			rewriteOp:   K8sAPIRewriteOperation{ID: "op-1", State: K8sAPIRewriteStateRunning},
			operationID: "op-1",
			want:        start,
		},
		{
			name:        "inactive nonterminal different operation rejects",
			rewriteOp:   K8sAPIRewriteOperation{ID: "op-1", State: K8sAPIRewriteStateRunning},
			operationID: "op-2",
			want:        reject,
		},
		{
			name:        "terminal different operation starts new work",
			rewriteOp:   K8sAPIRewriteOperation{ID: "op-1", State: K8sAPIRewriteStateFailed},
			operationID: "op-2",
			want:        start,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := decideStartObjectRewrite(tt.rewriteOp, tt.operationID, tt.rewriteActive)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetEtcdEncryptionAtRestObjectsRewriteStatus(t *testing.T) {
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, NewMockK8sEncryptionRunner(), nil)
	srv.NodeStatus.K8sAPIRewrite = K8sAPIRewriteOperation{
		ID:    "op-1",
		State: K8sAPIRewriteStateSucceeded,
	}

	resp, err := srv.GetEtcdEncryptionAtRestObjectsRewriteStatus(context.Background(), &pb.GetEtcdEncryptionAtRestObjectsRewriteStatusRequest{
		OperationId: "op-1",
	})

	require.NoError(t, err)
	require.NotNil(t, resp.GetOperation())
	assert.Equal(t, pb.EtcdEncryptionAtRestObjectsRewriteState_ETCD_ENCRYPTION_AT_REST_OBJECTS_REWRITE_STATE_SUCCEEDED, resp.GetOperation().GetState())
}

func TestGetEtcdEncryptionAtRestObjectsRewriteStatusNotFound(t *testing.T) {
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, NewMockK8sEncryptionRunner(), nil)
	srv.NodeStatus.K8sAPIRewrite = K8sAPIRewriteOperation{
		ID:    "op-1",
		State: K8sAPIRewriteStateRunning,
	}

	_, err := srv.GetEtcdEncryptionAtRestObjectsRewriteStatus(context.Background(), &pb.GetEtcdEncryptionAtRestObjectsRewriteStatusRequest{
		OperationId: "op-2",
	})

	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// ── unit tests: pure functions ────────────────────────────────────────────────

func TestParseEncryptionReloadCount(t *testing.T) {
	sLine := `apiserver_encryption_config_controller_automatic_reloads_total{status="success"} `
	oLine := `apiserver_encryption_config_controller_automatic_reload_success_total{apiserver_id_hash="x"} `
	for _, tc := range []struct {
		name  string
		input string
		want  int
	}{
		{"empty", "", 0},
		{"no match", "some_other_metric 5\n", 0},
		{"new format", sLine + "3\n", 3},
		{"old format", oLine + "7\n", 7},
		{"both summed", sLine + "2\n" + oLine + "5\n", 7},
		{"ignores comments", "# HELP foo\n" + sLine + "4\n", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseEncryptionReloadCount(tc.input))
		})
	}
}

func TestValidateEncryptionKey(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x00}, 16))
	for _, tc := range []struct{ key, wantErr string }{
		{"", "must not be empty"},
		{"not-valid-base64!!!", "base64"},
		{short, "32 bytes"},
		{testKey1, ""},
	} {
		err := validateEncryptionKey(tc.key)
		if tc.wantErr == "" {
			assert.NoError(t, err)
		} else {
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		}
	}
}

func TestReadEncryptionConfig_missingFileIsFailedPrecondition(t *testing.T) {
	srv := newEncryptionServer(t, t.TempDir(), ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithCount(0))
	_, err := srv.readEncryptionConfig()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error")
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "InitializeKubernetesAPIEncryption")
}

func TestNextKeyName(t *testing.T) {
	for _, tc := range []struct {
		keys []KeyConfig
		want string
	}{
		{nil, "kube-api-encryption-key-1"},
		{[]KeyConfig{{Name: "kube-api-encryption-key-1"}, {Name: "kube-api-encryption-key-2"}}, "kube-api-encryption-key-3"},
		{[]KeyConfig{{Name: "kube-api-encryption-key-1"}, {Name: "kube-api-encryption-key-3"}}, "kube-api-encryption-key-4"},
		{[]KeyConfig{{Name: "custom-name"}}, "kube-api-encryption-key-1"},
	} {
		assert.Equal(t, tc.want, nextKeyName(tc.keys))
	}
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.yaml")
	data := []byte("hello: world\n")
	require.NoError(t, atomicWriteFile(path, data, 0o600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temp files left behind")
}

func TestFetchApiserverMetricsFromRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics", r.URL.Path)
		_, _ = w.Write([]byte("apiserver_metric 1\n"))
	}))
	t.Cleanup(server.Close)
	registry := &fakeLocalClientRegistry{localClient: newTestClientset(t, server.URL)}

	raw, err := fetchApiserverMetricsFromRegistry(registry)

	require.NoError(t, err)
	assert.Equal(t, "apiserver_metric 1\n", string(raw))
	assert.Equal(t, 1, registry.localClientCalls)
}

func newTestClientset(t *testing.T, host string) *kubernetes.Clientset {
	t.Helper()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: host})
	require.NoError(t, err)
	return client
}

// ── UpdateEncryptionConfig: input validation & no-op paths ───────────────────

func TestUpdateEncryptionConfig_NotControlPlane(t *testing.T) {
	srv := newEncryptionServer(t, t.TempDir(), WorkerRole, NewMockK8sEncryptionRunner(), metricsWithCount(0))
	_, err := srv.UpdateEncryptionConfig(context.Background(), &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
			AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "controlplane")
}

func TestUpdateEncryptionConfig_NoOperation(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}})
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithCount(0))
	_, err := srv.UpdateEncryptionConfig(context.Background(), &pb.UpdateEncryptionConfigRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "operation is required")
}

func TestUpdateEncryptionConfig_NoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []KeyConfig
		op   *pb.UpdateEncryptionConfigRequest
	}{
		{
			"AddSecondaryKey already present",
			[]KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}},
			&pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
				AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey1},
			}},
		},
		{
			"PromotePrimaryKey already primary",
			[]KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}, {Name: "kube-api-encryption-key-2", Secret: testKey2}},
			&pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_PromotePrimaryKey{
				PromotePrimaryKey: &pb.PromotePrimaryKeyOperation{NewKey: testKey1},
			}},
		},
		{
			"RemoveOldKeys already sole key",
			[]KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}},
			&pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_RemoveOldKeys{
				RemoveOldKeys: &pb.RemoveOldKeysOperation{PrimaryKey: testKey1},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			writeEncryptionConfigFixture(t, tmpDir, tc.keys)
			srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithCount(0))
			_, err := srv.UpdateEncryptionConfig(context.Background(), tc.op)
			require.NoError(t, err)
			assert.False(t, walFileExists(srv))
		})
	}
}

// ── UpdateEncryptionConfig: fresh write paths ─────────────────────────────────

func TestUpdateEncryptionConfig_WritesConfigAndWaits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keys        []KeyConfig
		op          *pb.UpdateEncryptionConfigRequest
		wantLen     int
		wantPrimary string
	}{
		{
			"AddSecondaryKey",
			[]KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}},
			&pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
				AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
			}},
			2, testKey1,
		},
		{
			"PromotePrimaryKey",
			[]KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}, {Name: "kube-api-encryption-key-2", Secret: testKey2}},
			&pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_PromotePrimaryKey{
				PromotePrimaryKey: &pb.PromotePrimaryKeyOperation{NewKey: testKey2},
			}},
			2, testKey2,
		},
		{
			"RemoveOldKeys",
			[]KeyConfig{
				{Name: "kube-api-encryption-key-1", Secret: testKey1},
				{Name: "kube-api-encryption-key-2", Secret: testKey2},
				{Name: "kube-api-encryption-key-3", Secret: testKey3},
			},
			&pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_RemoveOldKeys{
				RemoveOldKeys: &pb.RemoveOldKeysOperation{PrimaryKey: testKey1},
			}},
			1, testKey1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			writeEncryptionConfigFixture(t, tmpDir, tc.keys)
			srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithReload(5))
			_, err := srv.UpdateEncryptionConfig(context.Background(), tc.op)
			require.NoError(t, err)
			cfg, err := srv.readEncryptionConfig()
			require.NoError(t, err)
			keys := firstSecretboxKeys(t, cfg)
			assert.Len(t, keys, tc.wantLen)
			assert.Equal(t, tc.wantPrimary, keys[0].Secret)
			assert.False(t, walFileExists(srv))
		})
	}
}

// TestUpdateEncryptionConfig_BlockingWaitPath verifies that when Phase 2
// already-confirmed check sees R==N, the blocking wait (Phase 3) executes.
func TestUpdateEncryptionConfig_BlockingWaitPath(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}})
	seq := metricsWithBlockingWaitPath(5)
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), seq)

	_, err := srv.UpdateEncryptionConfig(context.Background(), &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
			AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
		},
	})
	require.NoError(t, err)

	seq.mu.Lock()
	totalCalls := seq.callIdx
	seq.mu.Unlock()
	// call 0: baseline, call 1: phase 2 check (no fast exit), call 2: wait-loop poll
	assert.Equal(t, 3, totalCalls, "expected 3 metrics calls: baseline + phase2-check + wait-loop")
	assert.False(t, walFileExists(srv))
}

// ── UpdateEncryptionConfig: error paths ──────────────────────────────────────

func TestUpdateEncryptionConfig_MetricsUnavailable_FailsBeforeWrite(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}})
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithError("connection refused"))
	_, err := srv.UpdateEncryptionConfig(context.Background(), &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
			AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metrics unavailable")
	cfg, readErr := srv.readEncryptionConfig()
	require.NoError(t, readErr)
	assert.Len(t, firstSecretboxKeys(t, cfg), 1, "config must not be modified")
	assert.False(t, walFileExists(srv))
}

func TestUpdateEncryptionConfig_RemoveOldKeys_NonPrimaryKeyRejected(t *testing.T) {
	// Passing a secondary key as primary_key would discard the actual primary.
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{
		{Name: "kube-api-encryption-key-1", Secret: testKey1},
		{Name: "kube-api-encryption-key-2", Secret: testKey2},
	})
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithCount(0))
	_, err := srv.UpdateEncryptionConfig(context.Background(), &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_RemoveOldKeys{
			RemoveOldKeys: &pb.RemoveOldKeysOperation{PrimaryKey: testKey2},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PromotePrimaryKey first")
}

// ── Crash recovery ────────────────────────────────────────────────────────────

// S1: crash between WAL step ① (WAL written) and ② (config write).
// Config is still "old"; retry must re-write and wait.
func TestCrashRecovery_S1_StateFileWrittenConfigNotYet(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}})
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithReload(5))
	req := &pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
		AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
	}}
	writeWALFixture(t, srv, 5, req)

	_, err := srv.UpdateEncryptionConfig(context.Background(), req)
	require.NoError(t, err)
	cfg, err := srv.readEncryptionConfig()
	require.NoError(t, err)
	assert.Len(t, firstSecretboxKeys(t, cfg), 2, "config must be written on retry")
	assert.False(t, walFileExists(srv))
}

// S2: crash after config write, before reload confirmation. R==N → blocking wait runs.
func TestCrashRecovery_S2_ConfigWrittenReloadPending(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{
		{Name: "kube-api-encryption-key-1", Secret: testKey1},
		{Name: "kube-api-encryption-key-2", Secret: testKey2},
	})
	seq := &metricsSequencer{values: [][]byte{
		metricsOutput(5), // Phase 2: R == walN → enter blocking wait
		metricsOutput(6), // wait loop: R > walN → done
	}}
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), seq)
	req := &pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
		AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
	}}
	writeWALFixture(t, srv, 5, req)

	_, err := srv.UpdateEncryptionConfig(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, walFileExists(srv))
}

// S3: crash after reload confirmed, before WAL deleted. R > walN → skip wait.
func TestCrashRecovery_S3_ReloadConfirmedStateFileStale(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{
		{Name: "kube-api-encryption-key-1", Secret: testKey1},
		{Name: "kube-api-encryption-key-2", Secret: testKey2},
	})
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithCount(7))
	req := &pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
		AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
	}}
	writeWALFixture(t, srv, 5, req) // R=7 > walN=5

	_, err := srv.UpdateEncryptionConfig(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, walFileExists(srv))
}

// Stale opID: WAL left by a different operation whose reload was never
// confirmed. The RPC must return FailedPrecondition — the caller must retry
// the previous operation before issuing a new one.
func TestCrashRecovery_StaleStateFileDifferentOperation(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{
		{Name: "kube-api-encryption-key-2", Secret: testKey2},
		{Name: "kube-api-encryption-key-1", Secret: testKey1},
	})
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithCount(0))
	addSecondaryReq := &pb.UpdateEncryptionConfigRequest{Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
		AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
	}}
	writeWALFixture(t, srv, 5, addSecondaryReq) // opID "add:..."

	// PromotePrimaryKey opID ("promote:...") mismatches → FailedPrecondition.
	_, err := srv.UpdateEncryptionConfig(context.Background(), &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_PromotePrimaryKey{
			PromotePrimaryKey: &pb.PromotePrimaryKeyOperation{NewKey: testKey2},
		},
	})
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	// State file must remain untouched so the prior op can be retried.
	assert.True(t, walFileExists(srv))
}

// ── WAL ordering invariant ────────────────────────────────────────────────────

// TestWALInvariant_WALWrittenBeforeConfig verifies that the WAL (①)
// exists on disk before the config is written (②) — the critical ordering
// invariant that makes crash recovery safe.
func TestWALInvariant_WALWrittenBeforeConfig(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}})

	configPath := filepath.Join(tmpDir, "etc/kubernetes/kube-api-encryption/encryption-config.yaml")
	var walExistsOnPhase2, configOnPhase2 bool
	// call 0: baseline snapshot (inside lock); call 1: phase-2 fast-path check (outside lock).
	// The onCall hook fires at call index 1 to capture filesystem state between ① and ②.
	seq := &metricsSequencer{
		values: [][]byte{metricsOutput(5), metricsOutput(6)},
		onCall: func(idx int) {
			if idx == 1 {
				walExistsOnPhase2 = walFileExists(&NodeAgentServer{RootPath: tmpDir})
				_, err := os.Stat(configPath)
				configOnPhase2 = err == nil
			}
		},
	}
	srv := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), seq)
	_, err := srv.UpdateEncryptionConfig(context.Background(), &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
			AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
		},
	})
	require.NoError(t, err)
	assert.True(t, walExistsOnPhase2, "WAL must exist at Phase 2 (WAL step ① complete)")
	assert.True(t, configOnPhase2, "config must exist at Phase 2 (WAL step ② complete)")
	assert.False(t, walFileExists(srv))
}

// ── waitForEncryptionReload ───────────────────────────────────────────────────

func TestWaitForEncryptionReload_ReloadConfirmed(t *testing.T) {
	srv := &NodeAgentServer{reloadPollInterval: 5 * time.Millisecond, clientRegistry: newMetricsRegistry(t, metricsWithReload(3))}
	assert.NoError(t, srv.waitForEncryptionReload(context.Background(), 3))
}

func TestWaitForEncryptionReload_ContextCancelled(t *testing.T) {
	srv := &NodeAgentServer{reloadPollInterval: 5 * time.Millisecond, clientRegistry: newMetricsRegistry(t, metricsWithCount(5))}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := srv.waitForEncryptionReload(ctx, 5)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.DeadlineExceeded, st.Code())
	assert.Contains(t, err.Error(), "retry is safe")
}

// ── readEncRotationWAL / writeEncRotationWAL ──────────────────────────────────

func TestEncRotationState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc-rotation.state")

	_, exists, err := readEncRotationWAL(path)
	require.NoError(t, err)
	assert.False(t, exists)

	require.NoError(t, writeEncRotationWAL(path, encRotationWALEntry{ReloadCountBefore: 42, OpID: "add:AAAAAAAAAAAA"}))

	s, exists, err := readEncRotationWAL(path)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, 42, s.ReloadCountBefore)
	assert.Equal(t, "add:AAAAAAAAAAAA", s.OpID)
}

func TestEncRotationState_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc-rotation.state")
	require.NoError(t, os.WriteFile(path, []byte("not valid json\n"), 0o600))
	_, _, err := readEncRotationWAL(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse enc-rotation state")
}

func TestEncRotationState_CreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subdir", "deeper", "state")
	require.NoError(t, writeEncRotationWAL(path, encRotationWALEntry{ReloadCountBefore: 99, OpID: "promote:BBBBBBBBBBBB"}))
	s, exists, err := readEncRotationWAL(path)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, 99, s.ReloadCountBefore)
	assert.Equal(t, "promote:BBBBBBBBBBBB", s.OpID)
}

// ── Full key-rotation workflow (E2E) ─────────────────────────────────────────

// TestKeyRotationSequence_E2E chains AddSecondaryKey → PromotePrimaryKey →
// RemoveOldKeys through a single filesystem, verifying config at each step.
func TestKeyRotationSequence_E2E(t *testing.T) {
	tmpDir := t.TempDir()
	writeEncryptionConfigFixture(t, tmpDir, []KeyConfig{{Name: "kube-api-encryption-key-1", Secret: testKey1}})
	ctx := context.Background()

	// Step 1: add key2 as secondary.
	srv1 := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithReload(0))
	_, err := srv1.UpdateEncryptionConfig(ctx, &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_AddSecondaryKey{
			AddSecondaryKey: &pb.AddSecondaryKeyOperation{NewKey: testKey2},
		},
	})
	require.NoError(t, err)
	assert.False(t, walFileExists(srv1))
	cfg, _ := srv1.readEncryptionConfig()
	keys := firstSecretboxKeys(t, cfg)
	require.Len(t, keys, 2)
	assert.Equal(t, testKey1, keys[0].Secret, "key1 remains primary")
	assert.Equal(t, testKey2, keys[1].Secret, "key2 added as secondary")

	// Step 2: promote key2 to primary.
	srv2 := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithReload(1))
	_, err = srv2.UpdateEncryptionConfig(ctx, &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_PromotePrimaryKey{
			PromotePrimaryKey: &pb.PromotePrimaryKeyOperation{NewKey: testKey2},
		},
	})
	require.NoError(t, err)
	assert.False(t, walFileExists(srv2))
	cfg, _ = srv2.readEncryptionConfig()
	keys = firstSecretboxKeys(t, cfg)
	assert.Equal(t, testKey2, keys[0].Secret, "key2 is now primary")
	assert.Equal(t, testKey1, keys[1].Secret, "key1 demoted")

	// Step 3: remove old key1.
	srv3 := newEncryptionServer(t, tmpDir, ControlPlaneRole, NewMockK8sEncryptionRunner(), metricsWithReload(2))
	_, err = srv3.UpdateEncryptionConfig(ctx, &pb.UpdateEncryptionConfigRequest{
		Operation: &pb.UpdateEncryptionConfigRequest_RemoveOldKeys{
			RemoveOldKeys: &pb.RemoveOldKeysOperation{PrimaryKey: testKey2},
		},
	})
	require.NoError(t, err)
	assert.False(t, walFileExists(srv3))
	cfg, _ = srv3.readEncryptionConfig()
	keys = firstSecretboxKeys(t, cfg)
	require.Len(t, keys, 1)
	assert.Equal(t, testKey2, keys[0].Secret)
}
