// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	"github.com/dsx-ai-factory/nvke/agent/pkg/certificates"
)

func inspectionTestCertificate(t *testing.T, expiry time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: expiry.Add(-365 * 24 * time.Hour), NotAfter: expiry}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestInspectKubernetesCertificatesReadOnlyInventory(t *testing.T) {
	expiry := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	data := inspectionTestCertificate(t, expiry)
	for _, test := range []struct {
		name    string
		content []byte
		state   pb.KubernetesCertificateObservation_State
	}{
		{"expired certificate remains observable", data, pb.KubernetesCertificateObservation_STATE_AVAILABLE},
		{"invalid PEM", []byte("invalid"), pb.KubernetesCertificateObservation_STATE_INVALID},
		{"missing", nil, pb.KubernetesCertificateObservation_STATE_MISSING},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "var/lib/kubelet/pki/kubelet-client-current.pem")
			if test.content != nil {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, test.content, 0o600))
			}
			server := &NodeAgentServer{RootPath: root}
			response, err := server.InspectKubernetesCertificates(context.Background(), &pb.InspectKubernetesCertificatesRequest{})
			require.NoError(t, err)
			require.Len(t, response.Certificates, 2)
			require.Equal(t, "kubelet-client", response.Certificates[0].Credential)
			require.Equal(t, test.state, response.Certificates[0].State)
			if test.state == pb.KubernetesCertificateObservation_STATE_AVAILABLE {
				require.Equal(t, expiry.Unix(), response.Certificates[0].NotAfterUnixSeconds)
			}
			if test.content != nil {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, test.content, after)
			}
		})
	}
}

func TestInspectKubeconfigEmbeddedCertificateAndOptionalSuperAdmin(t *testing.T) {
	root := t.TempDir()
	expiry := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	cert := inspectionTestCertificate(t, expiry)
	config := "apiVersion: v1\nkind: Config\ncurrent-context: current\ncontexts:\n- name: current\n  context:\n    user: admin\nusers:\n- name: admin\n  user:\n    client-certificate-data: " + base64.StdEncoding.EncodeToString(cert) + "\n"
	path := filepath.Join(root, "etc/kubernetes/admin.conf")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
	server := &NodeAgentServer{RootPath: root}
	response, err := server.InspectKubernetesCertificates(context.Background(), &pb.InspectKubernetesCertificatesRequest{ControlPlane: true})
	require.NoError(t, err)
	require.Len(t, response.Certificates, len(certificates.Names(true)))
	for _, item := range response.Certificates {
		if item.Credential == "admin" {
			require.Equal(t, pb.KubernetesCertificateObservation_STATE_AVAILABLE, item.State)
			require.Equal(t, expiry.Unix(), item.NotAfterUnixSeconds)
		}
		if item.Credential == "super-admin" {
			require.Equal(t, pb.KubernetesCertificateObservation_STATE_MISSING, item.State)
			require.False(t, certificates.Required(item.Credential))
		}
	}
}

func TestInspectionBoundsAndCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "oversized")
	require.NoError(t, os.WriteFile(path, make([]byte, maxInspectedCredentialBytes+1), 0o600))
	_, err := readBoundedCredential(path)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (&NodeAgentServer{RootPath: root}).InspectKubernetesCertificates(ctx, &pb.InspectKubernetesCertificatesRequest{})
	require.Equal(t, codes.Canceled, status.Code(err))
}

func TestEarliestExpiryInCertificateChain(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	first := inspectionTestCertificate(t, now.Add(48*time.Hour))
	second := inspectionTestCertificate(t, now.Add(time.Hour))
	expiry, fingerprint, err := earliestCertificateMetadata(append(first, second...))
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Hour).Unix(), expiry)
	require.Len(t, fingerprint, 64)
}
