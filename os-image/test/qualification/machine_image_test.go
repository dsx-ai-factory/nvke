// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification_test

import (
	"os"
	"testing"
)

// TestDSXRecipeValidation reserves the RelEng acceptance suite from section 10
// of the recipe validation requirements referenced by NKX-17988.
func TestDSXRecipeValidation(t *testing.T) {
	pendingUbuntuVersions(t, "run RelEng's section 10 acceptance criteria on the shared DSX pool and record the qualification results")
}

// TestDSXHardwareSmoke reserves candidate-image boot and hardware checks.
func TestDSXHardwareSmoke(t *testing.T) {
	pendingUbuntuVersions(t, "provision the candidate image through NICo and verify image identity, boot, cloud-init networking, and applicable hardware drivers")
}

// TestDSXFDEEndToEnd reserves the real-TPM FDE provisioning and cluster checks.
func TestDSXFDEEndToEnd(t *testing.T) {
	pendingUbuntuVersions(t, "verify the NICo iPXE key handoff, real TPM attestation and binding, encrypted root, and FDE-backed cluster/workload health")
}

// TestDSXUpgradeAndReboot reserves node lifecycle checks for ordinary and FDE images.
func TestDSXUpgradeAndReboot(t *testing.T) {
	pendingUbuntuVersions(t, "verify reboot and power cycle without reimage, N-1 to N upgrade within an Ubuntu series, state preservation, FDE unseal, and recovered node/workload health")
}

// pendingUbuntuVersions keeps the epic's two required Ubuntu versions inside
// each functional test. Skipped cases are not qualification evidence.
func pendingUbuntuVersions(t *testing.T, acceptance string) {
	t.Helper()
	if machineType := os.Getenv("DSX_MACHINE_TYPE_ID"); machineType != "" {
		t.Logf("Requested NICo instance type: %s", machineType)
	}
	for _, ubuntu := range []string{"24.04", "26.04"} {
		t.Run("ubuntu-"+ubuntu, func(t *testing.T) {
			t.Skip("not implemented: " + acceptance)
		})
	}
}
