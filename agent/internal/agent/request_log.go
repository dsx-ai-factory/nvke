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

// The kubeadm init and join requests carry the cluster's etcd encryption key and
// the credentials a node joins with: the bootstrap token, and for a control
// plane node the certificate key that decrypts the control plane's certificates.
// Both requests are logged on arrival, and the journal is copied into support
// bundles, so the credentials are removed from the logged copy. The etcd key is
// replaced by its digest, logged beside it. A bootstrap token keeps its ID, which
// is not secret and identifies it in `kubeadm token list`.

// joinCredentials returns the credentials in a kubeadm join request, for
// redacting what is logged about the join.
func joinCredentials(req *pb.KubeadmJoinRequest) *redact.Credentials {
	credentials := redact.New()
	credentials.AddJoinString(req.GetKubeadmJoinString())
	credentials.AddConfig(req.GetJoinConfig())
	credentials.AddSecret(req.GetCertificateKey())
	return credentials
}

// initCredentials returns the credentials in a kubeadm init request, for
// redacting what is logged about the init.
func initCredentials(req *pb.KubeadmInitRequest) *redact.Credentials {
	credentials := redact.New()
	credentials.AddConfig(req.GetInitConfig())
	credentials.AddArgs(req.GetExtraArgs())
	return credentials
}

// loggableInitRequest returns req with the etcd encryption key removed and the
// credentials in its init configuration and extra arguments redacted. It returns
// req itself when there is nothing to remove.
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

// loggableJoinRequest returns req with the etcd encryption key removed and the
// join credentials redacted: the token and certificate key in the join command and
// the join configuration, and the certificate key field. It returns req itself
// when there is nothing to remove.
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
