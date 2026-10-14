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
)

const (
	refusalTimeout = 15 * time.Second
)

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
