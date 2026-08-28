// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// The authorizer is the whole of this agent's access control, so these pin what it
// refuses as much as what it admits.
func TestAuthorizeClient(t *testing.T) {
	t.Parallel()

	const agentTrustDomain = "example.org"

	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "legacy service path", id: "spiffe://example.org/nke-service/cloud-workflow-engine"},
		{name: "tenant-grammar path", id: "spiffe://example.org/tenant/nke/service/cloud-workflow-engine/bm/cwe"},
		{name: "tenant-grammar path, other service", id: "spiffe://example.org/tenant/nke/service/capi-manager/bm/cwe"},

		// Different paths that merely share leading characters.
		{name: "legacy prefix is not a substring match", id: "spiffe://example.org/nke-service-evil/cwe", wantErr: true},
		{name: "tenant prefix is not a substring match", id: "spiffe://example.org/tenant/nke/service-evil/cwe", wantErr: true},

		// The tenant segment is inside the prefix, so other tenants are refused.
		{name: "other tenant", id: "spiffe://example.org/tenant/acme/service/cloud-workflow-engine/bm/x", wantErr: true},

		{name: "unrelated path", id: "spiffe://example.org/nke-e2e/probe", wantErr: true},
		{name: "foreign trust domain", id: "spiffe://attacker.test/nke-service/cloud-workflow-engine", wantErr: true},
		{name: "trust domain is not a prefix match", id: "spiffe://example.org.attacker.test/nke-service/cwe", wantErr: true},
	}

	authorize := authorizeClient(agentTrustDomain)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, err := spiffeid.FromString(tc.id)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.id, err)
			}

			err = authorize(id)
			if tc.wantErr && err == nil {
				t.Fatalf("authorizeClient(%q) = nil, want an error", tc.id)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("authorizeClient(%q) = %v, want nil", tc.id, err)
			}
		})
	}
}
