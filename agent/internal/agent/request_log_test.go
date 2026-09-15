// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/NVIDIA/nke/agent/pkg/agent/proto"
)

const testEncryptionKey = "KEY-MATERIAL-THAT-MUST-NOT-BE-LOGGED"

func TestLoggableRequestsDropTheEncryptionKey(t *testing.T) {
	t.Run("init", func(t *testing.T) {
		req := &pb.KubeadmInitRequest{
			InitConfig:           "config",
			ApiEncryptionEnabled: true,
			ApiEncryptionKey:     testEncryptionKey,
		}
		logged := loggableInitRequest(req)

		assert.Empty(t, logged.ApiEncryptionKey)
		assert.Equal(t, "config", logged.InitConfig)
		assert.True(t, logged.ApiEncryptionEnabled)
		assert.Equal(t, testEncryptionKey, req.ApiEncryptionKey, "the request itself must be untouched")
		assertNotLogged(t, "issuing kubeadm init call", logged)
	})

	t.Run("join", func(t *testing.T) {
		req := &pb.KubeadmJoinRequest{
			KubeadmJoinString: "join-string",
			ApiEncryptionKey:  testEncryptionKey,
		}
		logged := loggableJoinRequest(req)

		assert.Empty(t, logged.ApiEncryptionKey)
		assert.Equal(t, "join-string", logged.KubeadmJoinString)
		assert.Equal(t, testEncryptionKey, req.ApiEncryptionKey, "the request itself must be untouched")
		assertNotLogged(t, "issuing kubeadm join call", logged)
	})

	t.Run("no key is a no-op", func(t *testing.T) {
		req := &pb.KubeadmInitRequest{InitConfig: "config"}
		assert.Same(t, req, loggableInitRequest(req))
	})
}

// assertNotLogged renders request the way the agent's JSON slog handler does
// and checks the key is not in the output.
func assertNotLogged(t *testing.T, msg string, request any) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info(msg, "request", request)

	require.True(t, json.Valid(buf.Bytes()), "log line is not JSON: %s", buf.String())
	assert.NotContains(t, buf.String(), testEncryptionKey)
	assert.NotContains(t, buf.String(), "api_encryption_key\"", "the field should be absent, not empty")
}

func TestEncryptionKeyDigest(t *testing.T) {
	assert.Empty(t, encryptionKeyDigest(""))

	digest := encryptionKeyDigest(testEncryptionKey)
	assert.Len(t, digest, 64)
	assert.Equal(t, digest, encryptionKeyDigest(testEncryptionKey), "stable")
	assert.NotContains(t, digest, testEncryptionKey)
}
