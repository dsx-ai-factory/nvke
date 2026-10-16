// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx_test

import "testing"

// TestDSXMachineImages records the hardware acceptance cases before implementing
// the NICo lifecycle and node assertions. Every leaf is deliberately skipped:
// compiling this scaffold is not evidence of image qualification.
func TestDSXMachineImages(t *testing.T) {
	images := []struct {
		ubuntu string
		arch   string
	}{
		{ubuntu: "24.04", arch: "amd64"},
		{ubuntu: "24.04", arch: "arm64"},
		{ubuntu: "26.04", arch: "amd64"},
		{ubuntu: "26.04", arch: "arm64"},
	}

	for _, image := range images {
		t.Run("ubuntu-"+image.ubuntu+"/"+image.arch, func(t *testing.T) {
			t.Run("CandidateImageProvisioning", func(t *testing.T) {
				t.Skip("not implemented: expose a per-run pre-validation image to NICo, provision it with user-data, and verify the booted image identity")
			})

			t.Run("HardwareBootAndNetworking", func(t *testing.T) {
				t.Skip("not implemented: verify cloud-init completion and usable networking on the real NICs, including provider network configuration and recorded fabric")
			})

			t.Run("PlatformKernelAndDrivers", func(t *testing.T) {
				t.Skip("not implemented: verify the platform kernel, including the arm64 64K-page flavor, and exercise the actual GPU and network devices where present")
			})

			t.Run("TPMAttestation", func(t *testing.T) {
				t.Skip("not implemented: verify an enabled physical TPM 2.0 and successful SPIRE TPM attestation against its real endorsement-key chain")
			})

			t.Run("FDEFirstBootKeyDelivery", func(t *testing.T) {
				t.Skip("not implemented: provision the FDE image through NICo iPXE with the luks.key handoff and reach first boot without operator intervention or key leakage")
			})

			t.Run("FDETPMBinding", func(t *testing.T) {
				t.Skip("not implemented: verify the encrypted root is bound to the physical TPM and the temporary provisioning unlock mechanism has been removed")
			})

			t.Run("FDEClusterEndToEnd", func(t *testing.T) {
				t.Skip("not implemented: provision an FDE-backed tenant cluster, verify attestation and Ready nodes, and run a workload successfully")
			})

			t.Run("PostJoinWarmReboot", func(t *testing.T) {
				t.Skip("not implemented: join a non-FDE node, reboot it through the API without reimaging, and verify a new boot ID, preserved node state, Ready status, and workload health")
			})

			t.Run("PostJoinPowerCycle", func(t *testing.T) {
				t.Skip("not implemented: power-cycle a joined non-FDE node through the API without reimaging and verify preserved state, Ready status, and workload health")
			})

			t.Run("FDEWarmRebootAndUnseal", func(t *testing.T) {
				t.Skip("not implemented: warm-reboot a joined FDE node and verify unattended TPM unseal, encrypted root, preserved state, and recovered cluster health")
			})

			t.Run("FDEPowerCycleAndUnseal", func(t *testing.T) {
				t.Skip("not implemented: power-cycle a joined FDE node and verify unattended TPM unseal with vendor firmware, encrypted root, and recovered cluster health")
			})

			t.Run("NonFDEUpgradeAndReboot", func(t *testing.T) {
				t.Skip("not implemented: provision and join on release N-1, upgrade in place to N, reboot, and verify versions, preserved state, Ready status, and workload health")
			})

			t.Run("FDEUpgradeAndReboot", func(t *testing.T) {
				t.Skip("not implemented: upgrade an FDE node from N-1 to N, handle any required TPM reseal, reboot, and verify unattended unseal, encryption, and cluster health")
			})

			t.Run("FailedImageRecovery", func(t *testing.T) {
				t.Skip("not implemented: fail a candidate-image provision and recover the same physical machine by API reimage to a known-good image for the next run")
			})

			t.Run("PoolCleanupAndReuse", func(t *testing.T) {
				t.Skip("not implemented: remove run-owned instances and image registrations, verify wipe and return to the scoped pool, and provision a subsequent run without leaked resources")
			})

			t.Run("FailureDiagnostics", func(t *testing.T) {
				t.Skip("not implemented (SHOULD): collect API serial-console logs when available and cloud-init, netplan, interface, journal, firmware, Secure Boot, and PCR evidence without secrets")
			})

			t.Run("RetainInstanceAcrossRuns", func(t *testing.T) {
				t.Skip("not implemented (SHOULD): explicitly retain a run-owned instance for a follow-up run, then release it without losing ownership or blocking the nightly window")
			})
		})
	}
}
