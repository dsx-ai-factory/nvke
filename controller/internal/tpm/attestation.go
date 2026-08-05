// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package tpm provides utilities for extracting TPM attestation data from EK certificates.
package tpm

import (
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// ComputePublicKeyHash computes the SHA256 hash of a TPM EK public key.
// Input is a base64-encoded certificate bundle containing one or more TPM EK certificates in DER format.
// Output is the SHA256 hash of the first EK's public key. Any further EK certificates in the bundle are ignored.
//
// Note that this uses asn1.Unmarshal to parse the cert data, because we have observed some TPM EK certificates that contain serial numbers
// that are not minimally encoded, making them technically invalid, and causing the strict parsing in x509.ParseCertificate to fail.
// For our purposes, we can still use a TPM that returns these invalid certs, because we only need to verify the identity of the EK for
// attestation in spire-tpm-plugin and Teleport.
func ComputePublicKeyHash(tpmEkCertBase64 string) (string, error) {
	normalizedBase64 := normalizeBase64Input(tpmEkCertBase64)
	tpmEkCertBytes, err := base64.StdEncoding.DecodeString(normalizedBase64)
	if err != nil {
		return "", fmt.Errorf("failed to base64 decode TPM EK certificate: %w", err)
	}
	if len(tpmEkCertBytes) == 0 {
		return "", fmt.Errorf("no certificates found in TPM EK certificate data")
	}

	var certSequence asn1.RawValue
	_, err = asn1.Unmarshal(tpmEkCertBytes, &certSequence)
	if err != nil {
		return "", fmt.Errorf("failed to parse TPM EK certificate DER sequence: %w", err)
	}

	// to-be-signed certificate, excluding signature info
	var tbsCertificate asn1.RawValue
	_, err = asn1.Unmarshal(certSequence.Bytes, &tbsCertificate)
	if err != nil {
		return "", fmt.Errorf("failed to parse tbsCertificate from TPM EK certificate: %w", err)
	}

	tbsRest := tbsCertificate.Bytes
	var field asn1.RawValue

	tbsRest, err = asn1.Unmarshal(tbsRest, &field)
	if err != nil {
		return "", fmt.Errorf("failed to parse first tbsCertificate field from TPM EK certificate: %w", err)
	}

	// tbsCertificate starts with either serialNumber or [0] EXPLICIT version.
	if field.Class == asn1.ClassContextSpecific && field.Tag == 0 {
		tbsRest, err = asn1.Unmarshal(tbsRest, &field) // serialNumber
		if err != nil {
			return "", fmt.Errorf("failed to parse serialNumber field from TPM EK certificate: %w", err)
		}
	}

	for i := range 4 { // signature, issuer, validity, subject
		tbsRest, err = asn1.Unmarshal(tbsRest, &field)
		if err != nil {
			return "", fmt.Errorf("failed to parse tbsCertificate field %d from TPM EK certificate: %w", i+1, err)
		}
	}

	_, err = asn1.Unmarshal(tbsRest, &field) // subjectPublicKeyInfo
	if err != nil {
		return "", fmt.Errorf("failed to parse subjectPublicKeyInfo from TPM EK certificate: %w", err)
	}

	pubHashBytes := sha256.Sum256(field.FullBytes)
	ekPubHash := hex.EncodeToString(pubHashBytes[:])

	return ekPubHash, nil
}

// base64 often includes embedded newlines or spaces (e.g., PEM-style wrapping), which would cause DecodeString to fail.
func normalizeBase64Input(value string) string {
	normalized := strings.TrimSpace(value)
	normalized = strings.ReplaceAll(normalized, "\n", "")
	normalized = strings.ReplaceAll(normalized, "\r", "")
	normalized = strings.ReplaceAll(normalized, "\t", "")
	return strings.ReplaceAll(normalized, " ", "")
}
