// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"crypto/sha256"
	"encoding/hex"

	"google.golang.org/protobuf/proto"

	pb "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"
)

// The kubeadm init and join requests carry the cluster's etcd encryption key,
// and both are logged on arrival. The journal is copied into support bundles,
// so the key is removed from the logged copy and its digest logged beside it.

// loggableInitRequest returns req with the etcd encryption key removed.
func loggableInitRequest(req *pb.KubeadmInitRequest) *pb.KubeadmInitRequest {
	if req.GetApiEncryptionKey() == "" {
		return req
	}
	out, _ := proto.Clone(req).(*pb.KubeadmInitRequest)
	out.ApiEncryptionKey = ""
	return out
}

// loggableJoinRequest returns req with the etcd encryption key removed.
func loggableJoinRequest(req *pb.KubeadmJoinRequest) *pb.KubeadmJoinRequest {
	if req.GetApiEncryptionKey() == "" {
		return req
	}
	out, _ := proto.Clone(req).(*pb.KubeadmJoinRequest)
	out.ApiEncryptionKey = ""
	return out
}

// encryptionKeyDigest is the hex SHA-256 of key, or "" when there is none. It
// identifies a key without disclosing it, and matches the digest nkectl logs
// for the key it saves.
func encryptionKeyDigest(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
