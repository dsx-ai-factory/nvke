// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"crypto/tls"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// A node without an IP address must not be dialed. The endpoint would be
// ":<port>", which the resolver reads as localhost, so the dial would reach
// whatever listens on the controller's own machine.
func TestDialRefusesANodeWithoutAnAddress(t *testing.T) {
	t.Parallel()

	transport := &Transport{}
	conn, cleanup, err := transport.Dial(t.Context(), db.Node{ID: uuid.New()})
	require.ErrorIs(t, err, ErrNoAddress)
	require.Nil(t, conn)
	require.Nil(t, cleanup)
}

// The client identity follows the node's agent version, and only when the
// new-URN identity is deployed.
func TestExternalClientCertificateGatedOnAgentVersion(t *testing.T) {
	t.Parallel()

	oldCert := &tls.Certificate{}
	newCert := &tls.Certificate{}
	tests := map[string]struct {
		newURN       *tls.Certificate
		agentVersion string
		want         *tls.Certificate
	}{
		"empty version gets the old URN":                     {newCert, "", oldCert},
		"below the cutover gets the old URN":                 {newCert, "v0.2.0", oldCert},
		"at the cutover gets the new URN":                    {newCert, "v0.3.0", newCert},
		"at the cutover without a new identity gets the old": {nil, "v0.3.0", oldCert},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport := &Transport{
				externalClientCertificate:       oldCert,
				externalClientCertificateNewURN: params.newURN,
			}
			got := transport.externalClientCertificateFor(db.Node{AgentVersion: params.agentVersion})
			assert.Same(t, params.want, got)
		})
	}
}
