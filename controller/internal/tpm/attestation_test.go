// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package tpm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputePublicKeyHash(t *testing.T) {
	t.Run("valid EK cert", func(t *testing.T) {
		ekCert := generateTestEKCert(t, 789)
		certBundleBase64 := base64.StdEncoding.EncodeToString(ekCert.Raw)

		hash, err := ComputePublicKeyHash(certBundleBase64)
		require.NoError(t, err)

		require.NotNil(t, hash)
		assert.Len(t, hash, 64)
	})

	t.Run("RSA public key", func(t *testing.T) {
		ekCert := generateRSACert(t, 999)
		certBundleBase64 := base64.StdEncoding.EncodeToString(ekCert.Raw)

		hash, err := ComputePublicKeyHash(certBundleBase64)
		require.NoError(t, err)

		require.NotNil(t, hash)
		assert.Len(t, hash, 64)
	})

	t.Run("nuvoton cert bundle with bad RSA serial number", func(t *testing.T) {
		// This is a specific bad cert we saw in the field. It has a RSA serial number that is not minimally encoded, making it technically
		// invalid and causing the strict parsing in x509.ParseCertificates to fail.
		const nuvotonBadRSASerialCertBundleBase64 = `
			MIIDUDCCAvagAwIBAgIKABnAET18foRxcTAKBggqhkjOPQQDAjBVMVMwHwYDVQQDExhOdXZvdG9uIFRQTSBSb290IENBIDIxMTEwJQYDVQQKEx5OdXZvdG9uIFRlY2hu
			b2xvZ3kgQ29ycG9yYXRpb24wCQYDVQQGEwJUVzAeFw0yMTA5MjkwNDQyNThaFw00MTA5MjUwNDQyNThaMAAwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCx
			99M5wl1KvrxrswWTHvBE5T7iJbul3lP2NXhRpVPJN8cpY/HI0ZxzGXokGNWylRnEY7vgV2KKO44IpJUWB7XB6+/mPgW0ALwSBr1sMGbxXYpoCidkp/1iKIjPulsVUJWY
			XdL/E5OX9pZSPDY7Gk/2cwqaImnqUFVYizivulaRpkZBO1H63ewTGGXMJPbU+cxU5G3XjSBSPp0QxQfMpUW2dKeFZZ0Pl44Lbvp6wwUsoqZ44Xc9oCqzcb/3jRsE30lI
			AjkNJvg7ARU/G5liiFLUH1nk4rw+0I5DfBy8lJiE0uxYRZnqCPre3uAaVwDVHfEYmUhMUwOZdvy3JM4qYLtxAgMBAAGjggE2MIIBMjBQBgNVHREBAf8ERjBEpEIwQDE+
			MBQGBWeBBQIBEwtpZDo0RTU0NDMwMDAQBgVngQUCAhMHTlBDVDc1eDAUBgVngQUCAxMLaWQ6MDAwNzAwMDIwDAYDVR0TAQH/BAIwADAQBgNVHSUECTAHBgVngQUIATAf
			BgNVHSMEGDAWgBQj9OIq0743SkSXcpVKooOu11JXLjAOBgNVHQ8BAf8EBAMCBSAwIgYDVR0JBBswGTAXBgVngQUCEDEOMAwMAzIuMAIBAAICAIowaQYIKwYBBQUHAQEE
			XTBbMFkGCCsGAQUFBzAChk1odHRwczovL3d3dy5udXZvdG9uLmNvbS9zZWN1cml0eS9OVEMtVFBNLUVLLUNlcnQvTnV2b3RvbiBUUE0gUm9vdCBDQSAyMTExLmNlcjAK
			BggqhkjOPQQDAgNIADBFAiEAuZW3Aa6DQKGHi3akm/tfxPLNJhIKBn/AatRs01ennZcCIFudB34UnrOkkkHsy4360qUF00kiWxa2rEqj2DziVAidMIIChTCCAiugAwIB
			AgIKSmhooVb44RKQTDAKBggqhkjOPQQDAjBVMVMwHwYDVQQDExhOdXZvdG9uIFRQTSBSb290IENBIDIxMTEwJQYDVQQKEx5OdXZvdG9uIFRlY2hub2xvZ3kgQ29ycG9y
			YXRpb24wCQYDVQQGEwJUVzAeFw0yMTA5MjkwNDQyNThaFw00MTA5MjUwNDQyNThaMAAwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAARwwsZzS0VgWkcFS8xaTKsVQPDW
			NRXZfz6fv9GfEsi+gh9liGsuXCKmR9Ip+RRebmYxLGZoYC8zixSdWci6KaB5o4IBNjCCATIwUAYDVR0RAQH/BEYwRKRCMEAxPjAUBgVngQUCARMLaWQ6NEU1NDQzMDAw
			EAYFZ4EFAgITB05QQ1Q3NXgwFAYFZ4EFAgMTC2lkOjAwMDcwMDAyMAwGA1UdEwEB/wQCMAAwEAYDVR0lBAkwBwYFZ4EFCAEwHwYDVR0jBBgwFoAUI/TiKtO+N0pEl3KV
			SqKDrtdSVy4wDgYDVR0PAQH/BAQDAgMIMCIGA1UdCQQbMBkwFwYFZ4EFAhAxDjAMDAMyLjACAQACAgCKMGkGCCsGAQUFBwEBBF0wWzBZBggrBgEFBQcwAoZNaHR0cHM6
			Ly93d3cubnV2b3Rvbi5jb20vc2VjdXJpdHkvTlRDLVRQTS1FSy1DZXJ0L051dm90b24gVFBNIFJvb3QgQ0EgMjExMS5jZXIwCgYIKoZIzj0EAwIDSAAwRQIgPc7FysUE
			CrNafg6zSAykOtSOPQP9mqdP7tglpvrPatACIQD0gudARyTcD0tgmhH5/eXdB4fn0CGQo0chaHiAFe8Zzw==
		`

		hash, err := ComputePublicKeyHash(nuvotonBadRSASerialCertBundleBase64)
		require.NoError(t, err)

		// Note this is the hash of the RSA cert, the first one in the file, not the ECC cert.
		// If you try parsing this with the following openssl command, it swallows the invalid RSA cert and moves on to the ECC cert:
		// cat bad-cert.b64 | base64 -d | openssl x509 -inform DER -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum
		//
		// The following one is the one that go-attestation comes up with as the first EK cert when talking directly to the TPM.
		assert.Equal(t, "641f7e0600ca5e9c73a9ab7af3e9182f55735b2e62b2dba128c97e45d42f5340", hash)
	})

	t.Run("nuvoton valid cert bundle", func(t *testing.T) {
		// "Typical" Nuvoton TPM EK certificate bundle as returned from the Forge API
		const nuvotonValidCertBundleBase64 = `
			MIIDUDCCAvagAwIBAgIKUrboCJDfYruBjTAKBggqhkjOPQQDAjBVMVMwHwYDVQQDExhOdXZvdG9uIFRQTSBSb290IENBIDIxMTEwJQYDVQQKEx5OdXZvdG9uIFRlY2hu
			b2xvZ3kgQ29ycG9yYXRpb24wCQYDVQQGEwJUVzAeFw0yMTA5MjkwNDQ2MzhaFw00MTA5MjUwNDQ2MzhaMAAwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDg
			LDBoXgAFOnf6HoOs/BsZxynE7wOVohNToJrB5pmyq7tfQSXge2tbqjjUv3h05ZagF09W8o+j+2rEfLIF7GQJ5IYeD8aiNKnjSk626+lAswoXxsvj3pEPTsoviOaW1WSc
			Qi1M04rrsJqQPIsmPaM0wGuYmscfThLeM+p4dsf+QiW7V05fK2vWc9TflC1UrbwirNXfRfa9Ts36gdHN3+ccDX6cnfG6uMwwlSd/dINd2Ohq4/ZNBd3v8SyzElaWx4n2
			7dvseHFF7FlEAF5VyCPBx4ctOzOR3mJpyRjA7hOmXELN4izjCPDj96JjKfhAbBV63COgdAPWtWeK9UlaIW5hAgMBAAGjggE2MIIBMjBQBgNVHREBAf8ERjBEpEIwQDE+
			MBQGBWeBBQIBEwtpZDo0RTU0NDMwMDAQBgVngQUCAhMHTlBDVDc1eDAUBgVngQUCAxMLaWQ6MDAwNzAwMDIwDAYDVR0TAQH/BAIwADAQBgNVHSUECTAHBgVngQUIATAf
			BgNVHSMEGDAWgBQj9OIq0743SkSXcpVKooOu11JXLjAOBgNVHQ8BAf8EBAMCBSAwIgYDVR0JBBswGTAXBgVngQUCEDEOMAwMAzIuMAIBAAICAIowaQYIKwYBBQUHAQEE
			XTBbMFkGCCsGAQUFBzAChk1odHRwczovL3d3dy5udXZvdG9uLmNvbS9zZWN1cml0eS9OVEMtVFBNLUVLLUNlcnQvTnV2b3RvbiBUUE0gUm9vdCBDQSAyMTExLmNlcjAK
			BggqhkjOPQQDAgNIADBFAiAFO4kAQpkZnXb9IIrT1tqsNQ9es6BJOkDxdLBotOvB3QIhANHHB7p5SYBwNf+grdKuuaytxfwnSJVStkBf/bEr+gxTMIIChDCCAiugAwIB
			AgIKfrzT5nUxSxHGADAKBggqhkjOPQQDAjBVMVMwHwYDVQQDExhOdXZvdG9uIFRQTSBSb290IENBIDIxMTEwJQYDVQQKEx5OdXZvdG9uIFRlY2hub2xvZ3kgQ29ycG9y
			YXRpb24wCQYDVQQGEwJUVzAeFw0yMTA5MjkwNDQ2MzhaFw00MTA5MjUwNDQ2MzhaMAAwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAATcmMVAlVfkxm3UTCt33sFhX5yY
			3F0GUNxtdVYZM9UB9Ocuwm03Kx83lC33i1/1q/ZonByAgi1l+EABD1VhpkDlo4IBNjCCATIwUAYDVR0RAQH/BEYwRKRCMEAxPjAUBgVngQUCARMLaWQ6NEU1NDQzMDAw
			EAYFZ4EFAgITB05QQ1Q3NXgwFAYFZ4EFAgMTC2lkOjAwMDcwMDAyMAwGA1UdEwEB/wQCMAAwEAYDVR0lBAkwBwYFZ4EFCAEwHwYDVR0jBBgwFoAUI/TiKtO+N0pEl3KV
			SqKDrtdSVy4wDgYDVR0PAQH/BAQDAgMIMCIGA1UdCQQbMBkwFwYFZ4EFAhAxDjAMDAMyLjACAQACAgCKMGkGCCsGAQUFBwEBBF0wWzBZBggrBgEFBQcwAoZNaHR0cHM6
			Ly93d3cubnV2b3Rvbi5jb20vc2VjdXJpdHkvTlRDLVRQTS1FSy1DZXJ0L051dm90b24gVFBNIFJvb3QgQ0EgMjExMS5jZXIwCgYIKoZIzj0EAwIDRwAwRAIgUZGTYICK
			ei9kyMqnO6QGWLrgCDsze+C294NSaTK9Rl0CICg8faSIE0sDe8X5qs7pU60eCXSKl2B0xnau4fzaO5bi
		`

		hash, err := ComputePublicKeyHash(nuvotonValidCertBundleBase64)
		require.NoError(t, err)

		assert.Equal(t, "207e80e1626b4339f390252b96676681f6a5a97f6c1180146368cd9496c406e7", hash)
	})

	t.Run("invalid base64", func(t *testing.T) {
		_, err := ComputePublicKeyHash("not-valid-base64!!!")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to base64 decode")
	})

	t.Run("invalid certificate data", func(t *testing.T) {
		invalidData := base64.StdEncoding.EncodeToString([]byte("not a certificate"))
		_, err := ComputePublicKeyHash(invalidData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse TPM EK certificate")
	})

	t.Run("empty certificate data", func(t *testing.T) {
		emptyData := base64.StdEncoding.EncodeToString([]byte{})
		_, err := ComputePublicKeyHash(emptyData)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no certificates found")
	})

	t.Run("deterministic hash", func(t *testing.T) {
		ekCert := generateTestEKCert(t, 54321)
		certBundleBase64 := base64.StdEncoding.EncodeToString(ekCert.Raw)

		hash1, err := ComputePublicKeyHash(certBundleBase64)
		require.NoError(t, err)

		hash2, err := ComputePublicKeyHash(certBundleBase64)
		require.NoError(t, err)

		assert.Equal(t, hash1, hash2)
	})

	t.Run("different certs produce different hashes", func(t *testing.T) {
		ekCert1 := generateTestEKCert(t, 111)
		ekCert2 := generateTestEKCert(t, 222)

		hash1, err := ComputePublicKeyHash(base64.StdEncoding.EncodeToString(ekCert1.Raw))
		require.NoError(t, err)

		hash2, err := ComputePublicKeyHash(base64.StdEncoding.EncodeToString(ekCert2.Raw))
		require.NoError(t, err)

		assert.NotEqual(t, hash1, hash2)
	})

	t.Run("invalid input", func(t *testing.T) {
		_, err := ComputePublicKeyHash("invalid-base64")
		require.Error(t, err)
	})
}

// Test helpers

func generateTestEKCert(t *testing.T, serialNumber int64) *x509.Certificate {
	t.Helper()

	caCert, caPrivKey := generateRootCAWithKey(t)

	ekPrivKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	ekTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(serialNumber),
		Subject: pkix.Name{
			CommonName:   "TPM EK Certificate",
			Organization: []string{"Test Org"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	ekCertDER, err := x509.CreateCertificate(rand.Reader, ekTemplate, caCert, &ekPrivKey.PublicKey, caPrivKey)
	require.NoError(t, err)

	ekCert, err := x509.ParseCertificate(ekCertDER)
	require.NoError(t, err)

	return ekCert
}

func generateRootCAWithKey(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	caPrivKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "Test CA",
			Organization: []string{"Test CA Org"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caPrivKey.PublicKey, caPrivKey)
	require.NoError(t, err)

	caCert, err := x509.ParseCertificate(caCertDER)
	require.NoError(t, err)

	return caCert, caPrivKey
}

func generateRSACert(t *testing.T, serialNumber int64) *x509.Certificate {
	t.Helper()

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(serialNumber),
		Subject: pkix.Name{
			CommonName:   "RSA TPM EK Certificate",
			Organization: []string{"Test Org"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privKey.PublicKey, privKey)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(certDER)
	require.NoError(t, err)

	return cert
}
