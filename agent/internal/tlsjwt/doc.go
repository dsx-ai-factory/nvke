// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tlsjwt implements the node-agent side of the pre-gRPC authentication
// exchange.
//
// The agent obtains a JWT-SVID from the node-local SPIRE Workload API for the
// controller audience, exchanges it at the DSX Identity token-exchange endpoint
// for a DSX-signed identity token, and writes that token as one NKEAuth record
// once per accepted connection, after the TLS handshake authenticates the
// controller and before gRPC begins HTTP/2 processing.
//
// The controller half of the protocol lives in the controller module's
// internal/agent package. Both sides share the record codec in pkg/nkeauth.
package tlsjwt
