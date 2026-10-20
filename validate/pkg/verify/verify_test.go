// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDigest = "49647c6b3bce4b808121bb0c100e3e87f5b86ba849d36092b53f93ab2ca66b32"

var testFinished = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func testStatement(t *testing.T, mutate func(*Statement)) []byte {
	t.Helper()
	st := Statement{
		Type:          StatementType,
		Subject:       []Subject{{Name: "nvke-node-fde-ubuntu-26.04.1-amd64.qcow2", Digest: map[string]string{"sha256": testDigest}}},
		PredicateType: PredicateType,
		Predicate: Predicate{
			Outcome:             OutcomePass,
			CheckSetVersion:     "0.2.0",
			ReportSchemaVersion: "1",
			Validator:           Tool{Name: "nvke-image-validate", Version: "v0.0.0-test"},
			Target:              Target{Variant: "fde", Arch: "amd64", Ubuntu: "26.04.1"},
			Summary:             Summary{Pass: 11, Skip: 4},
			Results:             []CheckResult{{ID: "INT-001", Status: "pass", Severity: "blocking", Evidence: "sha256 matches"}},
			FinishedAt:          testFinished,
		},
	}
	if mutate != nil {
		mutate(&st)
	}
	raw, err := json.Marshal(st)
	require.NoError(t, err)
	return raw
}

func ed25519Signer(t *testing.T) (crypto.Signer, crypto.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return priv, pub
}

func ecdsaSigner(t *testing.T) (crypto.Signer, crypto.PublicKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return priv, &priv.PublicKey
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	for name, gen := range map[string]func(*testing.T) (crypto.Signer, crypto.PublicKey){
		"ed25519":    ed25519Signer,
		"ecdsa-p256": ecdsaSigner,
	} {
		t.Run(name, func(t *testing.T) {
			signer, pub := gen(t)
			env, err := Sign(testStatement(t, nil), signer)
			require.NoError(t, err)

			st, err := Verify(env, testDigest, Policy{Keys: []crypto.PublicKey{pub}}, testFinished)
			require.NoError(t, err)
			assert.Equal(t, OutcomePass, st.Predicate.Outcome)
			assert.Equal(t, "0.2.0", st.Predicate.CheckSetVersion)
			assert.Equal(t, testDigest, st.Subject[0].Digest["sha256"])
		})
	}
}

func TestVerifyRejects(t *testing.T) {
	signer, pub := ed25519Signer(t)
	otherSigner, otherPub := ed25519Signer(t)
	_ = otherSigner
	goodEnv, err := Sign(testStatement(t, nil), signer)
	require.NoError(t, err)
	keys := Policy{Keys: []crypto.PublicKey{pub}}

	t.Run("no keys in the policy", func(t *testing.T) {
		_, err := Verify(goodEnv, testDigest, Policy{}, testFinished)
		require.ErrorContains(t, err, "no verification keys")
	})
	t.Run("wrong key", func(t *testing.T) {
		_, err := Verify(goodEnv, testDigest, Policy{Keys: []crypto.PublicKey{otherPub}}, testFinished)
		require.ErrorContains(t, err, "no envelope signature verifies")
	})
	t.Run("tampered payload", func(t *testing.T) {
		var env Envelope
		require.NoError(t, json.Unmarshal(goodEnv, &env))
		tampered := testStatement(t, func(st *Statement) { st.Predicate.Outcome = OutcomeFail })
		resigned, err := Sign(tampered, signer)
		require.NoError(t, err)
		var tamperedEnv Envelope
		require.NoError(t, json.Unmarshal(resigned, &tamperedEnv))
		// Splice the tampered payload under the original signature.
		env.Payload = tamperedEnv.Payload
		spliced, err := json.Marshal(env)
		require.NoError(t, err)
		_, err = Verify(spliced, testDigest, keys, testFinished)
		require.ErrorContains(t, err, "no envelope signature verifies")
	})
	t.Run("digest mismatch", func(t *testing.T) {
		_, err := Verify(goodEnv, strings.Repeat("0", 64), keys, testFinished)
		require.ErrorContains(t, err, "no statement subject carries")
	})
	t.Run("wrong predicate type", func(t *testing.T) {
		env, err := Sign(testStatement(t, func(st *Statement) { st.PredicateType = "https://example.com/other/v1" }), signer)
		require.NoError(t, err)
		_, err = Verify(env, testDigest, keys, testFinished)
		require.ErrorContains(t, err, "predicate type")
	})
	t.Run("unknown outcome", func(t *testing.T) {
		env, err := Sign(testStatement(t, func(st *Statement) { st.Predicate.Outcome = "maybe" }), signer)
		require.NoError(t, err)
		_, err = Verify(env, testDigest, keys, testFinished)
		require.ErrorContains(t, err, "outcome")
	})
	t.Run("check-set version below the floor", func(t *testing.T) {
		pol := keys
		pol.MinCheckSetVersion = "0.3.0"
		_, err := Verify(goodEnv, testDigest, pol, testFinished)
		require.ErrorContains(t, err, "below the required minimum")
	})
	t.Run("check-set version at the floor passes", func(t *testing.T) {
		pol := keys
		pol.MinCheckSetVersion = "0.2"
		_, err := Verify(goodEnv, testDigest, pol, testFinished)
		require.NoError(t, err)
	})
	t.Run("stale verdict", func(t *testing.T) {
		pol := keys
		pol.MaxAge = time.Hour
		_, err := Verify(goodEnv, testDigest, pol, testFinished.Add(2*time.Hour))
		require.ErrorContains(t, err, "old")
	})
	t.Run("fresh verdict passes the age policy", func(t *testing.T) {
		pol := keys
		pol.MaxAge = time.Hour
		_, err := Verify(goodEnv, testDigest, pol, testFinished.Add(30*time.Minute))
		require.NoError(t, err)
	})
	t.Run("fail outcome still verifies", func(t *testing.T) {
		env, err := Sign(testStatement(t, func(st *Statement) { st.Predicate.Outcome = OutcomeFail }), signer)
		require.NoError(t, err)
		st, err := Verify(env, testDigest, keys, testFinished)
		require.NoError(t, err, "a failing verdict is authentic evidence; outcome policy is the caller's")
		assert.Equal(t, OutcomeFail, st.Predicate.Outcome)
	})
}

func TestPEMRoundTrip(t *testing.T) {
	signer, pub := ecdsaSigner(t)
	der, err := x509.MarshalPKCS8PrivateKey(signer.(*ecdsa.PrivateKey))
	require.NoError(t, err)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	loaded, err := LoadSigner(privPEM)
	require.NoError(t, err)
	keys, err := LoadPublicKeys(pubPEM)
	require.NoError(t, err)

	env, err := Sign(testStatement(t, nil), loaded)
	require.NoError(t, err)
	_, err = Verify(env, testDigest, Policy{Keys: keys}, testFinished)
	require.NoError(t, err)
}

func TestLoadRejectsUnsupportedKeys(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	require.NoError(t, err)
	_, err = LoadSigner(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	require.ErrorContains(t, err, "unsupported signing key type")

	pubDER, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	require.NoError(t, err)
	_, err = LoadPublicKeys(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	require.ErrorContains(t, err, "unsupported verification key type")

	_, err = LoadPublicKeys([]byte("not pem"))
	require.ErrorContains(t, err, "no PUBLIC KEY PEM blocks")
}

func TestVersionLess(t *testing.T) {
	for _, tt := range []struct {
		a, b    string
		want    bool
		wantErr bool
	}{
		{a: "0.1.0", b: "0.2.0", want: true},
		{a: "0.2.0", b: "0.2.0", want: false},
		{a: "0.10.0", b: "0.9.0", want: false},
		{a: "0.2", b: "0.2.0", want: false},
		{a: "1.0.0-rc.1", b: "1.0.0", wantErr: true},
	} {
		got, err := versionLess(tt.a, tt.b)
		if tt.wantErr {
			require.Error(t, err, "%s < %s", tt.a, tt.b)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tt.want, got, "%s < %s", tt.a, tt.b)
	}
}
