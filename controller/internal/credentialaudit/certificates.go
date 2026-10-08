// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package credentialaudit

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strings"
)

// CertificateFingerprints accepts only public certificate PEM blocks and hashes
// their DER bytes. It rejects mixed/private-key bundles rather than redacting them.
func CertificateFingerprints(contents []byte) ([]string, error) {
	var fingerprints []string
	for strings.TrimSpace(string(contents)) != "" {
		// pem.Decode can skip arbitrary leading text; reject it explicitly.
		contents = []byte(strings.TrimSpace(string(contents)))
		if !strings.HasPrefix(string(contents), "-----BEGIN CERTIFICATE-----") {
			return nil, errors.New("audit input must contain only public certificate PEM blocks")
		}
		block, remaining := pem.Decode(contents)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("invalid public certificate PEM input")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid public certificate DER input")
		}
		sum := sha256.Sum256(certificate.Raw)
		fingerprints = append(fingerprints, hex.EncodeToString(sum[:]))
		if len(fingerprints) > 32 {
			return nil, errors.New("public certificate bundle exceeds audit fingerprint limit")
		}
		contents = remaining
	}
	if len(fingerprints) == 0 {
		return nil, errors.New("public certificate bundle is empty")
	}
	return fingerprints, nil
}
