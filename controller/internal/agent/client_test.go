// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestUnit_NodeAgentClientUsesPinnedSecurityProfile(t *testing.T) {
	t.Parallel()
	type parameters struct {
		node    db.Node
		wantErr string
	}
	tests := map[string]parameters{
		"old SPIFFE node does not switch to available external mTLS": {
			node:    db.Node{IP: "127.0.0.1", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM},
			wantErr: "trust domain is required for SPIFFE authentication",
		},
		"new external mTLS node requires its client identity": {
			node:    db.Node{IP: "127.0.0.1", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS, NodeAgentExternalMTLSEnabled: true},
			wantErr: "external mTLS client certificate is unavailable",
		},
		"new TLS JWT node requires a verifier": {
			node:    db.Node{IP: "127.0.0.1", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS, NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true},
			wantErr: "TLS/JWT verifier is unavailable",
		},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			client := &NodeAgentClient{
				transport: &Transport{
					cfg:                       config.Config{SpiffeEndpointSocket: "unix:///no-such-spire-agent.sock", NodeAgent: config.NodeAgentConfig{Port: "15005"}},
					externalClientCertificate: &tls.Certificate{},
				},
			}
			if params.node.NodeAgentExternalMTLSEnabled && !params.node.NodeAgentTLSJWTEnabled {
				client.transport.externalClientCertificate = nil
			}
			_, cleanup, err := client.getGRPCConnection(ctx, params.node)
			if cleanup != nil {
				cleanup()
			}
			require.ErrorContains(t, err, params.wantErr)
		})
	}
}
