// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// The kubeadm init and join requests carry the cluster's etcd encryption key and
// the credentials a node joins with: the bootstrap token, and for a control
// plane node the certificate key that decrypts the control plane's certificates.
// Both requests are logged on arrival, and the journal is copied into support
// bundles, so the credentials are removed from the logged copy. The etcd key is
// replaced by its digest, logged beside it. A bootstrap token keeps its ID, which
// is not secret and identifies it in `kubeadm token list`.

// redacted replaces a credential in a logged value.
const redacted = "<redacted>"

var (
	// tokenFlag and certificateKeyFlag match kubeadm's flags and their values, as
	// "--token VALUE" or "--token=VALUE". kubeadm join also takes a bootstrap token
	// as --discovery-token and --tls-bootstrap-token, but not --token-ttl or
	// --discovery-token-ca-cert-hash, which are not credentials.
	tokenFlag          = regexp.MustCompile(`(--(?:token|discovery-token|tls-bootstrap-token)(?:=|\s+))(\S+)`)
	certificateKeyFlag = regexp.MustCompile(`(--certificate-key(?:=|\s+))(\S+)`)

	// configToken and configCertificateKey match the keys of a kubeadm
	// configuration that hold a credential, including in a list item.
	configToken          = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?(?:token|tlsBootstrapToken)\s*:\s*)(["']?)([^\s"'#]+)(["']?)`)
	configCertificateKey = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?certificateKey\s*:\s*)(["']?)([^\s"'#]+)(["']?)`)

	// printedCertificateKey matches the certificate key as kubeadm prints it, after
	// its label, and bareToken a bootstrap token printed on its own.
	printedCertificateKey = regexp.MustCompile(`(Using certificate key:\s*)[0-9a-fA-F]{64}`)
	bareToken             = regexp.MustCompile(`\b([a-z0-9]{6})\.[a-z0-9]{16}\b`)

	// tokenShape is a bootstrap token: a 6 character ID, a dot and a 16 character secret.
	tokenShape = regexp.MustCompile(`^([a-z0-9]{6})\.[a-z0-9]{16}$`)
)

// tokenFlagNames are the kubeadm join flags whose value is a bootstrap token.
var tokenFlagNames = map[string]bool{"--token": true, "--discovery-token": true, "--tls-bootstrap-token": true}

// redactToken returns token with its secret part replaced and its ID kept, or the
// whole value replaced if it is not a bootstrap token. It leaves an already
// redacted value as it is.
func redactToken(token string) string {
	if m := tokenShape.FindStringSubmatch(token); m != nil {
		return m[1] + "." + redacted
	}
	if token == redacted || strings.HasSuffix(token, "."+redacted) {
		return token
	}
	return redacted
}

// redactFlag replaces the value in each match of re, which has the flag and its
// separator as its first group and the value as its second.
func redactFlag(s string, re *regexp.Regexp, redact func(string) string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		groups := re.FindStringSubmatch(match)
		return groups[1] + redact(groups[2])
	})
}

// redactJoinString returns a kubeadm join command with the values of --token and
// --certificate-key redacted. The rest of the command is unchanged.
func redactJoinString(s string) string {
	s = redactFlag(s, tokenFlag, redactToken)
	return redactFlag(s, certificateKeyFlag, func(string) string { return redacted })
}

// redactJoinArgs returns the arguments of a kubeadm join command with the values of
// --token and --certificate-key redacted. The input is not modified.
func redactJoinArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, arg := range out {
		name, value, hasValue := strings.Cut(arg, "=")
		switch {
		case tokenFlagNames[name] && hasValue:
			out[i] = name + "=" + redactToken(value)
		case tokenFlagNames[arg] && i+1 < len(out):
			out[i+1] = redactToken(out[i+1])
		case name == "--certificate-key" && hasValue:
			out[i] = name + "=" + redacted
		case arg == "--certificate-key" && i+1 < len(out):
			out[i+1] = redacted
		}
	}
	return out
}

// redactKubeadmConfig returns a kubeadm configuration with the values of its token,
// tlsBootstrapToken and certificateKey keys redacted. The rest is unchanged.
func redactKubeadmConfig(config string) string {
	config = configToken.ReplaceAllStringFunc(config, func(match string) string {
		g := configToken.FindStringSubmatch(match)
		return g[1] + g[2] + redactToken(g[3]) + g[4]
	})
	return configCertificateKey.ReplaceAllStringFunc(config, func(match string) string {
		g := configCertificateKey.FindStringSubmatch(match)
		return g[1] + g[2] + redacted + g[4]
	})
}

// redactKubeadmOutput returns the output of a kubeadm command with the credentials
// it prints redacted: the values of --token and --certificate-key in a join
// command, the certificate key after its label, and a bootstrap token on its own.
func redactKubeadmOutput(output string) string {
	output = redactJoinString(output)
	output = printedCertificateKey.ReplaceAllString(output, "${1}"+redacted)
	return bareToken.ReplaceAllString(output, "${1}."+redacted)
}

// loggableInitRequest returns req with the etcd encryption key removed and the
// credentials in its init configuration redacted. It returns req itself when
// there is nothing to remove.
func loggableInitRequest(req *pb.KubeadmInitRequest) *pb.KubeadmInitRequest {
	initConfig := redactKubeadmConfig(req.GetInitConfig())
	if req.GetApiEncryptionKey() == "" && initConfig == req.GetInitConfig() {
		return req
	}
	out, _ := proto.Clone(req).(*pb.KubeadmInitRequest)
	out.ApiEncryptionKey = ""
	out.InitConfig = initConfig
	return out
}

// loggableJoinRequest returns req with the etcd encryption key removed and the
// join credentials redacted: the token and certificate key in the join command and
// the join configuration, and the certificate key field. It returns req itself
// when there is nothing to remove.
func loggableJoinRequest(req *pb.KubeadmJoinRequest) *pb.KubeadmJoinRequest {
	joinString := redactJoinString(req.GetKubeadmJoinString())
	joinConfig := redactKubeadmConfig(req.GetJoinConfig())
	if req.GetApiEncryptionKey() == "" && req.GetCertificateKey() == "" &&
		joinString == req.GetKubeadmJoinString() && joinConfig == req.GetJoinConfig() {
		return req
	}
	out, _ := proto.Clone(req).(*pb.KubeadmJoinRequest)
	out.ApiEncryptionKey = ""
	if out.CertificateKey != "" {
		out.CertificateKey = redacted
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
