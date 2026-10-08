// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package csr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type csrAuditStore struct {
	events []credentialaudit.Event
	err    error
}

func (s *csrAuditStore) RecordCredentialAuditEvent(_ context.Context, event credentialaudit.Event) error {
	s.events = append(s.events, event)
	return s.err
}

func TestCSRAuditPreservesApprovalOnSinkFailureAndOmitsUntrustedNames(t *testing.T) {
	clusterID, nodeID := uuid.New(), uuid.New()
	csr := newServingCSR(t, "example-token", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	csr.UID = types.UID(uuid.NewString())
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(csr).Build()
	store := &csrAuditStore{err: errors.New("example-token")}
	r := &Reconciler{
		clusterID: clusterID, database: staticDatabase{nodes: []db.Node{{ID: nodeID, Name: "worker-1", IP: "192.0.2.10"}}},
		client: tenantClient, logger: slog.New(slog.DiscardHandler), audit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil),
	}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
	require.NoError(t, err)
	approved := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(t.Context(), types.NamespacedName{Name: csr.Name}, approved))
	require.Len(t, approved.Status.Conditions, 1)
	require.Len(t, store.events, 2)
	require.Equal(t, credentialaudit.ActionCheck, store.events[0].Action)
	require.Equal(t, credentialaudit.ActionApprove, store.events[1].Action)
	require.Equal(t, store.events[0].Correlation.OperationHash, store.events[1].Correlation.OperationHash)
	for _, event := range store.events {
		require.Equal(t, nodeID.String(), event.Target.NodeID)
		encoded, err := json.Marshal(event)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "example-token")
		require.NotContains(t, string(encoded), "worker-1")
	}
}

func TestCSRAuditObservesIssuedPublicCertificateWithoutReapproving(t *testing.T) {
	clusterID, nodeID := uuid.New(), uuid.New()
	csr := newServingCSR(t, "issued-serving", time.Now(), "system:node:worker-1", []string{"system:nodes"}, []string{"worker-1"}, nil, nil)
	block, _ := pem.Decode(csr.Spec.Request)
	require.NotNil(t, block)
	request, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: request.Subject,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, request.PublicKey, key)
	require.NoError(t, err)
	csr.Status.Certificate = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	csr.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue}}
	require.True(t, kubeletServingCSRPredicate().Create(ctrlevent.CreateEvent{Object: csr}))
	scheme := runtime.NewScheme()
	require.NoError(t, certificatesv1.AddToScheme(scheme))
	tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(csr).Build()
	store := &csrAuditStore{}
	r := &Reconciler{
		clusterID: clusterID, database: staticDatabase{nodes: []db.Node{{ID: nodeID, Name: "worker-1", IP: "192.0.2.10"}}},
		client: tenantClient, logger: slog.New(slog.DiscardHandler), audit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil),
	}
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
	require.NoError(t, err)
	require.Len(t, store.events, 1)
	require.Equal(t, credentialaudit.ActionVerify, store.events[0].Action)
	require.Equal(t, credentialaudit.ReasonObserved, store.events[0].Reason)
	fingerprints, err := credentialaudit.CertificateFingerprints(csr.Status.Certificate)
	require.NoError(t, err)
	require.Equal(t, fingerprints, store.events[0].Fingerprints)
	unchanged := &certificatesv1.CertificateSigningRequest{}
	require.NoError(t, tenantClient.Get(t.Context(), types.NamespacedName{Name: csr.Name}, unchanged))
	require.Len(t, unchanged.Status.Conditions, 1)
}

func TestCSRAuditRecordsSkipsOnlyForManagedNodes(t *testing.T) {
	tests := map[string]struct {
		username   string
		dnsNames   []string
		wantEvents int
	}{
		"non-node requestor is not audited":     {username: "example-user", dnsNames: []string{"worker-1"}},
		"unknown node requestor is not audited": {username: "system:node:worker-9", dnsNames: []string{"worker-9"}},
		"managed node SAN mismatch is audited":  {username: "system:node:worker-1", dnsNames: []string{"other"}, wantEvents: 1},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			nodeID := uuid.New()
			csr := newServingCSR(t, "skipped", time.Now(), test.username, []string{"system:nodes"}, test.dnsNames, nil, nil)
			csr.UID = types.UID(uuid.NewString())
			csr.Spec.Username = test.username
			scheme := runtime.NewScheme()
			require.NoError(t, certificatesv1.AddToScheme(scheme))
			tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(csr).Build()
			store := &csrAuditStore{}
			r := &Reconciler{
				clusterID: uuid.New(), database: staticDatabase{nodes: []db.Node{{ID: nodeID, Name: "worker-1", IP: "192.0.2.10"}}},
				client: tenantClient, logger: slog.New(slog.DiscardHandler), audit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), nil),
			}
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
			require.NoError(t, err)
			require.Len(t, store.events, test.wantEvents)
			for _, event := range store.events {
				require.Equal(t, credentialaudit.OutcomeNoop, event.Outcome)
				require.Equal(t, credentialaudit.ReasonRejected, event.Reason)
				require.Equal(t, nodeID.String(), event.Target.NodeID)
			}
			unchanged := &certificatesv1.CertificateSigningRequest{}
			require.NoError(t, tenantClient.Get(t.Context(), types.NamespacedName{Name: csr.Name}, unchanged))
			require.Empty(t, unchanged.Status.Conditions)
		})
	}
}
