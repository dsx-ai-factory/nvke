// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package naming

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateNodeNamePrefix(t *testing.T) {
	clusterName := "runtime-dgxc-k8s-forge-ytl-dev1"
	maxPrefix := strings.Repeat("a", MaxNodeNamePrefixLen(clusterName))

	t.Run("accepts prefix at limit", func(t *testing.T) {
		require.NoError(t, ValidateNodeNamePrefix(clusterName, maxPrefix))
	})

	t.Run("rejects prefix over limit", func(t *testing.T) {
		err := ValidateNodeNamePrefix(clusterName, maxPrefix+"x")
		require.Error(t, err)
		require.Contains(t, err.Error(), "nodeNamePrefix")
		require.Contains(t, err.Error(), "too long")
	})

	t.Run("rejects empty prefix", func(t *testing.T) {
		err := ValidateNodeNamePrefix(clusterName, "  ")
		require.EqualError(t, err, "nodeNamePrefix must not be empty")
	})

	t.Run("rejects cluster name that leaves no room for prefix", func(t *testing.T) {
		err := ValidateNodeNamePrefix(strings.Repeat("a", MaxGeneratedNodeNameLength), "cp")
		require.Error(t, err)
		require.Contains(t, err.Error(), "cluster name")
		require.Contains(t, err.Error(), "too long")
	})
}

func TestValidateClusterNameForNodeNaming(t *testing.T) {
	t.Run("accepts cluster name with room for prefix", func(t *testing.T) {
		require.NoError(t, ValidateClusterNameForNodeNaming("my-cluster"))
	})

	t.Run("rejects cluster name that leaves no room for prefix", func(t *testing.T) {
		err := ValidateClusterNameForNodeNaming(strings.Repeat("a", MaxGeneratedNodeNameLength))
		require.Error(t, err)
		require.Contains(t, err.Error(), "cluster name")
		require.Contains(t, err.Error(), "too long")
	})
}

func TestGenerateNodeName(t *testing.T) {
	t.Run("retries on collision", func(t *testing.T) {
		// Simulate node_set_controller naming without real randomness:
		//   real-use: GenerateNodeName(existingNodeNames, cluster.Name, pool.NodeNamePrefix, seededRand.Intn)
		//   test:       existingNames holds one taken node; randIntn returns suffix 0 then 1 in order.
		attempts := 0
		name := GenerateNodeName(
			[]string{"mycluster-cp-0000"}, // existingNames — suffix 0000 already provisioned
			"mycluster",
			"cp",
			func(int) int { // randIntn stub — 1st pick collides, 2nd is returned
				attempts++
				if attempts == 1 {
					return 0
				}
				return 1
			},
		)
		require.Equal(t, "mycluster-cp-0001", name)
	})
}
