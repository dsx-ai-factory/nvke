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
	"encoding/base64"
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

func TestRequiredChecksProfile(t *testing.T) {
	signer, pub := ed25519Signer(t)
	keys := []crypto.PublicKey{pub}
	results := []CheckResult{
		{ID: "INT-001", Status: "pass", Severity: "blocking"},
		{ID: "INT-002", Status: "skip", Severity: "blocking", Reason: "input-missing", Evidence: "digest-only mode"},
		{ID: "FDE-001", Status: "skip", Severity: "blocking", Reason: SkipNotApplicable, Evidence: "target outside appliesTo"},
		{ID: "HARD-001", Status: "skip", Severity: "blocking", Reason: "tier-off"},
		{ID: "OLD-001", Status: "skip", Severity: "blocking"}, // pre-reason verdict
		{ID: "SPEC-005", Status: "fail", Severity: "blocking", Evidence: "timeout is 120"},
	}
	env, err := Sign(testStatement(t, func(st *Statement) { st.Predicate.Results = results }), signer)
	require.NoError(t, err)

	tests := []struct {
		name     string
		required []string
		wantErr  string
	}{
		{name: "no profile keeps old behavior", required: nil},
		{name: "passing required check accepted", required: []string{"INT-001"}},
		{name: "not-applicable skip satisfies the profile", required: []string{"INT-001", "FDE-001"}},
		{name: "input-missing skip is omitted validation", required: []string{"INT-002"}, wantErr: "omitted validation"},
		{name: "tier-off skip is omitted validation", required: []string{"HARD-001"}, wantErr: "omitted validation"},
		{name: "reasonless skip is omitted validation", required: []string{"OLD-001"}, wantErr: "no reason recorded"},
		{name: "absent result is rejected", required: []string{"INT-404"}, wantErr: "no result for it"},
		{name: "failed required check is rejected", required: []string{"SPEC-005"}, wantErr: "status fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Verify(env, testDigest, Policy{Keys: keys, RequiredChecks: tt.required}, testFinished)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestPredicateBindingFieldsRoundTrip(t *testing.T) {
	signer, pub := ed25519Signer(t)
	env, err := Sign(testStatement(t, func(st *Statement) {
		st.Predicate.EvidenceMode = EvidenceModeDigestOnly
		st.Predicate.CheckSetSHA256 = strings.Repeat("a", 64)
		st.Predicate.BOMSHA256 = strings.Repeat("b", 64)
	}), signer)
	require.NoError(t, err)
	st, err := Verify(env, testDigest, Policy{Keys: []crypto.PublicKey{pub}}, testFinished)
	require.NoError(t, err)
	assert.Equal(t, EvidenceModeDigestOnly, st.Predicate.EvidenceMode)
	assert.Equal(t, strings.Repeat("a", 64), st.Predicate.CheckSetSHA256)
	assert.Equal(t, strings.Repeat("b", 64), st.Predicate.BOMSHA256)
}

func TestPolicyPinsCheckSetAndTarget(t *testing.T) {
	signer, pub := ed25519Signer(t)
	keys := []crypto.PublicKey{pub}
	approved := strings.Repeat("a", 64)
	// Every result not-applicable: what a registry whose selectors never
	// match the target produces (e.g. every appliesTo.arch misspelled).
	allNA := func(st *Statement) {
		st.Predicate.CheckSetSHA256 = strings.Repeat("f", 64)
		st.Predicate.Results = []CheckResult{
			{ID: "INT-001", Status: "skip", Severity: "blocking", Reason: SkipNotApplicable},
			{ID: "INT-002", Status: "skip", Severity: "blocking", Reason: SkipNotApplicable},
		}
	}
	naEnv, err := Sign(testStatement(t, allNA), signer)
	require.NoError(t, err)
	goodEnv, err := Sign(testStatement(t, func(st *Statement) { st.Predicate.CheckSetSHA256 = approved }), signer)
	require.NoError(t, err)

	// Documented trust boundary: without check-set pinning, applicability is
	// the producer's — an all-not-applicable verdict satisfies any profile.
	_, err = Verify(naEnv, testDigest, Policy{Keys: keys, RequiredChecks: []string{"INT-001", "INT-002"}}, testFinished)
	require.NoError(t, err)

	// Pinning closes it: the misselected registry's digest is not approved.
	_, err = Verify(naEnv, testDigest, Policy{Keys: keys, RequiredChecks: []string{"INT-001"}, CheckSetSHA256: []string{approved}}, testFinished)
	require.ErrorContains(t, err, "not an approved one")

	// The approved registry passes, including case-insensitively.
	_, err = Verify(goodEnv, testDigest, Policy{Keys: keys, CheckSetSHA256: []string{strings.ToUpper(approved)}}, testFinished)
	require.NoError(t, err)

	// A pre-field verdict (empty checkSetSha256) never satisfies a pin.
	oldEnv, err := Sign(testStatement(t, nil), signer)
	require.NoError(t, err)
	_, err = Verify(oldEnv, testDigest, Policy{Keys: keys, CheckSetSHA256: []string{approved}}, testFinished)
	require.ErrorContains(t, err, "not an approved one")

	// Expected target: any mismatching field rejects; matching passes.
	_, err = Verify(goodEnv, testDigest, Policy{Keys: keys, ExpectedTarget: Target{Arch: "arm64"}}, testFinished)
	require.ErrorContains(t, err, `target arch is "amd64"`)
	_, err = Verify(goodEnv, testDigest, Policy{Keys: keys, ExpectedTarget: Target{Variant: "fde", Arch: "amd64", Ubuntu: "26.04.1"}}, testFinished)
	require.NoError(t, err)
}

func TestDuplicateResultIDsAreRejected(t *testing.T) {
	signer, pub := ed25519Signer(t)
	env, err := Sign(testStatement(t, func(st *Statement) {
		st.Predicate.Results = []CheckResult{
			{ID: "INT-001", Status: "fail", Severity: "blocking"},
			{ID: "INT-001", Status: "pass", Severity: "blocking"},
		}
	}), signer)
	require.NoError(t, err)
	_, err = Verify(env, testDigest, Policy{Keys: []crypto.PublicKey{pub}, RequiredChecks: []string{"INT-001"}}, testFinished)
	require.ErrorContains(t, err, "duplicate results",
		"a signed fail-then-pass pair must not read as pass")

	_, err = Verify(env, testDigest, Policy{Keys: []crypto.PublicKey{pub}}, testFinished)
	require.ErrorContains(t, err, "duplicate results",
		"duplicate rejection is structural, not profile-dependent")
}

func TestMalformedPolicyPinIsAnError(t *testing.T) {
	signer, pub := ed25519Signer(t)
	env, err := Sign(testStatement(t, nil), signer)
	require.NoError(t, err)
	_, err = Verify(env, testDigest, Policy{Keys: []crypto.PublicKey{pub}, CheckSetSHA256: []string{"not-a-digest"}}, testFinished)
	require.ErrorContains(t, err, "not a bare-hex sha256",
		"a pin that can never match is a policy defect, not an inert entry")
}

// TestPolicyPinsTrustedRoot mirrors the check-set pin for the evidence trust
// root: a consumer can demand that the verdict's run verified its evidence
// against an approved root, and absence (a digest-only run, or a pre-field
// verdict) never satisfies the pin.
func TestPolicyPinsTrustedRoot(t *testing.T) {
	signer, pub := ed25519Signer(t)
	keys := []crypto.PublicKey{pub}
	approved := strings.Repeat("b", 64)

	goodEnv, err := Sign(testStatement(t, func(st *Statement) { st.Predicate.TrustedRootSHA256 = approved }), signer)
	require.NoError(t, err)
	otherEnv, err := Sign(testStatement(t, func(st *Statement) { st.Predicate.TrustedRootSHA256 = strings.Repeat("c", 64) }), signer)
	require.NoError(t, err)
	absentEnv, err := Sign(testStatement(t, nil), signer)
	require.NoError(t, err)

	// Without the pin, the field is informational.
	_, err = Verify(otherEnv, testDigest, Policy{Keys: keys}, testFinished)
	require.NoError(t, err)

	// The approved root passes, case-insensitively.
	_, err = Verify(goodEnv, testDigest, Policy{Keys: keys, TrustedRootSHA256: []string{strings.ToUpper(approved)}}, testFinished)
	require.NoError(t, err)

	// A different root does not.
	_, err = Verify(otherEnv, testDigest, Policy{Keys: keys, TrustedRootSHA256: []string{approved}}, testFinished)
	require.ErrorContains(t, err, "trusted root")
	require.ErrorContains(t, err, "not an approved one")

	// Absence never satisfies a content pin.
	_, err = Verify(absentEnv, testDigest, Policy{Keys: keys, TrustedRootSHA256: []string{approved}}, testFinished)
	require.ErrorContains(t, err, "not an approved one")

	// A malformed pin is a policy defect, not an inert entry.
	_, err = Verify(goodEnv, testDigest, Policy{Keys: keys, TrustedRootSHA256: []string{"sha256:" + approved}}, testFinished)
	require.ErrorContains(t, err, "not a bare-hex sha256")
}

// TestPredicateCarriesTrustedRootSHA256 pins the field's JSON name and
// round-trip, so a consumer-side rename cannot silently detach the pin.
func TestPredicateCarriesTrustedRootSHA256(t *testing.T) {
	signer, pub := ed25519Signer(t)
	root := strings.Repeat("d", 64)
	env, err := Sign(testStatement(t, func(st *Statement) { st.Predicate.TrustedRootSHA256 = root }), signer)
	require.NoError(t, err)
	st, err := Verify(env, testDigest, Policy{Keys: []crypto.PublicKey{pub}}, testFinished)
	require.NoError(t, err)
	assert.Equal(t, root, st.Predicate.TrustedRootSHA256)

	var decoded struct {
		Payload string `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(env, &decoded))
	payload, err := base64.StdEncoding.DecodeString(decoded.Payload)
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"trustedRootSha256":"`+root+`"`)
}

// TestVerifyRejectsFutureDatedVerdicts pins the forward-dating hole: a
// finishedAt in the future yields a negative age that passes any MaxAge
// forever, so it is rejected outright — with or without an age policy —
// while ordinary clock drift inside MaxClockSkew stays acceptable.
func TestVerifyRejectsFutureDatedVerdicts(t *testing.T) {
	signer, pub := ed25519Signer(t)
	pol := Policy{Keys: []crypto.PublicKey{pub}}

	farFuture, err := Sign(testStatement(t, func(st *Statement) {
		st.Predicate.FinishedAt = testFinished.Add(24 * time.Hour)
	}), signer)
	require.NoError(t, err)

	_, err = Verify(farFuture, testDigest, pol, testFinished)
	require.ErrorContains(t, err, "in the future", "rejected even with no MaxAge set")

	pol.MaxAge = time.Hour
	_, err = Verify(farFuture, testDigest, pol, testFinished)
	require.ErrorContains(t, err, "in the future", "a negative age must not satisfy MaxAge")

	withinSkew, err := Sign(testStatement(t, func(st *Statement) {
		st.Predicate.FinishedAt = testFinished.Add(MaxClockSkew - time.Second)
	}), signer)
	require.NoError(t, err)
	_, err = Verify(withinSkew, testDigest, pol, testFinished)
	require.NoError(t, err, "ordinary clock drift within MaxClockSkew is not forgery")
}
