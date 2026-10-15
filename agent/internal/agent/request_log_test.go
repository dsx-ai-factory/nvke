// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

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

func TestRedactJoinString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a worker join command keeps the token ID and the CA hash",
			in:   fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s", testToken, testCACertHash),
			want: fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s.<redacted> --discovery-token-ca-cert-hash %s", testTokenID, testCACertHash),
		},
		{
			name: "a control plane join command also loses the certificate key",
			in: fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s --control-plane --certificate-key %s",
				testToken, testCACertHash, testCertificateKey),
			want: fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s.<redacted> --discovery-token-ca-cert-hash %s --control-plane --certificate-key <redacted>",
				testTokenID, testCACertHash),
		},
		{
			name: "the --flag=value form",
			in:   fmt.Sprintf("join --token=%s --certificate-key=%s", testToken, testCertificateKey),
			want: fmt.Sprintf("join --token=%s.<redacted> --certificate-key=<redacted>", testTokenID),
		},
		{
			name: "the other flags that take a token, and flags that look like them but are not",
			in: fmt.Sprintf("join --discovery-token %s --tls-bootstrap-token=%s --token-ttl 24h "+
				"--discovery-token-unsafe-skip-ca-verification --node-name gpu-01", testToken, testToken),
			want: fmt.Sprintf("join --discovery-token %[1]s.<redacted> --tls-bootstrap-token=%[1]s.<redacted> --token-ttl 24h "+
				"--discovery-token-unsafe-skip-ca-verification --node-name gpu-01", testTokenID),
		},
		{
			name: "kubeadm's line continuations",
			in:   fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s \\\n    --control-plane --certificate-key %s", testToken, testCertificateKey),
			want: fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s.<redacted> \\\n    --control-plane --certificate-key <redacted>", testTokenID),
		},
		{
			name: "a value that is not a token shape is redacted whole",
			in:   "join --token not-a-token-shape",
			want: "join --token <redacted>",
		},
		{
			name: "a command with no credentials is unchanged",
			in:   "kubeadm join 10.0.0.1:6443 --control-plane",
			want: "kubeadm join 10.0.0.1:6443 --control-plane",
		},
		{name: "an empty string is unchanged", in: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactJoinString(tc.in)

			assert.Equal(t, tc.want, got)
			assertNoCredentials(t, got)
			assert.Equal(t, got, redactJoinString(got), "redacting twice changes nothing")
		})
	}
}

func TestRedactJoinArgs(t *testing.T) {
	args := []string{
		"join", "10.0.0.1:6443", "--token", testToken, "--discovery-token-ca-cert-hash", testCACertHash,
		"--control-plane", "--certificate-key", testCertificateKey,
	}
	original := append([]string(nil), args...)

	got := redactJoinArgs(args)

	assert.Equal(t, []string{
		"join", "10.0.0.1:6443", "--token", testTokenID + ".<redacted>", "--discovery-token-ca-cert-hash", testCACertHash,
		"--control-plane", "--certificate-key", "<redacted>",
	}, got)
	assert.Equal(t, original, args, "the input must not be modified")

	t.Run("the --flag=value form", func(t *testing.T) {
		got := redactJoinArgs([]string{"join", "--token=" + testToken, "--certificate-key=" + testCertificateKey})
		assert.Equal(t, []string{"join", "--token=" + testTokenID + ".<redacted>", "--certificate-key=<redacted>"}, got)
	})
	t.Run("the other flags that take a token", func(t *testing.T) {
		redactedToken := testTokenID + ".<redacted>"

		got := redactJoinArgs([]string{"join", "--discovery-token", testToken, "--tls-bootstrap-token=" + testToken, "--token-ttl", "24h"})

		assert.Equal(t, []string{"join", "--discovery-token", redactedToken, "--tls-bootstrap-token=" + redactedToken, "--token-ttl", "24h"}, got)
	})
	t.Run("a flag with no value is left alone", func(t *testing.T) {
		assert.Equal(t, []string{"join", "--token"}, redactJoinArgs([]string{"join", "--token"}))
	})
	t.Run("no arguments", func(t *testing.T) {
		assert.Empty(t, redactJoinArgs(nil))
	})
}

func TestRedactKubeadmConfig(t *testing.T) {
	join := fmt.Sprintf(`apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: 10.0.0.1:6443
    token: %s
    caCertHashes:
    - %s
  timeout: 5m0s
  tlsBootstrapToken: %s
controlPlane:
  certificateKey: "%s"
  localAPIEndpoint:
    advertiseAddress: 10.0.0.2
`, testToken, testCACertHash, testToken, testCertificateKey)
	init := fmt.Sprintf(`apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
bootstrapTokens:
- token: '%s'
  ttl: 24h0m0s
  tokenTTL: not-a-credential
certificateKey: %s
`, testToken, testCertificateKey)

	t.Run("a join configuration", func(t *testing.T) {
		got := redactKubeadmConfig(join)

		assertNoCredentials(t, got)
		assert.Contains(t, got, "token: "+testTokenID+".<redacted>\n")
		assert.Contains(t, got, "tlsBootstrapToken: "+testTokenID+".<redacted>\n")
		assert.Contains(t, got, `certificateKey: "<redacted>"`)
		assert.Equal(t, strings.Count(join, "\n"), strings.Count(got, "\n"), "every line is still there")
		for _, kept := range []string{"apiServerEndpoint: 10.0.0.1:6443", testCACertHash, "timeout: 5m0s", "advertiseAddress: 10.0.0.2"} {
			assert.Contains(t, got, kept)
		}
	})
	t.Run("an init configuration, with the token in a list item and quoted", func(t *testing.T) {
		got := redactKubeadmConfig(init)

		assertNoCredentials(t, got)
		assert.Contains(t, got, "- token: '"+testTokenID+".<redacted>'")
		assert.Contains(t, got, "certificateKey: <redacted>")
		assert.Contains(t, got, "tokenTTL: not-a-credential", "a key that only starts with token is not a credential")
		assert.Contains(t, got, "ttl: 24h0m0s")
	})
	t.Run("a configuration with no credentials is unchanged", func(t *testing.T) {
		const plain = "kind: InitConfiguration\nnodeRegistration:\n  name: node-1\n"
		assert.Equal(t, plain, redactKubeadmConfig(plain))
	})
	t.Run("redacting twice changes nothing", func(t *testing.T) {
		once := redactKubeadmConfig(join)
		assert.Equal(t, once, redactKubeadmConfig(once))
	})
}

func TestRedactKubeadmOutput(t *testing.T) {
	out := fmt.Sprintf(`[bootstrap-token] Using token: %s
[upload-certs] Using certificate key:
%s

Then you can join any number of worker nodes by running the following on each as root:

kubeadm join 10.0.0.1:6443 --token %s \
    --discovery-token-ca-cert-hash %s \
    --control-plane --certificate-key %s
`, testToken, testCertificateKey, testToken, testCACertHash, testCertificateKey)

	got := redactKubeadmOutput(out)

	assertNoCredentials(t, got)
	assert.Contains(t, got, "Using token: "+testTokenID+".<redacted>")
	assert.Contains(t, got, "Using certificate key:\n<redacted>")
	assert.Contains(t, got, "--token "+testTokenID+".<redacted>")
	assert.Contains(t, got, testCACertHash, "the CA hash is public and is kept")
	assert.Contains(t, got, "Then you can join any number of worker nodes", "the prose is kept")
	assert.Equal(t, got, redactKubeadmOutput(got), "redacting twice changes nothing")
	assert.Equal(t, "error execution phase preflight: [preflight] failed", redactKubeadmOutput("error execution phase preflight: [preflight] failed"))
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
