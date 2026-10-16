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

// TestLocalSiteReleasesDecode keeps the local site's release documents what
// nvkectl create takes: metadata and spec, no apiVersion or kind, one set
// per architecture.
func TestLocalSiteReleasesDecode(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "..", "deploy", "examples", "local-site", "releases")
	kinds := map[string]operator.Kind{"machineimage": operator.KindMachineImage, "clusterrelease": operator.KindClusterRelease, "nodepoolrelease": operator.KindNodePoolRelease}
	for _, arch := range []string{"amd64", "arm64"} {
		for file, kind := range kinds {
			name := file + "-" + arch + ".yaml"
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				data, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err)
				got, err := operator.Decode(kind, data)
				require.NoError(t, err)
				assert.Equal(t, "local-site-"+arch, got.Metadata.Name)
				if kind == operator.KindMachineImage {
					assert.Equal(t, arch, got.Spec["architecture"], "the machine image names its architecture")
				} else {
					assert.Equal(t, "local-site-"+arch, got.Spec["machineImageRef"].(map[string]any)["name"], "a release names its architecture's image")
				}
			})
		}
	}
}
