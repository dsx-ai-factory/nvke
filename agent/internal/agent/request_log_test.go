// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/dsx-ai-factory/nvke/agent/internal/redact"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const testEncryptionKey = "KEY-MATERIAL-THAT-MUST-NOT-BE-LOGGED"

func TestLoggableRequestsDropTheEncryptionKey(t *testing.T) {
	cases := []struct {
		name string
		// build returns a request carrying key, and the copy that would be
		// logged for it.
		build func(key string) (request, logged proto.Message)
	}{
		{
			name: "init",
			build: func(key string) (proto.Message, proto.Message) {
				req := &pb.KubeadmInitRequest{InitConfig: "config", ApiEncryptionEnabled: true, ApiEncryptionKey: key}
				return req, loggableInitRequest(req)
			},
		},
		{
			name: "join",
			build: func(key string) (proto.Message, proto.Message) {
				req := &pb.KubeadmJoinRequest{KubeadmJoinString: "join-string", ApiEncryptionKey: key}
				return req, loggableJoinRequest(req)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+" drops the key and nothing else", func(t *testing.T) {
			req, logged := tc.build(testEncryptionKey)

			assert.Equal(t, testEncryptionKey, encryptionKeyField(req), "the request itself must be untouched")
			assert.Empty(t, encryptionKeyField(logged))

			want := proto.Clone(req)
			clearEncryptionKeyField(want)
			assert.True(t, proto.Equal(want, logged), "logged copy differs from the request in more than the key")

			assertNotLogged(t, "issuing kubeadm "+tc.name+" call", logged)
		})

		t.Run(tc.name+" without a key is returned as is", func(t *testing.T) {
			req, logged := tc.build("")
			assert.Same(t, req, logged)
		})
	}
}

func TestEncryptionKeyDigest(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want func(t *testing.T, digest string)
	}{
		{"no key, no digest", "", func(t *testing.T, digest string) { assert.Empty(t, digest) }},
		{"a key digests to 64 hex digits", testEncryptionKey, func(t *testing.T, digest string) {
			assert.Len(t, digest, 64)
			assert.NotContains(t, digest, testEncryptionKey)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.want(t, encryptionKeyDigest(tc.key)) })
	}
	assert.Equal(t, encryptionKeyDigest(testEncryptionKey), encryptionKeyDigest(testEncryptionKey), "stable")
}

// assertNotLogged renders request the way the agent's JSON slog handler does
// and checks the key is not in the output.
func assertNotLogged(t *testing.T, msg string, request any) {
	t.Helper()
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info(msg, "request", request)

	require.True(t, json.Valid(buf.Bytes()), "log line is not JSON: %s", buf.String())
	assert.NotContains(t, buf.String(), testEncryptionKey)
	assert.NotContains(t, buf.String(), `"api_encryption_key"`, "the field should be absent, not empty")
}

const encryptionKeyFieldName protoreflect.Name = "api_encryption_key"

func encryptionKeyField(m proto.Message) string {
	r := m.ProtoReflect()
	return r.Get(r.Descriptor().Fields().ByName(encryptionKeyFieldName)).String()
}

func clearEncryptionKeyField(m proto.Message) {
	r := m.ProtoReflect()
	r.Clear(r.Descriptor().Fields().ByName(encryptionKeyFieldName))
}

// The credentials below are built from repeated characters, not written out, so
// that no test file holds something that looks like a real token or key. They
// have the shapes kubeadm checks for: a 6 character ID, a dot and a 16 character
// secret, and a 64 digit hex key.
var (
	testTokenID        = "aaaaaa"
	testTokenSecret    = strings.Repeat("b", 16)
	testToken          = testTokenID + "." + testTokenSecret
	testCertificateKey = strings.Repeat("c", 64)
	testCACertHash     = "sha256:" + strings.Repeat("d", 64)
)

// assertNoCredentials fails if got holds the token's secret or the certificate key.
func assertNoCredentials(t *testing.T, got string) {
	t.Helper()
	assert.NotContains(t, got, testTokenSecret, "the token's secret is in %q", got)
	assert.NotContains(t, got, testCertificateKey, "the certificate key is in %q", got)
}

func TestLoggableRequestsRedactJoinCredentials(t *testing.T) {
	joinString := fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s --control-plane --certificate-key %s",
		testToken, testCACertHash, testCertificateKey)
	joinConfig := fmt.Sprintf("kind: JoinConfiguration\ndiscovery:\n  tlsBootstrapToken: %s\ncontrolPlane:\n  certificateKey: %s\n",
		testToken, testCertificateKey)
	initConfig := fmt.Sprintf("kind: InitConfiguration\nbootstrapTokens:\n- token: %s\ncertificateKey: %s\n", testToken, testCertificateKey)
	files := []*pb.File{{Path: "/etc/kubernetes/example.yaml", Content: []byte("kept as it is"), Mode: 0o600}}

	t.Run("join", func(t *testing.T) {
		req := &pb.KubeadmJoinRequest{
			KubeadmJoinString: joinString, JoinConfig: joinConfig, CertificateKey: testCertificateKey, Files: files,
		}

		logged := loggableJoinRequest(req)

		assertNoCredentials(t, logged.GetKubeadmJoinString())
		assertNoCredentials(t, logged.GetJoinConfig())
		assert.Equal(t, "<redacted>", logged.GetCertificateKey(), "the field is shown as present, not as its value")
		assert.Contains(t, logged.GetKubeadmJoinString(), testTokenID+".<redacted>")
		assert.Contains(t, logged.GetKubeadmJoinString(), testCACertHash)
		assert.True(t, proto.Equal(&pb.File{Path: files[0].Path, Content: files[0].Content, Mode: files[0].Mode}, logged.GetFiles()[0]),
			"the files, and their contents, are logged as before")
		assert.Equal(t, joinString, req.GetKubeadmJoinString(), "the request itself must be untouched")
		assert.Equal(t, testCertificateKey, req.GetCertificateKey(), "the request itself must be untouched")
		assertRequestLogHasNoCredentials(t, "issuing kubeadm join call", logged)
	})
	t.Run("init", func(t *testing.T) {
		req := &pb.KubeadmInitRequest{InitConfig: initConfig, ApiEncryptionKey: testEncryptionKey}

		logged := loggableInitRequest(req)

		assertNoCredentials(t, logged.GetInitConfig())
		assert.Empty(t, logged.GetApiEncryptionKey(), "the etcd key is still removed")
		assert.Equal(t, initConfig, req.GetInitConfig(), "the request itself must be untouched")
		assertRequestLogHasNoCredentials(t, "issuing kubeadm init call", logged)
	})
	t.Run("init extra arguments and an inline configuration", func(t *testing.T) {
		req := &pb.KubeadmInitRequest{
			ExtraArgs:  []string{"--token=" + testToken, "--node-name", "gpu-01"},
			InitConfig: fmt.Sprintf("kind: InitConfiguration\nbootstrapTokens: [{token: %s, ttl: 24h0m0s}]\n", testToken),
		}

		logged := loggableInitRequest(req)

		assert.Equal(t, []string{"--token=" + testTokenID + ".<redacted>", "--node-name", "gpu-01"}, logged.GetExtraArgs())
		assert.Equal(t, "kind: InitConfiguration\nbootstrapTokens: [{token: "+testTokenID+".<redacted>, ttl: 24h0m0s}]\n", logged.GetInitConfig())
		assert.Equal(t, []string{"--token=" + testToken, "--node-name", "gpu-01"}, req.GetExtraArgs(), "the request itself must be untouched")
		assertRequestLogHasNoCredentials(t, "issuing kubeadm init call", logged)
	})
	t.Run("a request with nothing to redact is returned as it is", func(t *testing.T) {
		join := &pb.KubeadmJoinRequest{KubeadmJoinString: "kubeadm join 10.0.0.1:6443 --control-plane"}
		init := &pb.KubeadmInitRequest{InitConfig: "kind: InitConfiguration\n"}

		assert.Same(t, join, loggableJoinRequest(join))
		assert.Same(t, init, loggableInitRequest(init))
	})
}

// assertRequestLogHasNoCredentials renders request the way the agent's JSON slog
// handler does and checks no credential is in the output.
func assertRequestLogHasNoCredentials(t *testing.T, msg string, request any) {
	t.Helper()
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info(msg, "request", request)

	require.True(t, json.Valid(buf.Bytes()), "log line is not JSON: %s", buf.String())
	assertNoCredentials(t, buf.String())
}

func TestKubeadmFailureMessageRedactsBeforeTruncating(t *testing.T) {
	creds := redact.New()
	creds.AddToken(testToken)
	// The token starts a few characters before the 300 character limit, so cutting
	// the line first would leave its first characters, which are no longer a token.
	const prefix = "error execution phase kubelet-start: "
	line := prefix + strings.Repeat("x", 300-len(prefix)-12) + " " + testToken + " rejected"

	got := kubeadmFailureMessage(errors.New("exit status 1"), []byte("noise\n"+line+"\n"), creds)

	assert.NotContains(t, got, "bbb", "no part of the token's secret is left")
	assert.Contains(t, got, "error execution phase kubelet-start:")
	assert.Contains(t, got, testTokenID+".<r", "the token ID is kept")
}

func TestProcessKubeadmJoinFailureKeepsTheReasonAndRedactsTheCredentials(t *testing.T) {
	server := newKubeadmFileTestServer(t)
	server.NodeStatus = &NodeStatus{Status: PendingStatus, Phase: JoinPhase, Role: ControlPlaneRole}
	server.Runner = &MockRunner{
		Output: []byte("kubeadm join 10.0.0.1:6443 --token " + testToken + " --certificate-key " + testCertificateKey +
			"\nerror execution phase kubelet-start: lookup node01.abcdefghijklmnop.example: no such host\n"),
		Err: errors.New("exit status 1"),
	}
	req := &pb.KubeadmJoinRequest{CertificateKey: testCertificateKey}

	server.processKubeadmJoin([]string{"join", "10.0.0.1:6443", "--token", testToken, "--certificate-key", testCertificateKey}, req)

	assert.Equal(t, ErrorStatus, server.NodeStatus.Status)
	assertNoCredentials(t, server.NodeStatus.KubeadmErrorMessage)
	assert.Contains(t, server.NodeStatus.KubeadmErrorMessage, "lookup node01.abcdefghijklmnop.example: no such host", "the reason is kept as it was")
	assert.Equal(t, testCertificateKey, req.GetCertificateKey(), "execution inputs are untouched")
}
