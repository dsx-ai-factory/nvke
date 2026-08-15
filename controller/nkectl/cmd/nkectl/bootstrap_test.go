// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validInventory = `
clusterName: nke-site
cluster:
  initNode:
    name: node1
    ip: 10.0.0.1
  k8sVersion: "1.32.0"
  cniName: calico
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`

// TestBootstrapSiteCommandWired verifies the command tree exposes
// "nkectl bootstrap site" — the runnable command required by the ticket.
func TestBootstrapSiteCommandWired(t *testing.T) {
	root := newRootCmd("test", "test", "test", "test")
	cmd, _, err := root.Find([]string{"bootstrap", siteSubcommand})
	if err != nil {
		t.Fatalf("find bootstrap site: %v", err)
	}
	if cmd.Name() != siteSubcommand {
		t.Fatalf("resolved command = %q, want %q", cmd.Name(), siteSubcommand)
	}
}

// TestRunBootstrapSiteDryRunValid confirms a valid inventory passes validation
// and dry-run returns no error without dispatching anything.
func TestRunBootstrapSiteDryRunValid(t *testing.T) {
	path := writeInventory(t, validInventory)
	if err := runBootstrapSite(&bootstrapSiteOptions{inventory: path, dryRun: true}); err != nil {
		t.Fatalf("dry-run of valid inventory returned error: %v", err)
	}
}

// TestRunBootstrapSiteInvalidFailsFast confirms an invalid inventory errors
// before reaching the (stubbed) dispatch step.
func TestRunBootstrapSiteInvalidFailsFast(t *testing.T) {
	const bad = `
clusterName: nke-site
cluster:
  initNode:
    name: node1
    ip: not-an-ip
  k8sVersion: "1.32.0"
  cniName: calico
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"
registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
`
	err := runBootstrapSite(&bootstrapSiteOptions{inventory: writeInventory(t, bad)})
	if err == nil {
		t.Fatal("invalid inventory did not error")
	}
	if strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("reached dispatch stub before validation failed: %v", err)
	}
	if !strings.Contains(err.Error(), "cluster.initNode.ip") {
		t.Errorf("error does not name the bad field: %v", err)
	}
}

// TestRunBootstrapSiteValidReachesStub confirms a valid inventory (no dry-run)
// passes validation and reaches the not-yet-implemented dispatch stub.
func TestRunBootstrapSiteValidReachesStub(t *testing.T) {
	err := runBootstrapSite(&bootstrapSiteOptions{inventory: writeInventory(t, validInventory)})
	if err == nil {
		t.Fatal("expected the dispatch stub error")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("expected dispatch stub error, got: %v", err)
	}
}

func writeInventory(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	return path
}
