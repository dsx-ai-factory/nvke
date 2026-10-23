// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_NewNodesAttestThroughDSXImds(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		node db.Node
		want db.SpireAttestationMethod
	}{
		"a SPIFFE node on a tpm deployment attests through dsxImds": {
			node: db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodTPM},
			want: db.SpireAttestationMethodDSXIMDS,
		},
		"a node without SPIFFE keeps the deployment's method": {
			node: db.Node{SpireAttestationMethod: db.SpireAttestationMethodTPM},
			want: db.SpireAttestationMethodTPM,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			poolNode, placeholder := tt.node, tt.node
			require.NoError(t, New().ApplyNodePoolNodePayload(db.NodePool{}, &poolNode))
			New().ApplyPlaceholderNodePayload(&placeholder)

			require.Equal(t, tt.want, poolNode.SpireAttestationMethod)
			require.Equal(t, tt.want, placeholder.SpireAttestationMethod)
		})
	}
}
