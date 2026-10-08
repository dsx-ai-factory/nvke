// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dsx-ai-factory/nke/cli/internal/util"
	"github.com/dsx-ai-factory/nke/controller/api"
)

// TestExampleNodePoolSpecsDecode checks that every bundled node-pool example
// still decodes into the request the API generates. YAMLToRequest disallows
// unknown fields, so a field renamed in openapi.yaml fails here.
func TestExampleNodePoolSpecsDecode(t *testing.T) {
	t.Parallel()

	specs, err := filepath.Glob("../../../examples/*-nodepool.yaml")
	if err != nil {
		t.Fatalf("globbing example specs: %v", err)
	}
	if len(specs) == 0 {
		t.Fatal("no example node pool specs found")
	}

	for _, path := range specs {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}

			req, err := util.YAMLToRequest[api.CreateNodePoolJSONRequestBody](data)
			if err != nil {
				t.Fatalf("decoding %s: %v", path, err)
			}
			if req.Name == "" {
				t.Error("Name is empty")
			}
			if req.NodeNamePrefix == "" {
				t.Error("NodeNamePrefix is empty")
			}
		})
	}
}
