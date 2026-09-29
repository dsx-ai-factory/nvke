// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package externalmtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

// serverCertificateLifetime bounds the self-signed certificate the agent
// presents. The controller does not verify it, so the lifetime only has to
// outlast a long-running agent process rather than a deployment.
const serverCertificateLifetime = 10 * 365 * 24 * time.Hour

// serverCertificateSubject names the self-signed certificate. Nothing matches
// on it: the controller skips verification and the agent is authenticated by
// its NKEAuth record instead.
const serverCertificateSubject = "nke-agent"

// ControllerURI is the URI SAN the controller's client certificate must carry.
// Chaining to the bundle is not enough on its own: the bundle is an externally
// operated CA, so anything it has ever issued would otherwise be able to drive
// the agent. This is the external-mTLS counterpart to callerPathPrefixes on the
// SPIFFE path, and is fixed for the same reason -- one identity names the
// caller, and a deployment that needs a different one has to say so in code.
//
// It is exported so the flag help can state the requirement an operator has to
// meet when the controller's certificate is issued.
const ControllerURI = "urn:nvidia:nke:controller"

// ServerConfig returns the TLS configuration for an agent whose controller
// authenticates with an externally issued client certificate. Every connection
// must present a certificate that chains to caBundlePath and carries the
// controller's URI SAN; the certificate the agent serves is generated here and
// is not verified by the controller.
func ServerConfig(caBundlePath string) (*tls.Config, error) {
	clientCAs, err := loadClientCAs(caBundlePath)
	if err != nil {
		return nil, err
	}

	certificate, err := selfSignedCertificate()
	if err != nil {
		return nil, fmt.Errorf("generating the agent server certificate: %w", err)
	}

	return &tls.Config{
		MinVersion:       tls.VersionTLS12,
		Certificates:     []tls.Certificate{certificate},
		ClientAuth:       tls.RequireAndVerifyClientCert,
		ClientCAs:        clientCAs,
		VerifyConnection: verifyControllerIdentity,
	}, nil
}

// verifyControllerIdentity rejects a peer that chains to the controller CA but
// is not the controller. ClientAuth has already proved the chain by the time
// this runs, so only the leaf's URI SANs are examined.
func verifyControllerIdentity(state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		return errors.New("unauthorized: the controller presented no certificate")
	}
	for _, uri := range state.PeerCertificates[0].URIs {
		if uri.String() == ControllerURI {
			return nil
		}
	}
	return fmt.Errorf("unauthorized: the controller certificate carries no %q URI SAN", ControllerURI)
}

// loadClientCAs reads the controller CA bundle. An unreadable or empty bundle
// is an error rather than a fallback to the system pool: the system roots would
// accept any publicly issued certificate as a controller.
func loadClientCAs(caBundlePath string) (*x509.CertPool, error) {
	if caBundlePath == "" {
		return nil, errors.New("external mtls requires a controller client ca bundle")
	}

	bundle, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, fmt.Errorf("reading controller client ca bundle: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("controller client ca bundle %s contains no certificates", caBundlePath)
	}
	return pool, nil
}

func selfSignedCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating a key: %w", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating a serial number: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: serverCertificateSubject},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(serverCertificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("creating the certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parsing the generated certificate: %w", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}
