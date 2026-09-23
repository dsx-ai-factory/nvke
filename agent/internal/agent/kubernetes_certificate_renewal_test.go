// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
	nkeutils "github.com/dsx-ai-factory/nke/agent/pkg/utils"
)

const certificateExpirationOutput = `
[check-expiration] Reading configuration from the cluster...
CERTIFICATE                EXPIRES                  RESIDUAL TIME   CERTIFICATE AUTHORITY   EXTERNALLY MANAGED
admin.conf                 Aug 29, 2026 00:00 UTC   1d              ca                      no
apiserver                  Aug 30, 2026 00:00 UTC   2d              ca                      no
controller-manager.conf    Aug 28, 2026 12:00 UTC   12h             ca                      no
front-proxy-client         Aug 20, 2026 00:00 UTC   <invalid>       front-proxy-ca          no
scheduler.conf             Aug 28, 2027 00:00 UTC   365d            ca                      no

CERTIFICATE AUTHORITY      EXPIRES                  RESIDUAL TIME   EXTERNALLY MANAGED
ca                         Aug 28, 2035 00:00 UTC   8y              no
`

type certificateRenewalTestRunner struct {
	mu                  sync.Mutex
	rootPath            string
	checkOutput         string
	renewCalls          int
	failWhenStopped     bool
	conflictWhenStopped bool
	stopFailureIssued   bool
}

func (r *certificateRenewalTestRunner) Run(name string, args []string, _ *nkeutils.CommandOptions) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name != "kubeadm" {
		return nil, fmt.Errorf("unexpected command %s", name)
	}
	switch strings.Join(args, " ") {
	case "certs check-expiration":
		return []byte(r.checkOutput), nil
	case "certs renew all":
		r.renewCalls++
		return []byte("renewed"), nil
	default:
		return nil, fmt.Errorf("unexpected kubeadm args: %s", strings.Join(args, " "))
	}
}

func (r *certificateRenewalTestRunner) RunSplitOutput(
	name string,
	args []string,
	_ *nkeutils.CommandOptions,
) (nkeutils.CommandOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name != "crictl" || len(args) < 3 {
		return nkeutils.CommandOutput{}, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	component := strings.TrimPrefix(args[2], "io.kubernetes.container.name=")
	manifest := map[string]string{
		"kube-apiserver":          "kube-apiserver.yaml",
		"kube-controller-manager": "kube-controller-manager.yaml",
		"kube-scheduler":          "kube-scheduler.yaml",
		"etcd":                    "etcd.yaml",
	}[component]
	if manifest == "" {
		return nkeutils.CommandOutput{}, fmt.Errorf("unexpected component %s", component)
	}
	_, statErr := os.Stat(filepath.Join(r.rootPath, "etc", "kubernetes", "manifests", manifest))
	running := statErr == nil
	if !running && r.failWhenStopped && !r.stopFailureIssued {
		r.stopFailureIssued = true
		if r.conflictWhenStopped {
			path := filepath.Join(r.rootPath, "etc", "kubernetes", "manifests", manifest)
			if err := os.WriteFile(path, []byte("conflicting manifest"), 0o600); err != nil {
				return nkeutils.CommandOutput{}, err
			}
		}
		return nkeutils.CommandOutput{Stderr: []byte("runtime unavailable")}, errors.New("runtime unavailable")
	}
	if running {
		return nkeutils.CommandOutput{Stdout: []byte(`{"containers":[{"id":"new-container"}]}`)}, nil
	}
	return nkeutils.CommandOutput{Stdout: []byte(`{"containers":[]}`)}, nil
}

func (r *certificateRenewalTestRunner) RunDetached(string, []string, *nkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

func TestExpiringKubernetesCertificates(t *testing.T) {
	expiring, err := expiringKubernetesCertificates(certificateExpirationOutput, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"admin.conf", "controller-manager.conf", "front-proxy-client"}, expiring)
}

func TestKubernetesCertificateRenewalUnchanged(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{
		rootPath: rootPath,
		checkOutput: `CERTIFICATE EXPIRES RESIDUAL TIME CERTIFICATE AUTHORITY EXTERNALLY MANAGED
admin.conf Aug 28, 2027 00:00 UTC 365d ca no
CERTIFICATE AUTHORITY EXPIRES RESIDUAL TIME EXTERNALLY MANAGED
ca Aug 28, 2035 00:00 UTC 8y no`,
	}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalOperation(t, server, "unchanged", 1)

	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_UNCHANGED, operation.GetOutcome())
	assert.Zero(t, runner.renewCalls)
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

func TestKubernetesCertificateRenewalHappyPath(t *testing.T) {
	rootPath := t.TempDir()
	writeCertificateRenewalManifests(t, rootPath)
	runner := &certificateRenewalTestRunner{rootPath: rootPath, checkOutput: certificateExpirationOutput}
	server := newCertificateRenewalTestServer(rootPath, runner)

	operation := runCertificateRenewalOperation(t, server, "renewed", 7)

	assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, operation.GetOperation().GetPhase())
	assert.Equal(t, pb.KubernetesCertificateRenewalOperation_OUTCOME_RENEWED, operation.GetOutcome())
	assert.Equal(t, 1, runner.renewCalls)
	assert.Empty(t, server.kubernetesCertificateRenewalState())
	assertCertificateRenewalManifestsRestored(t, rootPath)
}

func TestKubernetesCertificateRenewalFailureRestoresManifests(t *testing.T) {
	tests := []struct {
		name        string
		conflict    bool
		wantOutcome pb.KubernetesCertificateRenewalOperation_Outcome
		wantStaged  bool
	}{
		{
			name:        "runtime failure restores all manifests",
			wantOutcome: pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_RESTORED,
		},
		{
			name:        "conflicting active manifest reports incomplete restore",
			conflict:    true,
			wantOutcome: pb.KubernetesCertificateRenewalOperation_OUTCOME_FAILED_NOT_RESTORED,
			wantStaged:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootPath := t.TempDir()
			writeCertificateRenewalManifests(t, rootPath)
			runner := &certificateRenewalTestRunner{
				rootPath:            rootPath,
				checkOutput:         certificateExpirationOutput,
				failWhenStopped:     true,
				conflictWhenStopped: tt.conflict,
			}
			server := newCertificateRenewalTestServer(rootPath, runner)

			operation := runCertificateRenewalOperation(t, server, "failed", 7)

			assert.Equal(t, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED, operation.GetOperation().GetPhase())
			assert.Equal(t, tt.wantOutcome, operation.GetOutcome())
			_, statErr := os.Stat(filepath.Join(rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/")))
			assert.Equal(t, tt.wantStaged, statErr == nil)
			if !tt.conflict {
				assertCertificateRenewalManifestsRestored(t, rootPath)
			}
		})
	}
}

func TestRecoverInterruptedCertificateRenewalRestoresBeforeRestart(t *testing.T) {
	rootPath := t.TempDir()
	stagingDir := filepath.Join(rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/"))
	require.NoError(t, os.MkdirAll(stagingDir, 0o700))
	for _, name := range append(certificateRenewalRequiredManifests, "kube-vip.yaml") {
		require.NoError(t, os.WriteFile(filepath.Join(stagingDir, name), []byte(name), 0o600))
	}
	server := &NodeAgentServer{
		RootPath:  rootPath,
		Persister: &MockPersister{},
		NodeStatus: &NodeStatus{
			Role: ControlPlaneRole,
			NodeAgentOperations: map[string]NodeAgentOperation{
				"stale": {
					ID: "stale", Type: operationTypeKubernetesCertificateRenewal,
					Phase: NodeAgentOperationPhaseRunning,
				},
			},
		},
	}

	require.NoError(t, server.recoverInterruptedCertificateRenewal("stale"))

	assertCertificateRenewalManifestsRestored(t, rootPath)
	_, found := server.NodeStatus.NodeAgentOperations["stale"]
	assert.False(t, found)
}

func newCertificateRenewalTestServer(rootPath string, runner nkeutils.CommandRunner) *NodeAgentServer {
	return &NodeAgentServer{
		RootPath:                       rootPath,
		Runner:                         runner,
		Persister:                      &MockPersister{},
		NodeStatus:                     &NodeStatus{Role: ControlPlaneRole},
		certificateRenewalPollInterval: time.Millisecond,
		certificateRenewalStopTimeout:  100 * time.Millisecond,
		certificateRenewalStartTimeout: 100 * time.Millisecond,
	}
}

func runCertificateRenewalOperation(
	t *testing.T,
	server *NodeAgentServer,
	operationID string,
	thresholdDays int32,
) *pb.KubernetesCertificateRenewalOperation {
	t.Helper()
	response, err := server.StartKubernetesCertificateRenewal(context.Background(), &pb.StartKubernetesCertificateRenewalRequest{
		OperationId: operationID, ThresholdDays: thresholdDays,
	})
	require.NoError(t, err)
	require.NotNil(t, response.GetOperation())
	deadline := time.Now().Add(2 * time.Second)
	for {
		statusResponse, statusErr := server.GetKubernetesCertificateRenewalStatus(
			context.Background(),
			&pb.GetKubernetesCertificateRenewalStatusRequest{OperationId: operationID},
		)
		require.NoError(t, statusErr)
		operation := statusResponse.GetOperation()
		require.NotNil(t, operation)
		if operation.GetOperation().GetPhase() != pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING {
			return operation
		}
		if time.Now().After(deadline) {
			t.Fatalf("certificate renewal operation did not finish: %v", operation)
		}
		time.Sleep(time.Millisecond)
	}
}

func writeCertificateRenewalManifests(t *testing.T, rootPath string) {
	t.Helper()
	dir := filepath.Join(rootPath, strings.TrimPrefix(defaultStaticPodManifestDir, "/"))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, name := range append(certificateRenewalRequiredManifests, "kube-vip.yaml") {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600))
	}
}

func assertCertificateRenewalManifestsRestored(t *testing.T, rootPath string) {
	t.Helper()
	dir := filepath.Join(rootPath, strings.TrimPrefix(defaultStaticPodManifestDir, "/"))
	for _, name := range append(certificateRenewalRequiredManifests, "kube-vip.yaml") {
		contents, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, name, string(contents))
	}
	_, err := os.Stat(filepath.Join(rootPath, strings.TrimPrefix(certificateRenewalStagingDir, "/")))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
