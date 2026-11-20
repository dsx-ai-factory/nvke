// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
)

// supportBundleLogs are the support bundle members that hold the agent's logs.
var supportBundleLogs = []string{"nvke-agent.journal.log", "nke-audit.log", "nvke-execute-scripts.log"}

func TestGetSupportBundleRedactsJoinCredentials(t *testing.T) {
	workerJoin := fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s", testToken, testCACertHash)
	controlPlaneJoin := workerJoin + " --control-plane --certificate-key " + testCertificateKey
	joinArgs := append(strings.Fields(controlPlaneJoin)[1:], "--node-name", "gpu-01")

	// Each log holds the credentials in the forms the agent writes them.
	journal := journalLines(jsonLog(func(logger *slog.Logger) {
		logger.Info("issuing kubeadm join command", "args", joinArgs)
		logger.Error("init failed", "err", "exit status 1",
			"stdout", "[upload-certs] Storing the certificates in Secret \"kubeadm-certs\"\n[upload-certs] Using certificate key:\n"+testCertificateKey+"\n")
	}))
	journal = append(journal, "Nov 19 17:16:12 gpu-01 nvke-agent[1234]: [upload-certs] Using certificate key:\n"+
		"                                         "+testCertificateKey+"\n"...)
	audit := jsonLog(func(logger *slog.Logger) {
		logger.Info("grpc request received",
			"method", pb.NodeAgent_ExecuteKubeadmJoin_FullMethodName,
			"error", "rpc error: code = Internal desc = command \""+controlPlaneJoin+"\" failed: exit status 1",
			"user-agent", "grpc-go/1.79.0",
			"client-address", "10.0.0.2:41234",
			"duration_ms", 1520)
	})
	// Script records with raw credentials, so that the bundle's redaction is tested.
	scriptLog := jsonLog(func(logger *slog.Logger) {
		logger.Info("script execution finished", "request_id", "req-1", "script_name", "rejoin.sh",
			"args", []string{"--token", testToken, "--certificate-key", testCertificateKey}, "output", controlPlaneJoin+"\n")
		logger.Info("script execution finished", "request_id", "req-2", "script_name", "list-tokens.sh",
			"output", "TOKEN   TTL\n"+testToken+"   23h\n")
	})

	cases := []struct {
		name     string
		joinInfo JoinInfo
	}{
		{
			name:     "first control-plane node, which holds the join strings",
			joinInfo: JoinInfo{ControlPlaneJoinString: controlPlaneJoin, WorkerJoinString: workerJoin},
		},
		{
			name: "worker node, which holds no join strings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("Doing: laying out a node whose logs hold the join credentials")
			root := t.TempDir()
			state, err := json.Marshal(NodeStatus{Status: ReadyStatus, JoinInfo: tc.joinInfo})
			require.NoError(t, err)
			writeHostFiles(t, root, map[string]string{StateFile: string(state), NVKEAuditLog: audit, NVKEExecuteScriptsLog: scriptLog})
			server := supportBundleServer(root, journal, tc.joinInfo)

			t.Log("Doing: collecting a support bundle")
			members := collectSupportBundle(t, server, nil)

			for _, name := range append([]string{"state.json"}, supportBundleLogs...) {
				got, ok := members[name]
				require.True(t, ok, "%s is not in the bundle", name)

				t.Logf("Testing: %s holds no token secret or certificate key", name)
				assertNoCredentials(t, got)
			}
			for _, name := range supportBundleLogs {
				t.Logf("Testing: %s keeps the token ID", name)
				assert.Contains(t, members[name], testTokenID+".<redacted>")
			}

			t.Log("Testing: the bundled state still parses and keeps the rest of each join string")
			var bundled NodeStatus
			require.NoError(t, json.Unmarshal([]byte(members["state.json"]), &bundled))
			assert.Equal(t, ReadyStatus, bundled.Status)
			if tc.joinInfo.ControlPlaneJoinString != "" {
				assert.Contains(t, bundled.JoinInfo.ControlPlaneJoinString, "10.0.0.1:6443")
				assert.Contains(t, bundled.JoinInfo.ControlPlaneJoinString, testCACertHash)
				assert.Contains(t, bundled.JoinInfo.ControlPlaneJoinString, testTokenID+".<redacted>")
				assert.Contains(t, bundled.JoinInfo.ControlPlaneJoinString, "--certificate-key <redacted>")
			} else {
				t.Log("Testing: the bundled state of a node that holds no join strings is unchanged")
				assert.Equal(t, string(state), members["state.json"])
			}

			t.Log("Testing: the node's own state file and join strings are untouched")
			onDisk, err := os.ReadFile(filepath.Join(root, StateFile))
			require.NoError(t, err)
			assert.Equal(t, string(state), string(onDisk))
			assert.Equal(t, tc.joinInfo, server.NodeStatus.JoinInfo)
		})
	}
}

func TestGetSupportBundleRedactsKnownJoinCredentials(t *testing.T) {
	controlPlaneJoin := fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --discovery-token-ca-cert-hash %s --control-plane --certificate-key %s",
		testToken, testCACertHash, testCertificateKey)
	joinInfo := JoinInfo{ControlPlaneJoinString: controlPlaneJoin}
	// Only the node's join strings identify a bare certificate key.
	journal := journalLines(jsonLog(func(logger *slog.Logger) {
		logger.Error("bootstrap token rejected", "token", testToken, "key", testCertificateKey)
	}))

	cases := []struct {
		name           string
		heldJoinInfo   JoinInfo
		storedJoinInfo JoinInfo
	}{
		{name: "join strings held in memory", heldJoinInfo: joinInfo},
		{name: "join strings in the state file only", storedJoinInfo: joinInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("Doing: collecting a support bundle from a node whose journal holds a bare certificate key")
			root := t.TempDir()
			state, err := json.Marshal(NodeStatus{Status: ReadyStatus, JoinInfo: tc.storedJoinInfo})
			require.NoError(t, err)
			writeHostFiles(t, root, map[string]string{StateFile: string(state)})

			members := collectSupportBundle(t, supportBundleServer(root, journal, tc.heldJoinInfo), nil)

			t.Log("Testing: the journal holds no token secret or certificate key, and keeps the token ID")
			assertNoCredentials(t, members["nvke-agent.journal.log"])
			assert.Contains(t, members["nvke-agent.journal.log"], testTokenID+".<redacted>")
		})
	}

	t.Run("a state file that does not parse is still redacted by context", func(t *testing.T) {
		t.Log("Doing: collecting a support bundle from a node whose state file does not parse")
		root := t.TempDir()
		writeHostFiles(t, root, map[string]string{StateFile: "{\"joinInfo\": " + controlPlaneJoin})

		members := collectSupportBundle(t, supportBundleServer(root, nil, JoinInfo{}), nil)

		t.Log("Testing: the state file holds no token secret or certificate key, and keeps the token ID")
		assertNoCredentials(t, members["state.json"])
		assert.Contains(t, members["state.json"], testTokenID+".<redacted>")
	})
}

func TestGetSupportBundleRedactsLoggedJoinRequests(t *testing.T) {
	joinConfig := fmt.Sprintf(`apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: 10.0.0.1:6443
    token: %[1]s
    caCertHashes:
    - %[2]s
  tlsBootstrapToken: %[1]s
controlPlane:
  certificateKey: %[3]s
`, testToken, testCACertHash, testCertificateKey)
	imageDigest := "registry.k8s.io/kube-apiserver@sha256:" + strings.Repeat("e", 64)
	// A worker holds no join strings, so field names and shape identify these.
	journal := journalLines(jsonLog(func(logger *slog.Logger) {
		logger.Info("issuing kubeadm join call", "request", &pb.KubeadmJoinRequest{JoinConfig: joinConfig, CertificateKey: testCertificateKey, ApiEncryptionKey: testEncryptionKey})
		logger.Info("pulled image", "image", imageDigest)
	}))
	t.Log("Doing: collecting a support bundle from a worker whose journal holds a join request")
	root := t.TempDir()
	state, err := json.Marshal(NodeStatus{Status: ReadyStatus})
	require.NoError(t, err)
	writeHostFiles(t, root, map[string]string{StateFile: string(state)})

	members := collectSupportBundle(t, supportBundleServer(root, journal, JoinInfo{}), nil)
	got := members["nvke-agent.journal.log"]

	t.Log("Testing: the journal holds no token secret, certificate key or etcd encryption key")
	assertNoCredentials(t, got)
	assert.NotContains(t, got, testEncryptionKey)
	assert.Contains(t, got, `"api_encryption_key":"<redacted>"`)

	t.Log("Testing: the journal keeps the token ID, the CA hash and the image digest")
	assert.Contains(t, got, `token: `+testTokenID+".<redacted>")
	assert.Contains(t, got, `tlsBootstrapToken: `+testTokenID+".<redacted>")
	assert.Contains(t, got, testCACertHash)
	assert.Contains(t, got, imageDigest)
}

func TestGetSupportBundleCopiesAdditionalFilesUnchanged(t *testing.T) {
	const requested = "/etc/kubernetes/kubeadm-join.sh"
	content := fmt.Sprintf("kubeadm join 10.0.0.1:6443 --token %s --control-plane --certificate-key %s\n", testToken, testCertificateKey)
	t.Log("Doing: collecting a support bundle with a caller-requested file that holds join credentials")
	root := t.TempDir()
	writeHostFiles(t, root, map[string]string{requested: content})

	members := collectSupportBundle(t, supportBundleServer(root, nil, JoinInfo{}), []string{requested})

	t.Log("Testing: the caller-requested file is in the bundle as it is on the node")
	assert.Equal(t, content, members["0__etc_kubernetes_kubeadm-join.sh"])
}

func TestGetSupportBundleRedactsLongLines(t *testing.T) {
	prefix := strings.Repeat("x", 100*1024) + " "
	t.Log("Doing: collecting a support bundle whose audit log has a line over 64 KiB that ends in a token")
	root := t.TempDir()
	writeHostFiles(t, root, map[string]string{NVKEAuditLog: prefix + testToken + "\nnext line\n"})

	members := collectSupportBundle(t, supportBundleServer(root, nil, JoinInfo{}), nil)

	t.Log("Testing: the long line is kept whole, with the token's secret redacted")
	assert.Equal(t, prefix+testTokenID+".<redacted>\nnext line\n", members["nke-audit.log"])
}

func TestLogScriptExecutionRedactsCredentials(t *testing.T) {
	joinString := "kubeadm join 10.0.0.1:6443 --token " + testToken + " --control-plane --certificate-key " + testCertificateKey
	cases := []struct {
		name     string
		joinInfo JoinInfo
		args     []string
		errMsg   string
		output   string
	}{
		{name: "short output", output: "TOKEN   TTL\n" + testToken + "   23h\n"},
		{name: "output at the limit", output: strings.Repeat("x", maxScriptLogOutputBytes-len(testToken)-1) + " " + testToken},
		{name: "output over the limit with a token across the cut", output: strings.Repeat("x", maxScriptLogOutputBytes-10) + " " + testToken + "\n"},
		{
			name:   "credentials passed as arguments, then printed alone",
			args:   []string{"--certificate-key", testCertificateKey, testToken},
			errMsg: "exit status 1: " + testCertificateKey,
			output: testCertificateKey + "\n",
		},
		{
			name:     "the certificate key the node holds, printed alone",
			joinInfo: JoinInfo{ControlPlaneJoinString: joinString},
			errMsg:   "exit status 1: " + testCertificateKey,
			output:   testCertificateKey + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("Doing: logging a script call")
			req := &pb.ExecuteScriptRequest{ScriptName: "rejoin.sh", Args: slices.Clone(tc.args)}
			log := jsonLog(func(logger *slog.Logger) {
				server := &NodeAgentServer{ScriptExecutionLogger: logger, NodeStatus: &NodeStatus{JoinInfo: tc.joinInfo}}
				server.logScriptExecution(context.Background(), "script execution finished", "req-1", req, "failed", 1, tc.errMsg, tc.output)
			})

			t.Log("Testing: the log holds no part of the token secret and no certificate key")
			assert.NotContains(t, log, testTokenID+"."+testTokenSecret[:1])
			assert.NotContains(t, log, testCertificateKey)
			assert.Equal(t, tc.args, req.GetArgs(), "the request itself must be untouched")
		})
	}
}

// jsonLog returns what write logs through slog's JSON handler.
func jsonLog(write func(*slog.Logger)) string {
	var buf bytes.Buffer
	write(slog.New(slog.NewJSONHandler(&buf, nil)))
	return buf.String()
}

// journalLines prefixes each line of logs as journalctl prints the agent's output.
func journalLines(logs string) []byte {
	var journal strings.Builder
	for line := range strings.Lines(logs) {
		journal.WriteString("Nov 19 17:16:12 gpu-01 nvke-agent[1234]: " + line)
	}
	return []byte(journal.String())
}

// writeHostFiles writes files, named by their absolute path on the node, under root.
func writeHostFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		hostPath := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(hostPath), 0o755))
		require.NoError(t, os.WriteFile(hostPath, []byte(content), 0o600))
	}
}

// supportBundleServer returns a server rooted at root whose journalctl prints journal.
func supportBundleServer(root string, journal []byte, joinInfo JoinInfo) *NodeAgentServer {
	journalArgs := []string{"-u", "nvke-agent", "--no-pager"}
	return &NodeAgentServer{
		RootPath: root,
		Runner: &MockRunner{
			Runners: []runner{runForCommand("journalctl", journalArgs, func(string, []string, *nvkeutils.CommandOptions) ([]byte, error) {
				return journal, nil
			})},
			Err: errors.New("unexpected command"),
		},
		NodeStatus: &NodeStatus{JoinInfo: joinInfo},
	}
}

// collectSupportBundle calls GetSupportBundle and returns the bundle's members by their base name.
func collectSupportBundle(t *testing.T, server *NodeAgentServer, additionalFiles []string) map[string]string {
	t.Helper()
	resp, err := server.GetSupportBundle(context.Background(), &pb.GetSupportBundleRequest{AdditionalFiles: additionalFiles})
	require.NoError(t, err)
	return untarSupportBundle(t, resp.GetBundleTgz())
}

// untarSupportBundle returns the regular files in a support bundle by their base name.
func untarSupportBundle(t *testing.T, tgz []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	require.NoError(t, err)
	reader := tar.NewReader(gz)
	members := map[string]string{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return members
		}
		require.NoError(t, err)
		if header.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(reader)
		require.NoError(t, err)
		members[path.Base(header.Name)] = string(content)
	}
}
