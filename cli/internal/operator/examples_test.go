// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/operator"
)

// TestExamplesDecode keeps the shipped example documents valid: they are what
// an operator copies first, so one that nvkectl itself rejects is a bug.
func TestExamplesDecode(t *testing.T) {
	t.Parallel()

	examples := map[string]operator.Kind{
		"machineimage.yaml":    operator.KindMachineImage,
		"clusterrelease.yaml":  operator.KindClusterRelease,
		"nodepoolrelease.yaml": operator.KindNodePoolRelease,
	}
	for file, kind := range examples {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("..", "..", "examples", file))
			require.NoError(t, err)

			got, err := operator.Decode(kind, data)

			require.NoError(t, err)
			assert.Equal(t, "example-ubuntu-24.04-amd64", got.Metadata.Name)
			if kind != operator.KindMachineImage {
				assert.Equal(t, map[string]string{
					"internal.nvke.nvidia.com/machine-init-kubernetes-version":          "1.34.2",
					"internal.nvke.nvidia.com/machine-init-kubernetes-version-injected": "false",
				}, got.Metadata.Annotations, "a release example carries the Kubernetes version annotations")
			}
		})
	}
}
