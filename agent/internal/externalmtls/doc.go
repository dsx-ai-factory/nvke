// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package externalmtls builds the agent's gRPC server TLS configuration for
// deployments where the controller presents an externally issued client
// certificate instead of a SPIFFE SVID.
//
// The trust in this mode is deliberately one-directional at the TLS layer. The
// agent requires and verifies the controller's client certificate against an
// operator-supplied CA bundle, while the certificate the agent presents is
// self-signed and the controller does not verify it. The agent proves its own
// identity afterwards, through the NKEAuth record written by the tlsjwt
// package, which is why this mode is only accepted alongside
// --grpc-auth-mode=tls-jwt.
//
// # Requirement on the controller certificate
//
// The controller's certificate must carry the URI SAN
// urn:nvidia:nke:controller. Chaining to the bundle is not enough: the bundle
// names an externally operated CA, so without an identity check anything that
// CA has ever issued could drive the agent. An operator enabling this mode has
// to request the SAN when the certificate is issued; a certificate that chains
// but omits it is rejected at the handshake, and the agent logs
// "the controller certificate carries no ... URI SAN".
package externalmtls
