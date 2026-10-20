// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
)

// Sign wraps an in-toto statement (as JSON) in a DSSE envelope signed with the
// operator's key: Ed25519 over the pre-authentication encoding, or ECDSA P-256
// over its SHA-256 (ASN.1 DER signature — the encoding cosign verifies). The
// signature carries no keyid: DSSE calls it advisory, but real verifiers
// (cosign among them) skip signatures whose keyid does not match their own
// derivation, so emitting one would break cross-verification; Verify tries
// every accepted key instead.
func Sign(statementJSON []byte, signer crypto.Signer) ([]byte, error) {
	message := pae(PayloadType, statementJSON)
	var sig []byte
	var err error
	switch key := signer.(type) {
	case ed25519.PrivateKey:
		sig, err = key.Sign(rand.Reader, message, crypto.Hash(0))
	case *ecdsa.PrivateKey:
		digest := sha256.Sum256(message)
		sig, err = key.Sign(rand.Reader, digest[:], crypto.SHA256)
	default:
		return nil, fmt.Errorf("unsupported signing key type %T (want ed25519 or ECDSA P-256)", signer)
	}
	if err != nil {
		return nil, fmt.Errorf("signing verdict: %w", err)
	}
	env := Envelope{
		PayloadType: PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(statementJSON),
		Signatures:  []Signature{{Sig: base64.StdEncoding.EncodeToString(sig)}},
	}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding envelope: %w", err)
	}
	return append(out, '\n'), nil
}

// KeyID derives the advisory key identifier: hex sha256 of the PKIX-encoded
// public key.
func KeyID(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("encoding public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// LoadSigner reads a PEM private key: PKCS#8 ("PRIVATE KEY") carrying Ed25519
// or ECDSA, or SEC 1 ("EC PRIVATE KEY").
func LoadSigner(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in signing key")
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing PKCS#8 signing key: %w", err)
		}
		switch k := key.(type) {
		case ed25519.PrivateKey:
			return k, nil
		case *ecdsa.PrivateKey:
			return k, nil
		default:
			return nil, fmt.Errorf("unsupported signing key type %T (want ed25519 or ECDSA)", key)
		}
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing EC signing key: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block %q in signing key", block.Type)
	}
}

// LoadPublicKeys reads every PKIX "PUBLIC KEY" PEM block in the input —
// multiple blocks support key rotation windows.
func LoadPublicKeys(pemBytes []byte) ([]crypto.PublicKey, error) {
	var keys []crypto.PublicKey
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("unsupported PEM block %q in verification keys (want PUBLIC KEY)", block.Type)
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing verification key: %w", err)
		}
		switch key.(type) {
		case ed25519.PublicKey, *ecdsa.PublicKey:
			keys = append(keys, key)
		default:
			return nil, fmt.Errorf("unsupported verification key type %T (want ed25519 or ECDSA)", key)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no PUBLIC KEY PEM blocks in verification keys")
	}
	return keys, nil
}
