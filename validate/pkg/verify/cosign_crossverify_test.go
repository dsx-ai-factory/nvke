// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build crossverify

// Cross-verification suite: proves this package's DSSE output is what the
// sigstore ecosystem accepts, by having the cosign CLI verify a verdict this
// package signed. cosign v3 consumes signed material as a sigstore bundle, so
// the envelope is wrapped in a minimal bundle for the call. Driven by
// hack/cross-verify-cosign.sh and the CI lane; skipped when cosign is absent.
package verify

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sigstoreBundle wraps a DSSE envelope in the minimal sigstore bundle v0.3
// framing cosign v3 reads for key-based blob attestation verification.
func sigstoreBundle(t *testing.T, envelopeJSON []byte, keyID string) []byte {
	t.Helper()
	bundle := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{
			"publicKey": map[string]any{"hint": keyID},
		},
		"dsseEnvelope": json.RawMessage(envelopeJSON),
	}
	out, err := json.Marshal(bundle)
	require.NoError(t, err)
	return out
}

func TestCosignCrossVerify(t *testing.T) {
	cosign, err := exec.LookPath("cosign")
	if err != nil {
		t.Skip("cosign is not on PATH; the cross-verify lane needs it")
	}
	dir := t.TempDir()

	blob := filepath.Join(dir, "image.qcow2")
	content := []byte("image bytes under cross-verification")
	require.NoError(t, os.WriteFile(blob, content, 0o644))
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	pubPath := filepath.Join(dir, "verdict.pub")
	require.NoError(t, os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644))

	statement := Statement{
		Type:          StatementType,
		Subject:       []Subject{{Name: "image.qcow2", Digest: map[string]string{"sha256": digest}}},
		PredicateType: PredicateType,
		Predicate: Predicate{
			Outcome:         OutcomePass,
			CheckSetVersion: "0.2.0",
			Validator:       Tool{Name: "nvke-image-validate", Version: "crossverify"},
			FinishedAt:      time.Now().UTC(),
		},
	}
	payload, err := json.Marshal(statement)
	require.NoError(t, err)
	envelope, err := Sign(payload, key)
	require.NoError(t, err)
	keyID, err := KeyID(key.Public())
	require.NoError(t, err)
	bundlePath := filepath.Join(dir, "verdict.sigstore.json")
	require.NoError(t, os.WriteFile(bundlePath, sigstoreBundle(t, envelope, keyID), 0o644))

	args := []string{
		"verify-blob-attestation",
		"--key", pubPath,
		"--bundle", bundlePath,
		"--type", PredicateType,
		"--insecure-ignore-tlog=true",
		blob,
	}
	out, err := exec.Command(cosign, args...).CombinedOutput()
	require.NoError(t, err, "cosign rejected the verdict this package signed:\n%s", out)

	// The same attestation must NOT verify against different blob bytes:
	// cosign's claim check ties the subject digest to the blob.
	wrong := filepath.Join(dir, "other.qcow2")
	require.NoError(t, os.WriteFile(wrong, []byte("different bytes"), 0o644))
	args[len(args)-1] = wrong
	out, err = exec.Command(cosign, args...).CombinedOutput()
	require.Error(t, err, "cosign accepted the verdict for a different blob:\n%s", out)

	// Sanity: this package's own verifier agrees with cosign on the positive case.
	_, err = Verify(envelope, digest, Policy{Keys: []crypto.PublicKey{&key.PublicKey}}, time.Now())
	require.NoError(t, err)
}
