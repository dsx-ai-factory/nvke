// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusters

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dsx-ai-factory/nke/cli/internal/util"
	"github.com/dsx-ai-factory/nke/controller/api"
)

// TestExampleClusterSpecsDecode checks that every bundled example spec still
// decodes into the request the API generates. YAMLToRequest disallows unknown
// fields, so a field renamed in openapi.yaml fails here rather than in a user's
// first create.
func TestExampleClusterSpecsDecode(t *testing.T) {
	t.Parallel()

	specs, err := filepath.Glob("../../../examples/*-cluster.yaml")
	if err != nil {
		t.Fatalf("globbing example specs: %v", err)
	}
	if len(specs) == 0 {
		t.Fatal("no example cluster specs found")
	}

	for _, path := range specs {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}

			req, err := util.YAMLToRequest[api.CreateClusterJSONRequestBody](data)
			if err != nil {
				t.Fatalf("decoding %s: %v", path, err)
			}
			if req.Name == "" {
				t.Error("Name is empty")
			}
			if req.NodePools == nil || len(*req.NodePools) == 0 {
				t.Error("NodePools is empty")
			}
		})
	}
}
