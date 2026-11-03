// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spiffe

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_NodeAgentID(t *testing.T) {
	t.Parallel()
	const nodeID = "45f59791-87df-4e85-9430-e565166ad020"

	tests := map[string]struct {
		tlsJWT bool
		want   string
	}{
		"legacy SPIFFE identity": {
			want: "spiffe://example.org/host/" + nodeID,
		},
		"canonical DSX exchange identity": {
			tlsJWT: true,
			want:   "spiffe://example.org/tenant/tenant-a/service/dsx/bm/" + nodeID,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.want, NodeAgentID("example.org", "tenant-a", "dsx", uuid.MustParse(nodeID), test.tlsJWT))
		})
	}
}
