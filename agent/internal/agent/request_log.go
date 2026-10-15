// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/dsx-ai-factory/nvke/agent/internal/redact"
	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// The kubeadm init and join requests carry the cluster's etcd encryption key, and
// both are logged on arrival. The journal is copied into support bundles, so the
// key is removed from the logged copy and its digest logged beside it. The join
// credentials in the requests are redacted from the copy as well.

// joinCredentials returns the credentials in a kubeadm join request.
func joinCredentials(req *pb.KubeadmJoinRequest) *redact.Credentials {
	credentials := redact.New()
	credentials.AddJoinString(req.GetKubeadmJoinString())
	credentials.AddConfig(req.GetJoinConfig())
	credentials.AddSecret(req.GetCertificateKey())
	return credentials
}

// initCredentials returns the credentials in a kubeadm init request.
func initCredentials(req *pb.KubeadmInitRequest) *redact.Credentials {
	credentials := redact.New()
	credentials.AddConfig(req.GetInitConfig())
	credentials.AddArgs(req.GetExtraArgs())
	return credentials
}

// loggableInitRequest returns req with the etcd encryption key removed and its
// credentials redacted. It returns req itself when there is nothing to change.
func loggableInitRequest(req *pb.KubeadmInitRequest) *pb.KubeadmInitRequest {
	credentials := initCredentials(req)
	initConfig := credentials.String(req.GetInitConfig())
	extraArgs := credentials.Args(req.GetExtraArgs())
	if req.GetApiEncryptionKey() == "" && initConfig == req.GetInitConfig() && slices.Equal(extraArgs, req.GetExtraArgs()) {
		return req
	}
	out, _ := proto.Clone(req).(*pb.KubeadmInitRequest)
	out.ApiEncryptionKey = ""
	out.InitConfig = initConfig
	out.ExtraArgs = extraArgs
	return out
}

// loggableJoinRequest returns req with the etcd encryption key removed and its
// credentials redacted. It returns req itself when there is nothing to change.
func loggableJoinRequest(req *pb.KubeadmJoinRequest) *pb.KubeadmJoinRequest {
	credentials := joinCredentials(req)
	joinString := credentials.String(req.GetKubeadmJoinString())
	joinConfig := credentials.String(req.GetJoinConfig())
	if req.GetApiEncryptionKey() == "" && req.GetCertificateKey() == "" &&
		joinString == req.GetKubeadmJoinString() && joinConfig == req.GetJoinConfig() {
		return req
	}
	out, _ := proto.Clone(req).(*pb.KubeadmJoinRequest)
	out.ApiEncryptionKey = ""
	if out.CertificateKey != "" {
		out.CertificateKey = redact.Redacted
	}
	out.KubeadmJoinString = joinString
	out.JoinConfig = joinConfig
	return out
}

// encryptionKeyDigest is the hex SHA-256 of key, or "" when there is none. It
// identifies a key without disclosing it, and matches the digest nvkectl logs
// for the key it saves.
func encryptionKeyDigest(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
