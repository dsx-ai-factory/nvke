// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certificates

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type inspectionAuditStore struct {
	events []credentialaudit.Event
	err    error
}

func (s *inspectionAuditStore) RecordCredentialAuditEvent(_ context.Context, event credentialaudit.Event) error {
	s.events = append(s.events, event)
	return s.err
}

func TestInspectionAuditExcludesStaleAndOutOfScopeFingerprints(t *testing.T) {
	node := db.Node{ID: uuid.New(), ClusterID: uuid.New(), ControlPlane: true}
	inspection := &db.CertificateInspection{Available: true, AttemptedAt: time.Now().UTC(), Certificates: []db.CertificateObservation{
		{Credential: "apiserver", State: "available", CertificateSHA256: credentialaudit.Hash("certificate", "apiserver")},
		{Credential: "etcd-server", State: "available", CertificateSHA256: credentialaudit.Hash("certificate", "etcd-server")},
		{Credential: "kubernetes-ca", State: "available", CertificateSHA256: credentialaudit.Hash("certificate", "ca")},
		{Credential: "kubelet-client", State: "available", CertificateSHA256: credentialaudit.Hash("certificate", "client")},
		{Credential: "kubelet-serving", State: "available", CertificateSHA256: credentialaudit.Hash("certificate", "serving")},
		{Credential: "super-admin", State: "missing"},
	}}
	events := inspectionAuditEvents(node, inspection)
	require.Len(t, events, 2)
	require.Len(t, events[0].Fingerprints, 2)
	require.NotContains(t, events[0].Fingerprints, credentialaudit.Hash("certificate", "ca"))
	require.NotContains(t, events[0].Fingerprints, credentialaudit.Hash("certificate", "client"))
	require.Equal(t, credentialaudit.OutcomeSuccess, events[0].Outcome)
	require.Equal(t, []string{credentialaudit.Hash("certificate", "serving")}, events[1].Fingerprints)
	for _, event := range events {
		_, err := credentialaudit.New(event)
		require.NoError(t, err)
	}
	inspection.Available = false
	events = inspectionAuditEvents(node, inspection)
	for _, event := range events {
		require.Empty(t, event.Fingerprints)
		require.Equal(t, credentialaudit.OutcomeFailure, event.Outcome)
	}
	inspection.Available = true
	inspection.Certificates[0].State = "missing"
	events = inspectionAuditEvents(node, inspection)
	require.Equal(t, credentialaudit.OutcomeFailure, events[0].Outcome)
	// Worker checks never claim inspection of a kubeadm-managed control-plane bundle.
	node.ControlPlane = false
	events = inspectionAuditEvents(node, inspection)
	require.Len(t, events, 1)
	require.Equal(t, credentialaudit.TypeServing, events[0].Credential)
}

func TestInspectionAuditReportsSinkFailureWithoutChangingObservation(t *testing.T) {
	store := &inspectionAuditStore{err: errors.New("example-token")}
	var observations []string
	c := &Controller{audit: credentialaudit.NewRecorder(store, slog.New(slog.DiscardHandler), func(outcome string) {
		observations = append(observations, outcome)
	})}
	inspection := &db.CertificateInspection{AttemptedAt: time.Now().UTC(), Reason: "unsupported"}
	c.recordInspectionAudit(t.Context(), db.Node{ID: uuid.New(), ClusterID: uuid.New()}, inspection)
	require.Equal(t, []string{credentialaudit.WriteFailed}, observations)
	require.Equal(t, "unsupported", inspection.Reason)
	require.False(t, inspection.Available)
	require.Len(t, store.events, 1)
	require.Equal(t, credentialaudit.ReasonUnavailable, store.events[0].Reason)
}
