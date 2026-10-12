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
