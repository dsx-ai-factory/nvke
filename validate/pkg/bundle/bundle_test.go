// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vendored fixture is the real os-image/v0.2.0-rc.1 release evidence:
// its published checksums.txt and the sigstore bundle GitHub Actions attested
// it with, plus the trusted root pinned at vendoring time (kept beside them
// for the tests' convenience — production pins it out-of-band). Everything
// verifies offline — a test that reaches the network is a defect.
const (
	fixture = "../../testdata/release-v0.2.0-rc.1"
	// fixtureTrustedRoot is the test's out-of-band pin. The production code
	// never discovers this name: tests pass its bytes to PinTrustedRoot.
	fixtureTrustedRoot = "trusted-root.json"
)

// releaseIdentity matches the workflow that signs os-image release evidence.
func releaseIdentity() Identity {
	return Identity{
		Issuer:   "https://token.actions.githubusercontent.com",
		SANRegex: `^https://github\.com/dsx-ai-factory/nvke/\.github/workflows/os-image-artifact\.yml@.+$`,
	}
}

// pinFixtureRoot pins the vendored trusted root onto b, the way a caller with
// an operator-pinned root path does.
func pinFixtureRoot(t *testing.T, b *Bundle) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixture, fixtureTrustedRoot))
	require.NoError(t, err)
	require.NoError(t, b.PinTrustedRoot(raw))
}

func TestLoadParsesChecksums(t *testing.T) {
	b, err := Load(fixture)
	require.NoError(t, err)
	assert.Len(t, b.Checksums, 17, "the fixture release ships 8 qcow2s + 8 SBOMs + 1 checker binary")
	assert.Contains(t, b.Checksums, "nke-node-fde-ubuntu-24.04.5-amd64.qcow2")
}

func TestVerifyEvidence(t *testing.T) {
	b, err := Load(fixture)
	require.NoError(t, err)
	pinFixtureRoot(t, b)

	ev, err := b.VerifyEvidence(releaseIdentity())
	require.NoError(t, err)
	assert.Contains(t, ev.SignerSAN, "os-image-artifact.yml@refs/tags/os-image/v0.2.0-rc.1",
		"the verified signer is the release workflow at the release tag")
}

func TestVerifyEvidenceRejectsWrongIdentity(t *testing.T) {
	b, err := Load(fixture)
	require.NoError(t, err)
	pinFixtureRoot(t, b)

	tests := []struct {
		name string
		id   Identity
	}{
		{name: "wrong workflow", id: Identity{Issuer: "https://token.actions.githubusercontent.com", SANRegex: `^https://github\.com/someone-else/.*$`}},
		{name: "wrong issuer", id: Identity{Issuer: "https://accounts.example.com", SANRegex: `.*`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := b.VerifyEvidence(tt.id)
			require.Error(t, err)
		})
	}
}

// copyFixture clones the fixture's evidence members into a temp dir so a test
// can tamper with one without touching the vendored evidence.
func copyFixture(t *testing.T, mutate func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{ChecksumsName, AttestationName} {
		data, err := os.ReadFile(filepath.Join(fixture, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	mutate(dir)
	return dir
}

func TestVerifyEvidenceRejectsTamperedChecksums(t *testing.T) {
	dir := copyFixture(t, func(dir string) {
		path := filepath.Join(dir, ChecksumsName)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		// Swap one digest hex char: still parses, no longer the attested bytes.
		data[0] = map[byte]byte{'0': '1'}[data[0]] | 'a'
		require.NoError(t, os.WriteFile(path, data, 0o644))
	})
	b, err := Load(dir)
	require.NoError(t, err, "a tampered checksums file still parses; only verification catches it")
	pinFixtureRoot(t, b)
	_, err = b.VerifyEvidence(releaseIdentity())
	require.Error(t, err)
	assert.ErrorContains(t, err, "evidence verification")
}

func TestVerifyEvidenceFailsClosedOnMissingPieces(t *testing.T) {
	t.Run("no attestation", func(t *testing.T) {
		dir := copyFixture(t, func(dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, AttestationName)))
		})
		b, err := Load(dir)
		require.NoError(t, err)
		pinFixtureRoot(t, b)
		_, err = b.VerifyEvidence(releaseIdentity())
		require.ErrorContains(t, err, "no readable attestation")
	})
	t.Run("no trusted root pinned", func(t *testing.T) {
		// The directory may even hold a trusted-root.json — nothing reads it.
		// Only an explicit PinTrustedRoot counts, and without one verification
		// fails closed.
		dir := copyFixture(t, func(dir string) {
			raw, err := os.ReadFile(filepath.Join(fixture, fixtureTrustedRoot))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, fixtureTrustedRoot), raw, 0o644))
		})
		b, err := Load(dir)
		require.NoError(t, err)
		_, err = b.VerifyEvidence(releaseIdentity())
		require.ErrorContains(t, err, "no trusted root pinned")
	})
}

// TestPinTrustedRootRejectsGarbage pins the fail-closed parse: bytes that are
// not a trusted root never become one, and an empty pin is an error, not an
// accept-everything collection.
func TestPinTrustedRootRejectsGarbage(t *testing.T) {
	b := &Bundle{}
	require.Error(t, b.PinTrustedRoot([]byte("{\"not\": \"a trusted root\"}\n")))
	require.ErrorContains(t, b.PinTrustedRoot(nil), "holds no roots")
	require.ErrorContains(t, b.PinTrustedRoot([]byte("\n\n")), "holds no roots")
}

// TestLoadSBOM parses the real vendored SBOM: digest-verified against the
// checksums, and its deb package set extracted (calibrated: 729 of the 730
// SPDX entries are deb packages; the one left out is the image root entry).
func TestLoadSBOM(t *testing.T) {
	b, err := Load(fixture)
	require.NoError(t, err)

	pkgs, err := b.LoadSBOM("nke-node-fde-ubuntu-24.04.5-amd64.qcow2")
	require.NoError(t, err)
	assert.Len(t, pkgs, 729)
	assert.Contains(t, pkgs, "nke-cni-calico-manifests-3.30.1", "the calico package name carries its own version on real images")
	assert.Contains(t, pkgs, "nke-agent")

	missing, err := b.LoadSBOM("nke-node-non-fde-ubuntu-26.04.1-arm64.qcow2")
	require.NoError(t, err, "a missing SBOM file is a nil result, not an error")
	assert.Nil(t, missing)
}

// TestLoadSBOMRejectsTamper pins that an SBOM whose bytes do not match the
// checksums entry is an error — evidence the checksums do not vouch for is
// never silently used.
func TestLoadSBOMRejectsTamper(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{ChecksumsName} {
		data, err := os.ReadFile(filepath.Join(fixture, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	sbom := "nke-node-fde-ubuntu-24.04.5-amd64.spdx.json"
	data, err := os.ReadFile(filepath.Join(fixture, sbom))
	require.NoError(t, err)
	data = append(data, ' ')
	require.NoError(t, os.WriteFile(filepath.Join(dir, sbom), data, 0o644))

	b, err := Load(dir)
	require.NoError(t, err)
	_, err = b.LoadSBOM("nke-node-fde-ubuntu-24.04.5-amd64.qcow2")
	require.ErrorContains(t, err, "does not match the checksums entry")
}

// TestLoadParsesTheAuthenticatedSnapshot pins the round-3 integrity repair:
// the digest map must come from the same bytes VerifyEvidence authenticates,
// so replacing the checksums file after Load changes neither.
func TestLoadParsesTheAuthenticatedSnapshot(t *testing.T) {
	dir := copyFixture(t, func(string) {})
	b, err := Load(dir)
	require.NoError(t, err)
	pinFixtureRoot(t, b)
	want := len(b.Checksums)
	wantDigest, ok := b.Checksums["nke-node-fde-ubuntu-24.04.5-amd64.qcow2"]
	require.True(t, ok)

	// The swap an attacker would race: a map naming different digests.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ChecksumsName),
		[]byte(strings.Repeat("0", 64)+"  nke-node-fde-ubuntu-24.04.5-amd64.qcow2\n"), 0o644))

	assert.Len(t, b.Checksums, want, "the loaded map must not depend on later file state")
	got := b.Checksums["nke-node-fde-ubuntu-24.04.5-amd64.qcow2"]
	assert.Equal(t, wantDigest, got)
	_, err = b.VerifyEvidence(Identity{Issuer: "https://token.actions.githubusercontent.com", SANRegex: `^https://github\.com/dsx-ai-factory/nvke/\.github/workflows/os-image-artifact\.yml@.+$`})
	require.NoError(t, err, "evidence still verifies: subject and map share one snapshot")
}
