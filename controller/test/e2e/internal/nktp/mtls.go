// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	otlpGRPCPort   = "4317"
	refusalTimeout = 15 * time.Second
)

// GatewayRefusesAnonymous passes only when the gateway refuses a
// certificate-less client with a TLS alert. That leaves client-go logging
// "error forwarding port 4317 ... broken pipe": the gateway hanging up afterwards.
func (v *Validator) GatewayRefusesAnonymous(gateway string) Check {
	service := chartRelease + "-" + gateway + "-collector"
	return Check{Name: service + " refuses a client without a certificate", Probe: func(ctx context.Context) error {
		roots, err := v.internalCA(ctx)
		if err != nil {
			return err
		}
		forwarder, err := kube.NewServiceForwarder(v.restConfig, chartNamespace, service, otlpGRPCPort)
		if err != nil {
			return err
		}
		defer forwarder.Close()
		conn, err := forwarder.Dial(ctx)
		if err != nil {
			return err
		}
		return refusesAnonymousClient(ctx, conn, service+"."+chartNamespace+".svc", roots)
	}}
}

// internalCA returns the CA that signs NKTP's internal mTLS certificates,
// loaded from the Secret managed by cert-manager.
func (v *Validator) internalCA(ctx context.Context) (*x509.CertPool, error) {
	name := chartRelease + "-internal-ca"
	secret, err := v.clientset.CoreV1().Secrets(chartNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the internal CA from Secret %s/%s: %w", chartNamespace, name, err)
	}
	pool := x509.NewCertPool()
	for _, key := range []string{corev1.ServiceAccountRootCAKey, corev1.TLSCertKey} {
		if pool.AppendCertsFromPEM(secret.Data[key]) {
			return pool, nil
		}
	}
	return nil, fmt.Errorf("secret %s/%s holds no CA certificate under %s or %s", chartNamespace, name, corev1.ServiceAccountRootCAKey, corev1.TLSCertKey)
}

// refusesAnonymousClient holds when the server on conn proves itself against
// roots and then refuses a certificate-less client with a TLS alert. Only the
// alert counts: a server that never asked for a certificate can also close,
// reset or go quiet. Under TLS 1.3 the alert arrives on the first read rather
// than in the handshake; a server that accepted the client answers that read
// with its HTTP/2 SETTINGS instead. It closes conn.
func refusesAnonymousClient(ctx context.Context, conn net.Conn, serverName string, roots *x509.CertPool) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(refusalTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return fmt.Errorf("setting a deadline on the connection to %s: %w", serverName, err)
	}

	serverVerified := false
	tlsConn := tls.Client(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: serverName,
		NextProtos: []string{"h2"},
		VerifyConnection: func(tls.ConnectionState) error {
			serverVerified = true
			return nil
		},
	})
	defer func() { _ = tlsConn.Close() }()

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		if serverVerified && isTLSAlert(err) {
			return nil
		}
		return fmt.Errorf("TLS handshake with %s: %w", serverName, err)
	}
	n, err := tlsConn.Read(make([]byte, 1))
	switch {
	case n > 0:
		return fmt.Errorf("%s accepted a client that presented no certificate", serverName)
	case isTLSAlert(err):
		return nil
	case err == nil:
		return fmt.Errorf("%s returned neither data nor an error to a client without a certificate", serverName)
	default:
		return fmt.Errorf("%s did not refuse a client without a certificate by TLS alert, so client authentication is unproven: %w",
			serverName, err)
	}
}

// crypto/tls reports a peer's alert as a net.OpError with Op "remote error".
func isTLSAlert(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "remote error"
}
