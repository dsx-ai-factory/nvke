// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

// syncBuffer is a log destination that can be written from the agent's goroutines
// while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs sends everything the agent logs, debug included, to the returned
// buffer for the rest of the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// kubeadmOutputWithCredentials is output that carries every credential kubeadm
// prints on init, ahead of a line that says what happened.
func kubeadmOutputWithCredentials(summary string) string {
	return fmt.Sprintf("[bootstrap-token] Using token: %s\n[upload-certs] Using certificate key:\n%s\n\n"+
		"kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s --control-plane --certificate-key %s\n\n%s\n",
		testToken, testCertificateKey, testToken, testCACertHash, testCertificateKey, summary)
}

func TestExecuteKubeadmJoinDoesNotLogJoinCredentials(t *testing.T) {
	logs := captureLogs(t)
	server := newKubeadmFileTestServer(t)
	server.Runner = &MockRunner{Output: []byte("joined")}
	server.NodeStatus = &NodeStatus{}
	req := &pb.KubeadmJoinRequest{
		KubeadmJoinString: fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s --control-plane --certificate-key %s",
			testToken, testCACertHash, testCertificateKey),
		JoinConfig:     fmt.Sprintf("kind: JoinConfiguration\ndiscovery:\n  tlsBootstrapToken: %s\ncontrolPlane:\n  certificateKey: %s\n", testToken, testCertificateKey),
		CertificateKey: testCertificateKey,
	}

	_, err := server.ExecuteKubeadmJoin(context.Background(), req)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		return server.NodeStatus.Status == ReadyStatus
	}, 5*time.Second, 10*time.Millisecond, "the join did not finish")

	got := logs.String()
	assert.Contains(t, got, "issuing kubeadm join call", "the line is still logged")
	assert.Contains(t, got, "issuing kubeadm join command", "the line is still logged")
	assertNoCredentials(t, got)
	assert.Contains(t, got, testTokenID+".<redacted>", "the token ID is kept for debugging")
	assert.Contains(t, got, testCACertHash, "the public CA hash is kept")
}

func TestProcessKubeadmJoinFailureDoesNotExposeCredentials(t *testing.T) {
	logs := captureLogs(t)
	server := newKubeadmFileTestServer(t)
	server.NodeStatus = &NodeStatus{Status: PendingStatus, Phase: JoinPhase, Role: ControlPlaneRole}
	server.Runner = &MockRunner{
		Output: []byte(kubeadmOutputWithCredentials("error execution phase preflight: the join failed")),
		Err:    errors.New("exit status 1"),
	}

	server.processKubeadmJoin([]string{"join", "10.0.0.1:6443"}, &pb.KubeadmJoinRequest{})

	assert.Equal(t, ErrorStatus, server.NodeStatus.Status)
	assertNoCredentials(t, server.NodeStatus.KubeadmErrorMessage)
	assert.Contains(t, server.NodeStatus.KubeadmErrorMessage, "error execution phase preflight: the join failed", "the reason is kept")
	assertNoCredentials(t, logs.String())
	assert.Contains(t, logs.String(), "join failed")
}

func TestProcessKubeadmInitFailuresDoNotExposeCredentials(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		err        error
		wantStatus string
		wantLogged string
	}{
		{
			name:       "kubeadm init fails",
			output:     kubeadmOutputWithCredentials("error execution phase wait-control-plane: it timed out"),
			err:        errors.New("exit status 1"),
			wantStatus: "kubeadm init failed",
			wantLogged: "init failed",
		},
		{
			name:       "the join strings cannot be parsed from the output",
			output:     fmt.Sprintf("[upload-certs] Using certificate key:\n%s\n[bootstrap-token] Using token: %s\nnothing else\n", testCertificateKey, testToken),
			wantStatus: "failed to parse join strings",
			wantLogged: "failed to parse join strings from output",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			server := &NodeAgentServer{
				Runner:     &MockRunner{Output: []byte(tc.output), Err: tc.err},
				FileWriter: &MockFileWriter{},
				Persister:  &MockPersister{},
				NodeStatus: &NodeStatus{Status: PendingStatus, Phase: InitPhase},
			}

			server.processKubeadmInit([]string{}, &pb.KubeadmInitRequest{NoCniInstall: true})

			assert.Equal(t, ErrorStatus, server.NodeStatus.Status)
			for name, field := range map[string]string{
				"ErrorMessage":        server.NodeStatus.ErrorMessage,
				"KubeadmErrorMessage": server.NodeStatus.KubeadmErrorMessage,
				"CurrentMessage":      server.NodeStatus.CurrentMessage,
			} {
				assertNoCredentials(t, field)
				assert.NotContains(t, field, "Using certificate key:\n"+testCertificateKey, name)
			}
			assert.Contains(t, server.NodeStatus.ErrorMessage+server.NodeStatus.CurrentMessage, tc.wantStatus, "the reason is kept")
			assertNoCredentials(t, logs.String())
			assert.Contains(t, logs.String(), tc.wantLogged, "the line is still logged")
		})
	}
}

func TestJoinStringErrorsDoNotExposeCredentials(t *testing.T) {
	uploadArgs := []string{kubeadmCmdInit, kubeadmCmdPhase, "upload-certs", "--upload-certs"}
	tokenArgs := []string{"token", "create", "--print-join-command"}
	reply := func(out string, err error) runFunc {
		return func(string, []string, *nvkeutils.CommandOptions) ([]byte, error) { return []byte(out), err }
	}
	uploaded := "[upload-certs] Using certificate key:\n" + testCertificateKey + "\n"
	tests := []struct {
		name    string
		runners []runner
		wantErr string
	}{
		{
			name:    "upload-certs fails",
			runners: []runner{runForCommand("kubeadm", uploadArgs, reply(kubeadmOutputWithCredentials("it failed"), errors.New("exit status 1")))},
			wantErr: "exit status 1",
		},
		{
			name:    "upload-certs prints no certificate key",
			runners: []runner{runForCommand("kubeadm", uploadArgs, reply("[bootstrap-token] Using token: "+testToken+"\n", nil))},
			wantErr: "no certificate key",
		},
		{
			name:    "the certificate key is malformed",
			runners: []runner{runForCommand("kubeadm", uploadArgs, reply("[upload-certs] Using certificate key:\n"+testTokenSecret+"\n", nil))},
			wantErr: "not a 32-byte hex string",
		},
		{
			name: "token create fails",
			runners: []runner{
				runForCommand("kubeadm", uploadArgs, reply(uploaded, nil)),
				runForCommand("kubeadm", tokenArgs, reply(kubeadmOutputWithCredentials("it failed"), errors.New("exit status 1"))),
			},
			wantErr: "kubeadm token create",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			server := &NodeAgentServer{
				Runner:     &MockRunner{Runners: tc.runners, Err: errors.New("unexpected command")},
				FileWriter: &MockFileWriter{},
				Persister:  &MockPersister{},
				NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			}

			_, err := server.UpdateAndFetchKubeAdmJoinStrings(context.Background(), &pb.KubeadmJoinStringRequest{})

			require.ErrorContains(t, err, tc.wantErr)
			assertNoCredentials(t, err.Error())
			assertNoCredentials(t, logs.String())
		})
	}
}

// parseJoinCmds logs the whole kubeadm init output at debug level. The join
// commands it returns are the real ones, since the controller uses them.
func TestParseJoinCmdsDebugLogDoesNotExposeCredentials(t *testing.T) {
	logs := captureLogs(t)

	controlPlane, worker := parseJoinCmds(kubeadmOutputWithCredentials("done") + "\nkubeadm join 10.0.0.1:6443 --token " + testToken + "\n")

	assert.Contains(t, controlPlane, testCertificateKey, "the returned join command is the real one")
	assert.Contains(t, worker, testToken, "the returned join command is the real one")
	assert.Contains(t, logs.String(), "executing parseJoinCmds", "the line is still logged")
	assertNoCredentials(t, logs.String())
}
