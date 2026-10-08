// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentialaudit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func validEvent() Event {
	return Event{
		Timestamp: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		Action:    ActionRenew, Credential: TypeKubeadm, Plane: PlaneT2,
		Target: Target{ClusterID: uuid.NewString(), NodeID: uuid.NewString()},
		Actor:  Actor{Kind: ActorOperatorUnattributed}, Outcome: OutcomeSuccess, Reason: ReasonCompleted,
		Correlation: Correlation{OperationHash: Hash("operation", "example-run")},
	}
}

func TestNewHasStableSemanticIDAndDoesNotMutateInput(t *testing.T) {
	input := validEvent()
	input.Fingerprints = []string{strings.Repeat("A", 64), strings.Repeat("b", 64), strings.Repeat("a", 64)}
	first, err := New(input)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, first.SchemaVersion)
	require.Len(t, first.Fingerprints, 2)
	require.Equal(t, strings.Repeat("A", 64), input.Fingerprints[0])
	input.Timestamp = input.Timestamp.Add(time.Hour)
	second, err := New(input)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	input.Outcome, input.Reason = OutcomeFailure, ReasonFailed
	third, err := New(input)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, third.ID)
	input.Correlation.Attempt = 2
	fourth, err := New(input)
	require.NoError(t, err)
	require.NotEqual(t, third.ID, fourth.ID)
}

func TestNewRejectsUnallowlistedAndPrivateFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Event)
	}{
		{"raw output as action", func(e *Event) { e.Action = "example-token" }},
		{"unknown credential", func(e *Event) { e.Credential = "private-key" }},
		{"invalid plane", func(e *Event) { e.Plane = "unknown" }},
		{"unbounded outcome", func(e *Event) { e.Outcome = "example-token" }},
		{"raw error reason", func(e *Event) { e.Reason = "Authorization: example-token" }},
		{"raw error phase", func(e *Event) { e.Phase = "example-token" }},
		{"empty timestamp", func(e *Event) { e.Timestamp = time.Time{} }},
		{"unmanaged target value", func(e *Event) { e.Target.ClusterID = "example-token" }},
		{"nil target UUID", func(e *Event) { e.Target.ClusterID = uuid.Nil.String() }},
		{"ambiguous identity", func(e *Event) { e.Target.ClusterHash = Hash("cluster", "external") }},
		{"unknown actor", func(e *Event) { e.Actor.Kind = "someone" }},
		{"unverified actor subject", func(e *Event) { e.Actor.SubjectHash = Hash("actor", "claimed") }},
		{"raw subject", func(e *Event) { e.Actor = Actor{Kind: ActorOperatorReported, SubjectHash: "example-token"} }},
		{"raw workflow", func(e *Event) { e.Correlation.WorkflowHash = "example-token" }},
		{"negative attempt", func(e *Event) { e.Correlation.Attempt = -1 }},
		{"private key fingerprint", func(e *Event) {
			e.Credential = TypeEtcdKey
			e.Fingerprints = []string{Hash("secret", "REDACTED")}
		}},
		{"raw PEM fingerprint", func(e *Event) { e.Fingerprints = []string{"-----BEGIN PRIVATE KEY-----"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := validEvent()
			test.mutate(&event)
			_, err := New(event)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "example-token")
			require.NotContains(t, err.Error(), "REDACTED")
		})
	}
}

func TestEventEncodesOnlyAllowlistedFields(t *testing.T) {
	event, err := New(validEvent())
	require.NoError(t, err)
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	require.Len(t, object, 11)
	for _, key := range []string{"output", "error", "message", "secret", "metadata", "csr_name", "username"} {
		require.NotContains(t, object, key)
	}
}

func TestCertificateFingerprintsRejectsNonCertificateData(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	fingerprints, err := CertificateFingerprints(publicPEM)
	require.NoError(t, err)
	require.Len(t, fingerprints, 1)
	require.Len(t, fingerprints[0], 64)
	for _, input := range [][]byte{
		nil, []byte("example-token"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("REDACTED")}),
		append([]byte("example-token\n"), publicPEM...),
		append(publicPEM, []byte("example-token")...),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid DER")}),
	} {
		_, err := CertificateFingerprints(input)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "example-token")
	}
}
