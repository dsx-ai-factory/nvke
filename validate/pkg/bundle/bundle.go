// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bundle loads a release evidence bundle and verifies its provenance
// attestation, entirely offline: the sigstore bundle is checked against a
// trusted root the operator pins out-of-band, never fetched and never read
// from the evidence directory — a root that travels with the evidence it
// authenticates could be replaced along with it. The evidence layout is what
// the os-image release workflow publishes: a checksums.txt naming every
// artifact's sha256 and a sigstore bundle whose in-toto subject is that
// checksums.txt. The trusted root arrives separately through PinTrustedRoot.
package bundle

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/static"
)

// Default member names inside a bundle directory, matching what the release
// publishes (checksums + its sigstore bundle). The trusted root has no member
// name here on purpose: it is pinned out-of-band, never discovered in the
// directory it authenticates.
const (
	ChecksumsName   = "checksums.txt"
	AttestationName = "checksums.txt.sigstore.json"
)

// Identity is the certificate identity the attestation's signer must satisfy.
// For evidence signed by the os-image release workflow, the SAN is the
// workflow ref and the issuer is GitHub's OIDC issuer.
type Identity struct {
	// Issuer is the exact OIDC issuer; empty means any issuer, which is only
	// acceptable in tests.
	Issuer string
	// SANRegex constrains the certificate's SubjectAlternativeName.
	SANRegex string
}

// Bundle is a loaded evidence directory. Checksums is parsed but UNVERIFIED
// until VerifyEvidence succeeds; callers that skip verification own that
// decision explicitly.
type Bundle struct {
	Dir       string
	Checksums map[string]string

	checksumsRaw []byte
	attestation  *sgbundle.Bundle
	trusted      root.TrustedMaterial
}

// Load reads a bundle directory: checksums.txt (required) and its sigstore
// attestation (required for VerifyEvidence; its absence is reported there,
// not here, so digest-only runs still work). The trusted root is not read
// here: pin it with PinTrustedRoot from an operator-supplied source.
func Load(dir string) (*Bundle, error) {
	checksumsPath := filepath.Join(dir, ChecksumsName)
	raw, err := os.ReadFile(checksumsPath)
	if err != nil {
		return nil, fmt.Errorf("reading bundle checksums: %w", err)
	}
	// Parse the same byte snapshot VerifyEvidence authenticates: a second
	// read could hand the checks a map the attestation never covered.
	sums, err := static.ParseChecksumsBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("checksums %s: %w", checksumsPath, err)
	}
	b := &Bundle{Dir: dir, Checksums: sums, checksumsRaw: raw}

	if att, err := sgbundle.LoadJSONFromPath(filepath.Join(dir, AttestationName)); err == nil {
		b.attestation = att
	}
	return b, nil
}

// PinTrustedRoot parses and pins the trusted root VerifyEvidence verifies
// against: one JSON trusted root per line, the format
// `gh attestation trusted-root` emits. Several lines become a collection that
// accepts material trusted by any of them. The caller supplies the bytes from
// an operator-pinned location — never from the evidence directory, whose
// contents are exactly what the root is needed to authenticate.
func (b *Bundle) PinTrustedRoot(raw []byte) error {
	var collection root.TrustedMaterialCollection
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		tr, err := root.NewTrustedRootFromJSON(line)
		if err != nil {
			return fmt.Errorf("parsing trusted root: %w", err)
		}
		collection = append(collection, tr)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading trusted root: %w", err)
	}
	if len(collection) == 0 {
		return fmt.Errorf("the trusted root holds no roots")
	}
	b.trusted = collection
	return nil
}

// Evidence is a successful verification: the attestation is signed by
// material the trusted root vouches for, the signer satisfies the identity
// constraint, and the attested subject is byte-for-byte this bundle's
// checksums.txt.
type Evidence struct {
	// SignerSAN is the verified certificate's SubjectAlternativeName — for
	// workflow-signed evidence, the workflow ref that produced it.
	SignerSAN string
}

// VerifyEvidence verifies the attestation offline against the bundle's
// trusted root and binds it to this bundle's checksums.txt. Any missing piece
// (attestation, trusted root) is a verification failure: the caller asked for
// authenticated evidence, so absence must not read as success.
func (b *Bundle) VerifyEvidence(id Identity) (*Evidence, error) {
	if b.attestation == nil {
		return nil, fmt.Errorf("bundle %s: no readable attestation %s", b.Dir, AttestationName)
	}
	if b.trusted == nil {
		return nil, fmt.Errorf("bundle %s: no trusted root pinned — supply the operator-pinned root (--trusted-root); the root never travels with the evidence it authenticates", b.Dir)
	}
	verifier, err := verify.NewVerifier(b.trusted, verify.WithObserverTimestamps(1))
	if err != nil {
		return nil, fmt.Errorf("building verifier: %w", err)
	}
	certID, err := verify.NewShortCertificateIdentity(id.Issuer, "", "", id.SANRegex)
	if err != nil {
		return nil, fmt.Errorf("building identity policy: %w", err)
	}
	subject := sha256.Sum256(b.checksumsRaw)
	result, err := verifier.Verify(b.attestation, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", subject[:]),
		verify.WithCertificateIdentity(certID),
	))
	if err != nil {
		return nil, fmt.Errorf("evidence verification (subject sha256:%s): %w", hex.EncodeToString(subject[:]), err)
	}
	ev := &Evidence{}
	if result.Signature != nil && result.Signature.Certificate != nil {
		ev.SignerSAN = result.Signature.Certificate.SubjectAlternativeName
	}
	return ev, nil
}
