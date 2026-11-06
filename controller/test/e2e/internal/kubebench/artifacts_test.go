// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteArtifacts(t *testing.T) {
	t.Parallel()

	nodes := []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.12", StatusFail), result("1.2.11", StatusPass))}
	out := Evaluate(ClusterTenant, nodes, Register{Exceptions: []Exception{entry("1.2.11")}})
	dir := filepath.Join(t.TempDir(), "kube-bench", "tenant-1")

	require.NoError(t, WriteArtifacts(dir, nodes, out))

	raw, err := os.ReadFile(filepath.Join(dir, "results.json"))
	require.NoError(t, err)
	var results struct {
		Cluster   Cluster      `json:"cluster"`
		Benchmark string       `json:"benchmark"`
		Image     string       `json:"image"`
		Nodes     []NodeReport `json:"nodes"`
		Outcome   Outcome      `json:"outcome"`
	}
	require.NoError(t, json.Unmarshal(raw, &results))
	assert.Equal(t, ClusterTenant, results.Cluster)
	assert.Equal(t, "cis-1.12", results.Benchmark)
	assert.Equal(t, Image, results.Image)
	assert.Equal(t, nodes, results.Nodes)
	assert.Equal(t, out, results.Outcome)

	summary, err := os.ReadFile(filepath.Join(dir, "summary.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(summary), "1.1.12")
	assert.Contains(t, string(summary), "cp-a")
	assert.Contains(t, string(summary), "1.2.11")
	assert.Equal(t, out.Summary(), string(summary))
}

func TestSummary(t *testing.T) {
	t.Parallel()

	passed := Evaluate(ClusterSite, []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.11", StatusPass))}, Register{})
	assert.Contains(t, passed.Summary(), "passed")

	failed := Evaluate(ClusterSite, []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.1.12", StatusFail))}, Register{})
	assert.Contains(t, failed.Summary(), "failed")
	assert.Contains(t, failed.Summary(), "1 violation")
	assert.Contains(t, failed.Summary(), "value of 1.1.12")

	listed := Evaluate(ClusterTenant, []NodeReport{nodeReport("cp-a", RoleControlPlane, result("1.2.11", StatusPass))},
		Register{Exceptions: []Exception{entry("1.2.11"), entry("9.9.9")}})
	assert.Contains(t, listed.Summary(), "1 stale entry")
	assert.Contains(t, listed.Summary(), "9.9.9 (title 9.9.9)")
	assert.Contains(t, listed.Summary(), "1.2.11 (title 1.2.11)")
}
