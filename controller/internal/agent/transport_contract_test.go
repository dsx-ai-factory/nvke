// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

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
