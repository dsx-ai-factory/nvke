// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/clientcmd"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/certificates"
)

const maxInspectedCredentialBytes = 1024 * 1024

// InspectKubernetesCertificates observes certificates without running commands or writing files.
func (s *NodeAgentServer) InspectKubernetesCertificates(ctx context.Context, req *pb.InspectKubernetesCertificatesRequest) (*pb.InspectKubernetesCertificatesResponse, error) {
	response := &pb.InspectKubernetesCertificatesResponse{}
	for _, name := range certificates.Names(req.GetControlPlane()) {
		if err := ctx.Err(); err != nil {
			return nil, status.Error(codes.Canceled, "certificate inspection cancelled")
		}
		expiry, fingerprint, err := s.inspectCertificate(name)
		observation := &pb.KubernetesCertificateObservation{Credential: name, State: pb.KubernetesCertificateObservation_STATE_AVAILABLE, NotAfterUnixSeconds: expiry, CertificateSha256: fingerprint}
		if errors.Is(err, os.ErrNotExist) {
			observation.State = pb.KubernetesCertificateObservation_STATE_MISSING
		} else if err != nil {
			observation.State = pb.KubernetesCertificateObservation_STATE_INVALID
		}
		response.Certificates = append(response.Certificates, observation)
	}
	return response, nil
}

func (s *NodeAgentServer) inspectCertificate(name string) (int64, string, error) {
	paths := map[string]string{
		"kubelet-client": "/var/lib/kubelet/pki/kubelet-client-current.pem", "kubelet-serving": "/var/lib/kubelet/pki/kubelet-server-current.pem",
		"apiserver": "/etc/kubernetes/pki/apiserver.crt", "apiserver-kubelet-client": "/etc/kubernetes/pki/apiserver-kubelet-client.crt",
		"front-proxy-client": "/etc/kubernetes/pki/front-proxy-client.crt", "apiserver-etcd-client": "/etc/kubernetes/pki/apiserver-etcd-client.crt",
		"etcd-server": "/etc/kubernetes/pki/etcd/server.crt", "etcd-peer": "/etc/kubernetes/pki/etcd/peer.crt", "etcd-healthcheck-client": "/etc/kubernetes/pki/etcd/healthcheck-client.crt",
		"kubernetes-ca": "/etc/kubernetes/pki/ca.crt", "front-proxy-ca": "/etc/kubernetes/pki/front-proxy-ca.crt", "etcd-ca": "/etc/kubernetes/pki/etcd/ca.crt",
	}
	path, ok := paths[name]
	if !ok {
		path = "/etc/kubernetes/" + name + ".conf"
	}
	data, err := readBoundedCredential(s.rootedPath(path))
	if err != nil {
		return 0, "", err
	}
	if !ok {
		cfg, err := clientcmd.Load(data)
		if err != nil {
			return 0, "", err
		}
		current, ok := cfg.Contexts[cfg.CurrentContext]
		if !ok {
			return 0, "", errors.New("missing current kubeconfig context")
		}
		user, ok := cfg.AuthInfos[current.AuthInfo]
		if !ok {
			return 0, "", errors.New("missing kubeconfig client")
		}
		data = user.ClientCertificateData
		if len(data) == 0 {
			// Support the standard kubelet symlink only. Arbitrary references in a
			// kubeconfig must not widen this read-only RPC's fixed inventory.
			certPath := filepath.Clean(user.ClientCertificate)
			if certPath != "/var/lib/kubelet/pki/kubelet-client-current.pem" {
				return 0, "", errors.New("unsupported kubeconfig certificate reference")
			}
			data, err = readBoundedCredential(s.rootedPath(certPath))
			if err != nil {
				return 0, "", err
			}
		}
	}
	return earliestCertificateMetadata(data)
}

func readBoundedCredential(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("credential is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxInspectedCredentialBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInspectedCredentialBytes {
		return nil, errors.New("credential exceeds inspection limit")
	}
	return data, nil
}

func earliestCertificateMetadata(data []byte) (int64, string, error) {
	var earliest int64
	var fingerprint string
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		data = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return 0, "", err
		}
		expiry := cert.NotAfter.Unix()
		if expiry <= 0 {
			return 0, "", errors.New("invalid certificate expiration")
		}
		if earliest == 0 || expiry < earliest {
			earliest = expiry
			hash := sha256.Sum256(cert.Raw)
			fingerprint = hex.EncodeToString(hash[:])
		}
	}
	if earliest == 0 {
		return 0, "", errors.New("no certificate in credential")
	}
	return earliest, fingerprint, nil
}
