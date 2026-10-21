// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/verify"
)

// fixture signs a verdict over content's digest and returns the attestation,
// public key and image paths plus the digest.
func fixture(t *testing.T, outcome string) (att, pub, image, digest string) {
	t.Helper()
	dir := t.TempDir()

	content := []byte("verdict subject bytes")
	image = filepath.Join(dir, "image.qcow2")
	require.NoError(t, os.WriteFile(image, content, 0o644))
	sum := sha256.Sum256(content)
	digest = hex.EncodeToString(sum[:])

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	pub = filepath.Join(dir, "verdict.pub")
	require.NoError(t, os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644))

	statement := verify.Statement{
		Type:          verify.StatementType,
		Subject:       []verify.Subject{{Name: "image.qcow2", Digest: map[string]string{"sha256": digest}}},
		PredicateType: verify.PredicateType,
		Predicate: verify.Predicate{
			Outcome:         outcome,
			CheckSetVersion: "0.2.0",
			Validator:       verify.Tool{Name: "nvke-image-validate", Version: "test"},
			FinishedAt:      time.Now().UTC(),
			EvidenceMode:    verify.EvidenceModeBundle,
			CheckSetSHA256:  "49" + strings.Repeat("a", 62),
			Target:          verify.Target{Variant: "fde", Arch: "amd64", Ubuntu: "26.04.1"},
			Results: []verify.CheckResult{
				{ID: "INT-001", Status: "pass", Severity: "blocking"},
				{ID: "INT-002", Status: "skip", Severity: "blocking", Reason: "input-missing"},
				{ID: "FDE-001", Status: "skip", Severity: "blocking", Reason: "not-applicable"},
			},
		},
	}
	payload, err := json.Marshal(statement)
	require.NoError(t, err)
	envelope, err := verify.Sign(payload, key)
	require.NoError(t, err)
	att = filepath.Join(dir, "verdict.json")
	require.NoError(t, os.WriteFile(att, envelope, 0o644))
	return att, pub, image, digest
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestRun(t *testing.T) {
	att, pub, image, digest := fixture(t, verify.OutcomePass)
	failAtt, failPub, _, failDigest := fixture(t, verify.OutcomeFail)
	sink := devNull(t)

	tests := []struct {
		name string
		args []string
		want int
	}{
		{
			name: "passing verdict by digest",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest},
			want: exitPass,
		},
		{
			name: "passing verdict by image hash",
			args: []string{"--attestation", att, "--key", pub, "--image", image},
			want: exitPass,
		},
		{
			name: "uppercase digest is normalized",
			args: []string{"--attestation", att, "--key", pub, "--digest", strings.ToUpper(digest)},
			want: exitPass,
		},
		{
			name: "wrong digest cannot verify",
			args: []string{"--attestation", att, "--key", pub, "--digest", strings.Repeat("0", 64)},
			want: exitCannot,
		},
		{
			name: "authentic but failing verdict exits 1",
			args: []string{"--attestation", failAtt, "--key", failPub, "--digest", failDigest},
			want: exitFail,
		},
		{
			name: "wrong key cannot verify",
			args: []string{"--attestation", att, "--key", failPub, "--digest", digest},
			want: exitCannot,
		},
		{
			name: "check-set floor above the verdict cannot verify",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--min-check-set-version", "0.3.0"},
			want: exitCannot,
		},
		{
			name: "profile satisfied by pass and not-applicable",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--required-checks", "INT-001, FDE-001"},
			want: exitPass,
		},
		{
			name: "profile rejects omitted validation",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--required-checks", "INT-001,INT-002"},
			want: exitCannot,
		},
		{
			name: "approved check set accepted",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--check-set-sha256", "49" + strings.Repeat("a", 62)},
			want: exitPass,
		},
		{
			name: "pin flag given but empty is a usage error",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--check-set-sha256", " , "},
			want: exitCannot,
		},
		{
			name: "unapproved check set rejected",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--check-set-sha256", strings.Repeat("b", 64)},
			want: exitCannot,
		},
		{
			name: "wrong validated target rejected",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--target-arch", "arm64"},
			want: exitCannot,
		},
		{
			name: "matching validated target accepted",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--target-variant", "fde", "--target-arch", "amd64"},
			want: exitPass,
		},
		{
			name: "missing required flags is a usage error",
			args: []string{"--attestation", att},
			want: exitCannot,
		},
		{
			name: "digest and image together is a usage error",
			args: []string{"--attestation", att, "--key", pub, "--digest", digest, "--image", image},
			want: exitCannot,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, run(tt.args, sink, sink))
		})
	}
}

func TestRunPrintsTheVerdict(t *testing.T) {
	att, pub, _, digest := fixture(t, verify.OutcomePass)
	out := filepath.Join(t.TempDir(), "out.txt")
	f, err := os.Create(out)
	require.NoError(t, err)
	assert.Equal(t, exitPass, run([]string{"--attestation", att, "--key", pub, "--digest", digest}, f, devNull(t)))
	require.NoError(t, f.Close())
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(data), "outcome: pass")
	assert.Contains(t, string(data), "check set: 0.2.0")
	assert.Contains(t, string(data), digest)
}
