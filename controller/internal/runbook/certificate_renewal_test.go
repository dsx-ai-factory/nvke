// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCertificateRenewalRunbookTerminalReceiptFencesLateRetry models cycle B
// replacing the shared phase file after A completes. A receipt-aware delayed A
// must return A's outcome without renewing again or touching B's staged manifest.
func TestCertificateRenewalRunbookTerminalReceiptFencesLateRetry(t *testing.T) {
	for _, nextPhase := range []string{"complete", "restart_pending"} {
		t.Run(nextPhase, func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.setEnv("OPERATION_ID", "forced-a")
			harness.setEnv("FORCE", "true")
			output, err := harness.run(t)
			require.NoError(t, err, output)
			require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
			harness.setEnv("STATUS_ONLY", "true")
			output, err = harness.run(t)
			require.NoError(t, err, output)
			require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=forced-a\n", output)

			// B replaces the shared phase file after A's cluster ownership is released.
			harness.setEnv("STATUS_ONLY", "false")
			harness.setEnv("OPERATION_ID", "cycle-b")
			harness.setEnv("FORCE", "false")
			statePath := filepath.Join(harness.stateDir, "state")
			if nextPhase == "complete" {
				output, err = harness.run(t)
				require.NoError(t, err, output)
				require.Contains(t, output, "RENEWAL_OUTCOME=unchanged")
			} else {
				// Model B with a staged manifest and no receipt; late A must not restore or resume it.
				require.NoError(t, os.WriteFile(statePath, []byte("cycle-b\nrestart_pending\netcd\nrenewed\n"+harness.caFingerprints(t)+"\nterminal-receipt-v1\n"), 0o600))
				require.NoError(t, os.Rename(filepath.Join(harness.manifestsDir, "etcd.yaml"), filepath.Join(harness.stagingDir, "etcd.yaml")))
			}
			before, err := os.ReadFile(statePath)
			require.NoError(t, err)
			beforeRestarts, err := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
			require.NoError(t, err)

			harness.setEnv("OPERATION_ID", "forced-a")
			harness.setEnv("FORCE", "true")
			output, err = harness.run(t)

			require.NoError(t, err, output)
			require.Contains(t, output, "RENEWAL_OUTCOME=renewed", "retain A's original outcome, not B's")
			after, err := os.ReadFile(statePath)
			require.NoError(t, err)
			require.Equal(t, before, after, "late A must not touch B's phase file")
			renewed, err := os.ReadFile(filepath.Join(harness.runtimeDir, "renewed"))
			require.NoError(t, err)
			require.Equal(t, "renew\n", string(renewed), "late A must never renew again")
			afterRestarts, err := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
			require.NoError(t, err)
			require.Equal(t, beforeRestarts, afterRestarts)
			if nextPhase == "restart_pending" {
				require.FileExists(t, filepath.Join(harness.stagingDir, "etcd.yaml"))
				require.NoFileExists(t, filepath.Join(harness.manifestsDir, "etcd.yaml"))
			}
			harness.setEnv("STATUS_ONLY", "true")
			output, err = harness.run(t)
			require.NoError(t, err, output)
			require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=forced-a\n", output)
		})
	}
}

// TestCertificateRenewalRunbookOldStateCannotProduceTerminalReceipt preserves old
// phase-file resumption without inventing receipt-aware provenance: delayed old
// scripts may ignore receipts, so resumed old state cannot prove fencing.
func TestCertificateRenewalRunbookOldStateCannotProduceTerminalReceipt(t *testing.T) {
	for _, phase := range []string{"complete", "renewing", "restart_pending"} {
		t.Run(phase, func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.setEnv("OPERATION_ID", "old-cycle")
			statePath := filepath.Join(harness.stateDir, "state")
			component := ""
			if phase == "restart_pending" {
				component = "etcd"
			}
			require.NoError(t, os.WriteFile(statePath, []byte("old-cycle\n"+phase+"\n"+component+"\nrenewed\n"+harness.caFingerprints(t)+"\n"), 0o600))

			output, err := harness.run(t)

			require.NoError(t, err, output)
			require.Contains(t, output, "RENEWAL_OUTCOME=renewed", "preserve old state resumption")
			state, err := os.ReadFile(statePath)
			require.NoError(t, err)
			require.Equal(t, 5, strings.Count(string(state), "\n"), "resumption must not upgrade the phase format")
			require.NoDirExists(t, filepath.Join(harness.stateDir, "completed"))
			harness.setEnv("STATUS_ONLY", "true")
			output, err = harness.run(t)
			require.Error(t, err, output)
			require.NotContains(t, output, "RENEWAL_TERMINAL_OPERATION_ID=")
		})
	}
}

// TestCertificateRenewalRunbookV1DoesNotProduceTerminalReceipt keeps newly started
// V1 calls in the old phase format rather than promising V2 termination evidence.
func TestCertificateRenewalRunbookV1DoesNotProduceTerminalReceipt(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("EXPECTED_ETCD_MEMBERS", "")
	harness.setEnv("OPERATION_ID", "v1-cycle")

	output, err := harness.run(t)

	require.NoError(t, err, output)
	require.Contains(t, output, "RENEWAL_OUTCOME=unchanged")
	state, err := os.ReadFile(filepath.Join(harness.stateDir, "state"))
	require.NoError(t, err)
	require.Equal(t, 5, strings.Count(string(state), "\n"))
	require.NoDirExists(t, filepath.Join(harness.stateDir, "completed"))
}

// TestCertificateRenewalRunbookMalformedReceiptsFailClosed prevents corrupt or
// mismatched receipts from authorizing either a success report or fresh mutation.
func TestCertificateRenewalRunbookMalformedReceiptsFailClosed(t *testing.T) {
	for _, contents := range []string{
		"",
		"terminal-receipt-v1\nreceipt-cycle\n",
		"terminal-receipt-v2\nreceipt-cycle\nunchanged\n",
		"terminal-receipt-v1\nanother-cycle\nunchanged\n",
		"terminal-receipt-v1\nreceipt-cycle\nunknown\n",
		"terminal-receipt-v1\nreceipt-cycle\nunchanged\nextra\n",
	} {
		t.Run(fmt.Sprintf("%q", contents), func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.setEnv("OPERATION_ID", "receipt-cycle")
			output, err := harness.run(t)
			require.NoError(t, err, output)
			receiptPath := filepath.Join(harness.stateDir, "completed", fmt.Sprintf("%x", sha256.Sum256([]byte("receipt-cycle"))))
			require.NoError(t, os.WriteFile(receiptPath, []byte(contents), 0o600))
			before, err := os.ReadFile(filepath.Join(harness.stateDir, "state"))
			require.NoError(t, err)
			for _, statusOnly := range []string{"true", "false"} {
				harness.setEnv("STATUS_ONLY", statusOnly)
				harness.setEnv("FORCE", "true")
				output, err = harness.run(t)
				require.Error(t, err, output)
				require.NotContains(t, output, "RENEWAL_TERMINAL_OPERATION_ID=")
				require.NotContains(t, output, "RENEWAL_OUTCOME=")
			}
			after, err := os.ReadFile(filepath.Join(harness.stateDir, "state"))
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			require.NoFileExists(t, filepath.Join(harness.runtimeDir, "restarts"))
		})
	}
}

func TestCertificateRenewalRunbookUnknownPhaseVersionFailsClosed(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("OPERATION_ID", "unknown-version")
	state := []byte("unknown-version\nrestart_pending\netcd\nrenewed\n" + harness.caFingerprints(t) + "\nterminal-receipt-v2\n")
	statePath := filepath.Join(harness.stateDir, "state")
	require.NoError(t, os.WriteFile(statePath, state, 0o600))
	require.NoError(t, os.Rename(filepath.Join(harness.manifestsDir, "etcd.yaml"), filepath.Join(harness.stagingDir, "etcd.yaml")))

	output, err := harness.run(t)

	require.Error(t, err, output)
	require.Contains(t, output, "unknown receipt version")
	after, err := os.ReadFile(statePath)
	require.NoError(t, err)
	require.Equal(t, state, after)
	require.FileExists(t, filepath.Join(harness.stagingDir, "etcd.yaml"))
	require.NoFileExists(t, filepath.Join(harness.manifestsDir, "etcd.yaml"))
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "restarts"))
}

// TestCertificateRenewalRunbookRetryPersistsInterruptedTerminalReceipt injects
// persistence failure around receipt rename. Neither status nor success may
// acknowledge an unsynchronized receipt; a retry preserves the completed outcome
// while finishing receipt persistence, even if the retry requests Force.
func TestCertificateRenewalRunbookRetryPersistsInterruptedTerminalReceipt(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-rename=%t", afterRename), func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.setEnv("OPERATION_ID", "receipt-cycle")
			// Fail persistence after the new-format complete phase file was written.
			fakeSync := filepath.Join(filepath.Dir(harness.scriptPath), "bin", "sync")
			syncScript := "#!/bin/bash\nexit 1\n"
			if afterRename {
				syncScript = `#!/bin/bash
if [[ ! -f "$FAKE_RUNTIME_DIR/sync-called" ]]; then
  touch "$FAKE_RUNTIME_DIR/sync-called"
  exit 0
fi
exit 1
`
			}
			writeExecutable(t, fakeSync, syncScript)
			output, err := harness.run(t)
			require.Error(t, err, output)
			require.NotContains(t, output, "RENEWAL_OUTCOME=")
			harness.setEnv("STATUS_ONLY", "true")
			output, err = harness.run(t)
			require.Error(t, err, output)
			require.NotContains(t, output, "RENEWAL_TERMINAL_OPERATION_ID=")

			require.NoError(t, os.Remove(fakeSync))
			harness.setEnv("STATUS_ONLY", "false")
			harness.setEnv("FORCE", "true")
			output, err = harness.run(t)
			require.NoError(t, err, output)
			require.Contains(t, output, "RENEWAL_OUTCOME=unchanged", "the retry must preserve the original completed result")
			require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			harness.setEnv("STATUS_ONLY", "true")
			output, err = harness.run(t)
			require.NoError(t, err, output)
			require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=receipt-cycle\n", output)
		})
	}
}

// TestCertificateRenewalRunbookStatusOnlyNeverStartsOrResumes keeps cleanup
// from starting or resuming renewal even when Force or a resumable phase is
// present. An old phase file for this operation is not a substitute for a durable
// receipt; without one, only an abandoned receipt may be recorded.
func TestCertificateRenewalRunbookStatusOnlyNeverStartsOrResumes(t *testing.T) {
	for _, state := range []string{"missing", "renewing", "restart_pending", "mismatched", "malformed", "complete"} {
		t.Run(state, func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.setEnv("OPERATION_ID", "status-cycle")
			harness.setEnv("STATUS_ONLY", "true")
			harness.setEnv("FORCE", "true")
			require.NoError(t, os.WriteFile(filepath.Join(harness.stateDir, "lock"), nil, 0o600))
			statePath := filepath.Join(harness.stateDir, "state")
			var before []byte
			if state != "missing" {
				operationID, phase, component := "status-cycle", state, ""
				if state == "mismatched" {
					operationID, phase = "another-cycle", "complete"
				}
				if state == "restart_pending" {
					component = "etcd"
					require.NoError(t, os.Rename(filepath.Join(harness.manifestsDir, "etcd.yaml"), filepath.Join(harness.stagingDir, "etcd.yaml")))
				}
				before = []byte(operationID + "\n" + phase + "\n" + component + "\nrenewed\n" + harness.caFingerprints(t) + "\n")
				require.NoError(t, os.WriteFile(statePath, before, 0o600))
			}

			output, err := harness.run(t)

			if state == "missing" || state == "mismatched" {
				require.NoError(t, err, output)
				require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=status-cycle\nRENEWAL_TERMINAL_OUTCOME=abandoned\n", output)
			} else {
				require.Error(t, err, output)
				require.NotContains(t, output, "RENEWAL_TERMINAL_OPERATION_ID=", "even an old complete phase file is not fencing evidence")
				require.NoDirExists(t, filepath.Join(harness.stateDir, "completed"))
			}
			require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			require.NoFileExists(t, filepath.Join(harness.runtimeDir, "restarts"))
			if state == "missing" {
				require.NoFileExists(t, statePath)
			} else {
				after, readErr := os.ReadFile(statePath)
				require.NoError(t, readErr)
				require.Equal(t, before, after)
			}
			if state == "restart_pending" {
				require.FileExists(t, filepath.Join(harness.stagingDir, "etcd.yaml"))
				require.NoFileExists(t, filepath.Join(harness.manifestsDir, "etcd.yaml"))
			}
		})
	}
}

// TestCertificateRenewalRunbookStatusOnlyAbandonsFailedOperation lets cleanup
// resolve a receipt-aware attempt that failed before writing any phase state.
// The abandoned receipt is idempotent and makes a delayed attempt for the same
// operation fail without renewing, restarting, or touching phase state.
func TestCertificateRenewalRunbookStatusOnlyAbandonsFailedOperation(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("OPERATION_ID", "failed-cycle")
	harness.setEnv("FORCE", "true")
	harness.setEnv("FAKE_ETCD_UNHEALTHY", "true")
	output, err := harness.run(t)
	require.Error(t, err, output)
	require.Contains(t, output, "The etcd cluster is not healthy")
	statePath := filepath.Join(harness.stateDir, "state")
	require.NoFileExists(t, statePath, "preflight failed before any phase was recorded")

	harness.setEnv("STATUS_ONLY", "true")
	for range 2 {
		output, err = harness.run(t)
		require.NoError(t, err, output)
		require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=failed-cycle\nRENEWAL_TERMINAL_OUTCOME=abandoned\n", output)
	}
	receipt, err := os.ReadFile(filepath.Join(harness.stateDir, "completed", fmt.Sprintf("%x", sha256.Sum256([]byte("failed-cycle")))))
	require.NoError(t, err)
	require.Equal(t, "terminal-receipt-v1\nfailed-cycle\nabandoned\n", string(receipt))

	// A delayed attempt would now pass preflight, but must not renew.
	harness.setEnv("STATUS_ONLY", "false")
	harness.setEnv("FAKE_ETCD_UNHEALTHY", "false")
	output, err = harness.run(t)
	require.Error(t, err, output)
	require.Contains(t, output, "was abandoned")
	require.NotContains(t, output, "RENEWAL_OUTCOME=")
	require.NoFileExists(t, statePath)
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "restarts"))

	// A later operation still renews normally and resolves without the abandoned marker.
	harness.setEnv("OPERATION_ID", "next-cycle")
	output, err = harness.run(t)
	require.NoError(t, err, output)
	require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
	harness.setEnv("STATUS_ONLY", "true")
	output, err = harness.run(t)
	require.NoError(t, err, output)
	require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=next-cycle\n", output)
}

// TestCertificateRenewalRunbookStatusOnlyAbandonsWithoutStateDir resolves an
// operation whose dispatch never started the script on a node where the runbook
// never ran: status-only must still record the abandoned receipt.
func TestCertificateRenewalRunbookStatusOnlyAbandonsWithoutStateDir(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	require.NoError(t, os.RemoveAll(harness.stateDir))
	harness.setEnv("OPERATION_ID", "never-started")
	harness.setEnv("STATUS_ONLY", "true")

	output, err := harness.run(t)
	require.NoError(t, err, output)
	require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=never-started\nRENEWAL_TERMINAL_OUTCOME=abandoned\n", output)
	info, err := os.Stat(harness.stateDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	require.NoFileExists(t, filepath.Join(harness.stateDir, "state"))
	receipt, err := os.ReadFile(filepath.Join(harness.stateDir, "completed", fmt.Sprintf("%x", sha256.Sum256([]byte("never-started")))))
	require.NoError(t, err)
	require.Equal(t, "terminal-receipt-v1\nnever-started\nabandoned\n", string(receipt))
}

// TestCertificateRenewalRunbookNextOperationResumesAbandonedRenewal releases an
// operation whose restart failed after renewal: the next operation resumes and
// completes its pending restarts, while the abandoned operation stays fenced.
func TestCertificateRenewalRunbookNextOperationResumesAbandonedRenewal(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("OPERATION_ID", "half-done")
	harness.setEnv("FORCE", "true")
	harness.setEnv("FAKE_STUCK_COMPONENT", "kube-apiserver")
	output, err := harness.run(t)
	require.Error(t, err, output)
	state, err := os.ReadFile(filepath.Join(harness.stateDir, "state"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(state), "half-done\nrestart_pending\nkube-apiserver\n"), string(state))

	harness.setEnv("STATUS_ONLY", "true")
	output, err = harness.run(t)
	require.NoError(t, err, output)
	require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=half-done\nRENEWAL_TERMINAL_OUTCOME=abandoned\n", output)

	harness.setEnv("STATUS_ONLY", "false")
	harness.setEnv("FAKE_STUCK_COMPONENT", "")
	output, err = harness.run(t)
	require.Error(t, err, output)
	require.Contains(t, output, "was abandoned")

	harness.setEnv("OPERATION_ID", "next-cycle")
	harness.setEnv("FORCE", "false")
	output, err = harness.run(t)
	require.NoError(t, err, output)
	require.Contains(t, output, "Resuming certificate renewal operation 'half-done' at static pod 'kube-apiserver'.")
	require.True(t, strings.HasSuffix(strings.TrimSpace(output), "RENEWAL_OUTCOME=unchanged"), output)
	restarts, err := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
	require.NoError(t, err)
	require.Equal(t, "etcd\nkube-apiserver\nkube-controller-manager\nkube-scheduler\n", string(restarts))
	harness.setEnv("STATUS_ONLY", "true")
	output, err = harness.run(t)
	require.NoError(t, err, output)
	require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=next-cycle\n", output)
	harness.setEnv("OPERATION_ID", "half-done")
	output, err = harness.run(t)
	require.NoError(t, err, output)
	require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=half-done\nRENEWAL_TERMINAL_OUTCOME=abandoned\n", output)
}

// TestCertificateRenewalRunbookStatusOnlyExcludesActiveExitRestoration requires
// both a receipt and acquisition of the renewal flock: an EXIT handler can still
// be restoring files after the main script body has finished.
func TestCertificateRenewalRunbookStatusOnlyExcludesActiveExitRestoration(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("OPERATION_ID", "status-cycle")
	// Obtain a real terminal receipt before testing exclusion of a still-held flock.
	output, err := harness.run(t)
	require.NoError(t, err, output)
	harness.setEnv("STATUS_ONLY", "true")
	harness.setEnv("LOCK_TIMEOUT", "0")
	// A terminal receipt alone is insufficient while an EXIT handler still owns
	// the same flock. Model restoration holding that lock until explicitly released.
	restoring := filepath.Join(harness.runtimeDir, "restoring")
	release := filepath.Join(harness.runtimeDir, "release")
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `
exec 9>"$STATE_DIR/lock"
flock 9
trap 'touch "$FAKE_RUNTIME_DIR/restoring"; while [[ ! -f "$FAKE_RUNTIME_DIR/release" ]]; do sleep 0.01; done' EXIT
exit 1
`)
	cmd.Env = harness.env
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = cmd.Wait()
	})
	require.Eventually(t, func() bool { _, err := os.Stat(restoring); return err == nil }, 2*time.Second, 10*time.Millisecond)

	output, err = harness.run(t)

	require.Error(t, err, output)
	require.Contains(t, output, "Timed out waiting for certificate-renewal termination")
	require.NotContains(t, output, "RENEWAL_TERMINAL_OPERATION_ID=")
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	require.Error(t, cmd.Wait(), "the simulated renewal exits unsuccessfully after restoration")
	output, err = harness.run(t)
	require.NoError(t, err, output)
	require.Equal(t, "RENEWAL_TERMINAL_OPERATION_ID=status-cycle\n", output)
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "restarts"))
}

func TestCertificateRenewalRunbookNoOpIgnoresCATable(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(harness.env, "FAKE_RESIDUAL=365d", "OPERATION_ID=no-op")

	output, err := harness.run(t)

	require.NoError(t, err, output)
	require.Contains(t, output, "RENEWAL_OUTCOME=unchanged")
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "restarts"))
}

// TestCertificateRenewalRunbookParsesHumanDurations covers the compound units
// kubeadm prints (apimachinery HumanDuration) against the default 7-day threshold.
func TestCertificateRenewalRunbookParsesHumanDurations(t *testing.T) {
	for _, tt := range []struct {
		residual string
		renews   bool
	}{
		{residual: "45s", renews: true},
		{residual: "2m30s", renews: true},
		{residual: "5h10m", renews: true},
		{residual: "3d4h", renews: true},
		{residual: "6d23h", renews: true},
		{residual: "7d", renews: false},
		{residual: "7d1h", renews: false},
		{residual: "400d", renews: false},
		{residual: "2y1d", renews: false},
		{residual: "8y", renews: false},
		{residual: "<invalid>", renews: true},
	} {
		t.Run(tt.residual, func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.env = append(harness.env, "FAKE_RESIDUAL="+tt.residual, "OPERATION_ID=human-duration")

			output, err := harness.run(t)

			require.NoError(t, err, output)
			if tt.renews {
				require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
				require.FileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			} else {
				require.Contains(t, output, "RENEWAL_OUTCOME=unchanged")
				require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			}
		})
	}
}

func TestCertificateRenewalRunbookFailsClosedOnUnknownExpirationFormat(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(harness.env, "FAKE_RESIDUAL=unknown", "OPERATION_ID=unknown-format")

	output, err := harness.run(t)

	require.Error(t, err, output)
	require.Contains(t, output, "Unable to parse kubeadm certificate expiration output safely")
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
}

func TestCertificateRenewalRunbookMissingCertificateFailsClosed(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(harness.env, "FAKE_MISSING_CERT=admin.conf", "OPERATION_ID=missing-cert")

	output, err := harness.run(t)

	require.Error(t, err, output)
	require.Contains(t, output, "Unable to parse kubeadm certificate expiration output safely")
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
}

// Joined control-plane nodes never have super-admin.conf; kubeadm reports it
// missing on every healthy joined node, so selection must still succeed.
func TestCertificateRenewalRunbookJoinedControlPlaneMissingSuperAdmin(t *testing.T) {
	for _, tt := range []struct {
		name     string
		residual string
		renews   bool
	}{
		{name: "fresh", residual: "365d", renews: false},
		{name: "due", residual: "1d", renews: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.env = append(harness.env, "FAKE_MISSING_CERT=super-admin.conf", "FAKE_RESIDUAL="+tt.residual, "OPERATION_ID=joined-node")

			output, err := harness.run(t)

			require.NoError(t, err, output)
			if tt.renews {
				require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
				require.FileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			} else {
				require.Contains(t, output, "RENEWAL_OUTCOME=unchanged")
				require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			}
		})
	}

	t.Run("check-only", func(t *testing.T) {
		harness := newCertificateRenewalHarness(t)
		harness.setEnv("CHECK_ONLY", "true")
		harness.setEnv("FAKE_MISSING_CERT", "super-admin.conf")
		harness.setEnv("FAKE_RESIDUAL", "365d")

		output, err := harness.run(t)

		require.NoError(t, err, output)
		require.Equal(t, "RENEWAL_REQUIRED=false\n", output)
	})
}

// A table whose only leaf row is the tolerated missing super-admin.conf has no
// certificate to judge, so it is not a healthy node and selection must fail closed.
func TestCertificateRenewalRunbookOnlyMissingSuperAdminFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name      string
		checkOnly bool
	}{
		{name: "renewal", checkOnly: false},
		{name: "check-only", checkOnly: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.setEnv("CHECK_ONLY", fmt.Sprintf("%t", tt.checkOnly))
			harness.env = append(harness.env, "FAKE_MISSING_CERT=super-admin.conf", "FAKE_OMIT_ADMIN=true", "OPERATION_ID=only-super-admin")

			output, err := harness.run(t)

			require.Error(t, err, output)
			require.Contains(t, output, "Unable to parse kubeadm certificate expiration output safely")
			require.NotContains(t, output, "RENEWAL_REQUIRED=")
			require.NotContains(t, output, "RENEWAL_OUTCOME=")
			require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
		})
	}
}

// kubeadm renders the table with tabwriter, which can leave trailing padding
// after the last column; it must not change how a row parses.
func TestCertificateRenewalRunbookToleratesTrailingRowPadding(t *testing.T) {
	for _, tt := range []struct {
		name     string
		residual string
		renews   bool
	}{
		{name: "fresh", residual: "365d", renews: false},
		{name: "due", residual: "1d", renews: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.env = append(harness.env, "FAKE_ROW_PADDING=   ", "FAKE_RESIDUAL="+tt.residual, "OPERATION_ID=row-padding")

			output, err := harness.run(t)

			require.NoError(t, err, output)
			if tt.renews {
				require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
				require.FileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			} else {
				require.Contains(t, output, "RENEWAL_OUTCOME=unchanged")
				require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
			}
		})
	}
}

func TestCertificateRenewalRunbookRenewsCertificatesInsideThreshold(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(harness.env, "FAKE_RESIDUAL=1d", "OPERATION_ID=scheduled-due")

	output, err := harness.run(t)

	require.NoError(t, err, output)
	require.Contains(t, output, "admin.conf 1d")
	require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
	renewed, err := os.ReadFile(filepath.Join(harness.runtimeDir, "renewed"))
	require.NoError(t, err)
	require.Equal(t, "renew\n", string(renewed))
}

func TestCertificateRenewalRunbookForceRestartsStaticPodsSequentially(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(harness.env, "FAKE_RESIDUAL=365d", "FORCE=true", "OPERATION_ID=forced-cycle")

	output, err := harness.run(t)

	require.NoError(t, err, output)
	require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
	renewed, err := os.ReadFile(filepath.Join(harness.runtimeDir, "renewed"))
	require.NoError(t, err)
	require.Equal(t, "renew\n", string(renewed))
	restarts, err := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
	require.NoError(t, err)
	require.Equal(t, "etcd\nkube-apiserver\nkube-controller-manager\nkube-scheduler\n", string(restarts))
	for _, component := range certificateRenewalComponents {
		require.FileExists(t, filepath.Join(harness.manifestsDir, component+".yaml"))
	}
}

func TestCertificateRenewalRunbookForceBypassesExpirationParsing(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(harness.env, "FAKE_RESIDUAL=unknown", "FORCE=true", "OPERATION_ID=forced-unknown-format")

	output, err := harness.run(t)

	require.NoError(t, err, output)
	require.Contains(t, output, "unknown")
	require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
	renewed, err := os.ReadFile(filepath.Join(harness.runtimeDir, "renewed"))
	require.NoError(t, err)
	require.Equal(t, "renew\n", string(renewed))
	restarts, err := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
	require.NoError(t, err)
	require.Equal(t, "etcd\nkube-apiserver\nkube-controller-manager\nkube-scheduler\n", string(restarts))
}

// TestCertificateRenewalRunbookWaitsForPostRestartHealth gives replacement
// containers time to become ready but keeps restart intent on persistent failure.
// Restored files must not produce a successful outcome while probes still fail.
func TestCertificateRenewalRunbookWaitsForPostRestartHealth(t *testing.T) {
	for _, probe := range []struct {
		name      string
		component string
		message   string
	}{
		{name: "member-list", component: "etcd", message: "Cannot read the etcd membership list"},
		{name: "endpoint-health", component: "etcd", message: "The etcd cluster is not healthy"},
		{name: "readyz", component: "kube-apiserver", message: "The local Kubernetes API is not ready"},
	} {
		for _, persistent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/persistent=%t", probe.name, persistent), func(t *testing.T) {
				harness := newCertificateRenewalHarness(t)
				harness.setEnv("FORCE", "true")
				harness.setEnv("OPERATION_ID", "post-restart-health")
				harness.setEnv("FAKE_HEALTH_PROBE", probe.name)
				harness.setEnv("FAKE_HEALTH_COMPONENT", probe.component)
				harness.setEnv("FAKE_HEALTH_FAILURES", "2")
				harness.setEnv("START_TIMEOUT", "5")
				if persistent {
					harness.setEnv("FAKE_HEALTH_FAILURES", "-1")
					harness.setEnv("START_TIMEOUT", "1")
				}
				started := time.Now()

				output, err := harness.run(t)

				if persistent {
					require.Error(t, err, output)
					require.Contains(t, output, "Timed out waiting for '"+probe.component+"' to become ready")
					require.NotContains(t, output, "RENEWAL_OUTCOME=renewed")
					require.GreaterOrEqual(t, time.Since(started), time.Second)
					state, readErr := os.ReadFile(filepath.Join(harness.stateDir, "state"))
					require.NoError(t, readErr)
					require.Contains(t, string(state), "\nrestart_pending\n"+probe.component+"\n")
				} else {
					require.NoError(t, err, output)
					require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
					restarts, readErr := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
					require.NoError(t, readErr)
					require.Equal(t, "etcd\nkube-apiserver\nkube-controller-manager\nkube-scheduler\n", string(restarts))
				}
				failures := strings.Count(output, probe.message)
				if persistent {
					require.Greater(t, failures, 1, output)
				} else {
					require.Equal(t, 2, failures, output)
				}
				renewed, err := os.ReadFile(filepath.Join(harness.runtimeDir, "renewed"))
				require.NoError(t, err)
				require.Equal(t, "renew\n", string(renewed))
				for _, component := range certificateRenewalComponents {
					require.FileExists(t, filepath.Join(harness.manifestsDir, component+".yaml"))
					require.NoFileExists(t, filepath.Join(harness.stagingDir, component+".yaml"))
				}
			})
		}
	}
}

// TestCertificateRenewalRunbookFailedCRILookupIsNotAStoppedContainer fails the
// runtime lookup while the manifest is staged. The wait must keep polling until
// the runtime confirms the old container is gone instead of restoring the
// manifest early; a persistent failure must time out waiting to stop.
func TestCertificateRenewalRunbookFailedCRILookupIsNotAStoppedContainer(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%t", persistent), func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.setEnv("FORCE", "true")
			harness.setEnv("OPERATION_ID", "cri-lookup-failure")
			harness.setEnv("FAKE_CRICTL_PS_FAIL_COMPONENT", "etcd")
			harness.setEnv("FAKE_CRICTL_PS_FAILURES", "2")
			if persistent {
				harness.setEnv("FAKE_CRICTL_PS_FAILURES", "-1")
				harness.setEnv("STOP_TIMEOUT", "1")
			}

			output, err := harness.run(t)

			if persistent {
				require.Error(t, err, output)
				require.Contains(t, output, "Timed out waiting for 'etcd' to stop")
			} else {
				require.NoError(t, err, output)
				require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
				restarts, readErr := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
				require.NoError(t, readErr)
				require.Equal(t, "etcd\nkube-apiserver\nkube-controller-manager\nkube-scheduler\n", string(restarts))
			}
			failures, readErr := os.ReadFile(filepath.Join(harness.runtimeDir, "crictl-ps-failures"))
			require.NoError(t, readErr)
			if persistent {
				require.NotEqual(t, "1\n", string(failures), "a persistent failure must be polled more than once")
			} else {
				require.Equal(t, "2\n", string(failures))
			}
			for _, component := range certificateRenewalComponents {
				require.FileExists(t, filepath.Join(harness.manifestsDir, component+".yaml"))
				require.NoFileExists(t, filepath.Join(harness.stagingDir, component+".yaml"))
			}
		})
	}
}

// TestCertificateRenewalRunbookResumesRestartWithoutRenewingAgain protects the
// persisted restart boundary: fresh certificates outside the threshold must not
// skip unfinished component restarts, nor trigger another renewal.
func TestCertificateRenewalRunbookResumesRestartWithoutRenewingAgain(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	fingerprints := harness.caFingerprints(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(harness.stateDir, "state"),
		[]byte("resumed-cycle\nrestart_pending\nkube-apiserver\nrenewed\n"+fingerprints+"\n"),
		0o600,
	))
	harness.env = append(harness.env, "FAKE_RESIDUAL=365d", "OPERATION_ID=resumed-cycle")
	require.NoError(t, os.WriteFile(filepath.Join(harness.runtimeDir, "kube-apiserver.generation"), []byte("1\n"), 0o600))
	harness.setEnv("FAKE_HEALTH_PROBE", "readyz")
	harness.setEnv("FAKE_HEALTH_COMPONENT", "kube-apiserver")
	harness.setEnv("FAKE_HEALTH_FAILURES", "2")

	output, err := harness.run(t)

	require.NoError(t, err, output)
	require.Equal(t, 2, strings.Count(output, "The local Kubernetes API is not ready"))
	require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
	restarts, err := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
	require.NoError(t, err)
	require.Equal(t, "kube-apiserver\nkube-controller-manager\nkube-scheduler\n", string(restarts))
}

func TestCertificateRenewalRunbookRestoresManifestAfterRestartFailure(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(
		harness.env,
		"FAKE_RESIDUAL=1d",
		"FAKE_STUCK_COMPONENT=etcd",
		"OPERATION_ID=failing-cycle",
		"STOP_TIMEOUT=0",
		"POLL_INTERVAL=0",
	)

	output, err := harness.run(t)

	require.Error(t, err)
	require.Contains(t, output, "Timed out waiting for 'etcd' to stop")
	require.FileExists(t, filepath.Join(harness.manifestsDir, "etcd.yaml"))
	require.NoFileExists(t, filepath.Join(harness.stagingDir, "etcd.yaml"))
}

func TestCertificateRenewalRunbookRetryAfterRestartFailureConverges(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("FAKE_RESIDUAL", "1d")
	harness.setEnv("FAKE_STUCK_COMPONENT", "etcd")
	harness.setEnv("OPERATION_ID", "retry-cycle")
	harness.setEnv("STOP_TIMEOUT", "0")

	firstOutput, firstErr := harness.run(t)
	require.Error(t, firstErr, firstOutput)
	require.Contains(t, firstOutput, "Timed out waiting for 'etcd' to stop")

	harness.setEnv("FAKE_STUCK_COMPONENT", "")
	harness.setEnv("STOP_TIMEOUT", "2")
	secondOutput, secondErr := harness.run(t)

	require.NoError(t, secondErr, secondOutput)
	require.Contains(t, secondOutput, "RENEWAL_OUTCOME=renewed")
	renewed, err := os.ReadFile(filepath.Join(harness.runtimeDir, "renewed"))
	require.NoError(t, err)
	require.Equal(t, "renew\n", string(renewed), "retry must not renew certificates a second time")
	restarts, err := os.ReadFile(filepath.Join(harness.runtimeDir, "restarts"))
	require.NoError(t, err)
	require.Equal(t, "etcd\nkube-apiserver\nkube-controller-manager\nkube-scheduler\n", string(restarts))
}

func TestCertificateRenewalRunbookResumesRenewingPhase(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	fingerprints := harness.caFingerprints(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(harness.stateDir, "state"),
		[]byte("renewing-cycle\nrenewing\n\nrenewed\n"+fingerprints+"\n"),
		0o600,
	))
	harness.setEnv("OPERATION_ID", "renewing-cycle")

	output, err := harness.run(t)

	require.NoError(t, err, output)
	require.Contains(t, output, "Resuming certificate renewal operation 'renewing-cycle' from renewal")
	require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
	require.FileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
}

func TestCertificateRenewalRunbookConcurrentRetryWaitsForCompletion(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("FAKE_RESIDUAL", "1d")
	harness.setEnv("FAKE_RENEW_SLEEP", "1")
	harness.setEnv("OPERATION_ID", "concurrent-cycle")

	firstCmd := exec.CommandContext(t.Context(), "bash", harness.scriptPath)
	firstCmd.Env = harness.env
	var firstOutput bytes.Buffer
	firstCmd.Stdout = &firstOutput
	firstCmd.Stderr = &firstOutput
	require.NoError(t, firstCmd.Start())
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(harness.runtimeDir, "renewed"))
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)

	started := time.Now()
	secondOutput, secondErr := harness.run(t)
	elapsed := time.Since(started)
	firstErr := firstCmd.Wait()

	require.NoError(t, firstErr, firstOutput.String())
	require.NoError(t, secondErr, secondOutput)
	require.GreaterOrEqual(t, elapsed, 500*time.Millisecond)
	require.Contains(t, secondOutput, "already completed")
	require.Contains(t, secondOutput, "RENEWAL_OUTCOME=renewed")
}

func TestCertificateRenewalRunbookRejectsCAChanges(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(
		harness.env,
		"FAKE_RESIDUAL=1d",
		"FAKE_CHANGE_CA=true",
		"OPERATION_ID=ca-change-cycle",
	)

	output, err := harness.run(t)

	require.Error(t, err)
	require.Contains(t, output, "A Kubernetes CA fingerprint changed")
}

// TestCertificateRenewalRunbookRenewsSingleVoterEtcd shows a one-voter cluster is
// exempt from the lose-one-voter rule, matching the controller health gate.
func TestCertificateRenewalRunbookRenewsSingleVoterEtcd(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("EXPECTED_ETCD_MEMBERS", "1")
	harness.env = append(harness.env, "FAKE_RESIDUAL=1d", "FAKE_ETCD_MEMBER_COUNT=1", "OPERATION_ID=single-voter")

	output, err := harness.run(t)

	require.NoError(t, err, output)
	require.Contains(t, output, "RENEWAL_OUTCOME=renewed")
	require.FileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
}

// TestCertificateRenewalRunbookTwoVotersCannotTolerateLoss keeps the multi-node
// quorum gate: two voters cannot lose one.
func TestCertificateRenewalRunbookTwoVotersCannotTolerateLoss(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.setEnv("EXPECTED_ETCD_MEMBERS", "2")
	harness.env = append(harness.env, "FAKE_RESIDUAL=1d", "FAKE_ETCD_MEMBER_COUNT=2", "OPERATION_ID=two-voters")

	output, err := harness.run(t)

	require.Error(t, err)
	require.Contains(t, output, "cannot tolerate one unavailable member")
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
}

func TestCertificateRenewalRunbookEtcdMembershipMismatchPreventsMutation(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(
		harness.env,
		"FAKE_RESIDUAL=1d",
		"FAKE_ETCD_MEMBER_COUNT=2",
		"OPERATION_ID=membership-mismatch",
	)

	output, err := harness.run(t)

	require.Error(t, err)
	require.Contains(t, output, "does not match the control-plane snapshot")
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
}

// TestCertificateRenewalRunbookEtcdLearnerPreventsMutation rejects non-voting
// members even when the total member count matches the control-plane snapshot.
func TestCertificateRenewalRunbookEtcdLearnerPreventsMutation(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(
		harness.env,
		"FAKE_RESIDUAL=1d",
		"FAKE_ETCD_LEARNER=true",
		"OPERATION_ID=learner-member",
	)

	output, err := harness.run(t)

	require.Error(t, err)
	require.Contains(t, output, "contains a non-voting member")
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
}

func TestCertificateRenewalRunbookEtcdFailurePreventsMutation(t *testing.T) {
	harness := newCertificateRenewalHarness(t)
	harness.env = append(
		harness.env,
		"FAKE_RESIDUAL=1d",
		"FAKE_ETCD_UNHEALTHY=true",
		"OPERATION_ID=unhealthy-cycle",
	)

	output, err := harness.run(t)

	require.Error(t, err)
	require.Contains(t, output, "The etcd cluster is not healthy")
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
	require.NoFileExists(t, filepath.Join(harness.runtimeDir, "restarts"))
}

var certificateRenewalComponents = []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"}

// certificateRenewalHarness runs the embedded Bash script against real temporary
// files and flock, replacing Kubernetes commands with manifest-driven fakes. It
// exercises restart/receipt ordering, not real kubelet or etcd recovery.
type certificateRenewalHarness struct {
	scriptPath    string
	kubernetesDir string
	manifestsDir  string
	stateDir      string
	stagingDir    string
	runtimeDir    string
	env           []string
	agentState    string
}

func newCertificateRenewalHarness(t *testing.T) *certificateRenewalHarness {
	t.Helper()
	for _, command := range []string{"bash", "flock", "jq", "timeout"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s is required to exercise the Linux certificate-renewal runbook", command)
		}
	}

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	kubernetesDir := filepath.Join(root, "kubernetes")
	manifestsDir := filepath.Join(kubernetesDir, "manifests")
	stateDir := filepath.Join(root, "state")
	stagingDir := filepath.Join(kubernetesDir, "staging")
	runtimeDir := filepath.Join(root, "runtime")
	for _, directory := range []string{binDir, manifestsDir, stateDir, stagingDir, runtimeDir, filepath.Join(kubernetesDir, "pki", "etcd")} {
		require.NoError(t, os.MkdirAll(directory, 0o755))
	}
	for _, component := range certificateRenewalComponents {
		require.NoError(t, os.WriteFile(filepath.Join(manifestsDir, component+".yaml"), []byte(component), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(runtimeDir, component+".generation"), []byte("0\n"), 0o600))
	}
	for path, contents := range map[string]string{
		filepath.Join(kubernetesDir, "admin.conf"):                "admin",
		filepath.Join(kubernetesDir, "pki", "ca.crt"):             "cluster-ca",
		filepath.Join(kubernetesDir, "pki", "front-proxy-ca.crt"): "front-proxy-ca",
		filepath.Join(kubernetesDir, "pki", "etcd", "ca.crt"):     "etcd-ca",
		filepath.Join(kubernetesDir, "pki", "etcd", "server.crt"): "etcd-server",
		filepath.Join(kubernetesDir, "pki", "etcd", "server.key"): "etcd-key",
	} {
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	}

	script, err := Script("", "k8s-cert-renewal")
	require.NoError(t, err)
	scriptPath := filepath.Join(root, "k8s-cert-renewal.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o700))
	writeExecutable(t, filepath.Join(binDir, "kubeadm"), fakeKubeadm)
	writeExecutable(t, filepath.Join(binDir, "kubectl"), "#!/bin/bash\nexec fake-health readyz\n")
	writeExecutable(t, filepath.Join(binDir, "crictl"), fakeCrictl)
	writeExecutable(t, filepath.Join(binDir, "fake-health"), fakeCertificateRenewalHealth)

	return &certificateRenewalHarness{
		scriptPath:    scriptPath,
		kubernetesDir: kubernetesDir,
		manifestsDir:  manifestsDir,
		stateDir:      stateDir,
		stagingDir:    stagingDir,
		runtimeDir:    runtimeDir,
		agentState:    filepath.Join(root, "agent-state.json"),
		env: append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"KUBERNETES_DIR="+kubernetesDir,
			"MANIFESTS_DIR="+manifestsDir,
			"LOG_FILE="+filepath.Join(root, "renewal.log"),
			"STATE_DIR="+stateDir,
			"STAGING_DIR="+stagingDir,
			"AGENT_STATE_FILE="+filepath.Join(root, "agent-state.json"),
			"FAKE_RUNTIME_DIR="+runtimeDir,
			"EXPECTED_ETCD_MEMBERS=3",
			"LOCK_TIMEOUT=5",
			"START_TIMEOUT=2",
			"STOP_TIMEOUT=2",
			"POLL_INTERVAL=0",
		),
	}
}

func (h *certificateRenewalHarness) run(t *testing.T) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", h.scriptPath)
	cmd.Env = h.env
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (h *certificateRenewalHarness) setEnv(key, value string) {
	prefix := key + "="
	for index, entry := range h.env {
		if strings.HasPrefix(entry, prefix) {
			h.env[index] = prefix + value
			return
		}
	}
	h.env = append(h.env, prefix+value)
}

func (h *certificateRenewalHarness) caFingerprints(t *testing.T) string {
	t.Helper()
	paths := []string{
		filepath.Join(h.kubernetesDir, "pki", "ca.crt"),
		filepath.Join(h.kubernetesDir, "pki", "etcd", "ca.crt"),
		filepath.Join(h.kubernetesDir, "pki", "front-proxy-ca.crt"),
	}
	fingerprints := make([]string, 0, len(paths))
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		require.NoError(t, err)
		fingerprints = append(fingerprints, fmt.Sprintf("%x", sha256.Sum256(contents)))
	}
	return strings.Join(fingerprints, ":")
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o700))
}

const fakeKubeadm = `#!/bin/bash
set -euo pipefail
if [[ "$*" == "certs check-expiration" ]]; then
  cat <<EOF
CERTIFICATE                EXPIRES                  RESIDUAL TIME   CERTIFICATE AUTHORITY   EXTERNALLY MANAGED
EOF
  if [[ "${FAKE_MISSING_CERT:-}" != "admin.conf" && "${FAKE_OMIT_ADMIN:-}" != "true" ]]; then
    echo "admin.conf                 Sep 04, 2027 12:00 UTC   ${FAKE_RESIDUAL:-365d}            ca                      no${FAKE_ROW_PADDING:-}"
  fi
  if [[ -n "${FAKE_MISSING_CERT:-}" ]]; then
    echo "!MISSING! ${FAKE_MISSING_CERT}"
  fi
  cat <<EOF
CERTIFICATE AUTHORITY      EXPIRES                  RESIDUAL TIME   EXTERNALLY MANAGED
ca                         Sep 04, 2026 12:00 UTC   1d              no
EOF
  exit 0
fi
if [[ "$*" == "certs renew all" ]]; then
  echo renew >> "${FAKE_RUNTIME_DIR}/renewed"
  sleep "${FAKE_RENEW_SLEEP:-0}"
  if [[ "${FAKE_CHANGE_CA:-false}" == "true" ]]; then
    echo changed-ca > "${KUBERNETES_DIR}/pki/ca.crt"
  fi
  exit 0
fi
exit 1
`

const fakeCertificateRenewalHealth = `#!/bin/bash
set -euo pipefail
[[ "${FAKE_HEALTH_PROBE:-}" == "$1" ]] || exit 0
generation="$(cat "${FAKE_RUNTIME_DIR}/${FAKE_HEALTH_COMPONENT}.generation")"
(( generation > 0 )) || exit 0
attempts_file="${FAKE_RUNTIME_DIR}/health-attempts"
attempts=0
if [[ -f "$attempts_file" ]]; then
  attempts="$(cat "$attempts_file")"
fi
attempts=$(( attempts + 1 ))
echo "$attempts" > "$attempts_file"
if (( FAKE_HEALTH_FAILURES < 0 || attempts <= FAKE_HEALTH_FAILURES )); then
  exit 1
fi
`

const fakeCrictl = `#!/bin/bash
set -euo pipefail
command="$1"
shift
case "$command" in
  ps)
    component=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --label) component="${2#io.kubernetes.container.name=}"; shift 2 ;;
        -q|--quiet) shift ;;
        *) shift ;;
      esac
    done
    generation_file="${FAKE_RUNTIME_DIR}/${component}.generation"
    manifest="${MANIFESTS_DIR}/${component}.yaml"
    missing="${FAKE_RUNTIME_DIR}/${component}.missing"
    failures_file="${FAKE_RUNTIME_DIR}/crictl-ps-failures"
    failures="$(cat "$failures_file" 2>/dev/null || echo 0)"
    if [[ "${FAKE_CRICTL_PS_FAIL_COMPONENT:-}" == "$component" && ! -f "$manifest" ]] &&
      (( FAKE_CRICTL_PS_FAILURES < 0 || failures < FAKE_CRICTL_PS_FAILURES )); then
      echo "$(( failures + 1 ))" > "$failures_file"
      echo "runtime service unavailable" >&2
      exit 1
    fi
    if [[ "${FAKE_STUCK_COMPONENT:-}" == "$component" ]]; then
      generation="$(tr -d '\n' < "$generation_file")"
      echo "${component}__${generation}"
      exit 0
    fi
    if [[ ! -f "$manifest" ]]; then
      touch "$missing"
      exit 0
    fi
    if [[ -f "$missing" ]]; then
      generation="$(( $(tr -d '\n' < "$generation_file") + 1 ))"
      echo "$generation" > "$generation_file"
      rm -f "$missing"
      echo "$component" >> "${FAKE_RUNTIME_DIR}/restarts"
    fi
    generation="$(tr -d '\n' < "$generation_file")"
    echo "${component}__${generation}"
    ;;
  inspect)
    container_id="$1"
    component="${container_id%%__*}"
    printf '{"status":{"labels":{"io.kubernetes.pod.namespace":"kube-system","io.kubernetes.pod.name":"%s-node"}}}\n' "$component"
    ;;
  exec)
    if [[ "$*" == *"member list --write-out=json"* ]]; then
      fake-health member-list || exit 1
      count="${FAKE_ETCD_MEMBER_COUNT:-3}"
      printf '{"members":['
      for (( index = 0; index < count; index++ )); do
        (( index == 0 )) || printf ','
        if [[ "${FAKE_ETCD_LEARNER:-false}" == "true" && "$index" -eq $(( count - 1 )) ]]; then
          printf '{"ID":%d,"isLearner":true}' "$index"
        else
          printf '{"ID":%d}' "$index"
        fi
      done
      printf ']}\n'
      exit 0
    fi
    if [[ "${FAKE_ETCD_UNHEALTHY:-false}" == "true" ]]; then
      exit 1
    fi
    fake-health endpoint-health || exit 1
    exit 0
    ;;
  *) exit 1 ;;
esac
`

// Check-only selection cannot resume a staged renewal, write state/logs, or
// acquire a lock, even when Force is set or expiration parsing fails. Persisted
// renewing/restart_pending intent from either executor requires renewal even
// with fresh certificates, because only a renewal attempt resumes it; the
// runbook's own intent selects the legacy executor, the only one that reads it,
// followed by the typed executor when the agent also holds pending intent.
func TestCertificateRenewalRunbookCheckOnlyIsReadOnly(t *testing.T) {
	runbookStates := map[string]string{
		"none":            "",
		"restart_pending": "pending-cycle\nrestart_pending\netcd\nrenewed\n",
		"renewing":        "pending-cycle\nrenewing\n\nrenewed\n",
		"complete":        "done-cycle\ncomplete\n\nrenewed\n",
	}
	agentStates := map[string]string{
		"missing":         "",
		"no_renewal":      `{"name":"node","status":"ready"}`,
		"restart_pending": `{"name":"node","k8sCertRenewalState":"restart_pending"}`,
		"renewing":        `{"name":"node","k8sCertRenewalState":"renewing"}`,
	}
	for runbookName, runbookState := range runbookStates {
		for agentName, agentState := range agentStates {
			for _, residual := range []string{"365d", "1d", "unknown"} {
				for _, force := range []bool{false, true} {
					t.Run(fmt.Sprintf("runbook=%s/agent=%s/%s/force=%t", runbookName, agentName, residual, force), func(t *testing.T) {
						harness := newCertificateRenewalHarness(t)
						harness.setEnv("CHECK_ONLY", "true")
						harness.setEnv("FORCE", fmt.Sprint(force))
						harness.setEnv("FAKE_RESIDUAL", residual)
						statePath := filepath.Join(harness.stateDir, "state")
						state := []byte(runbookState + harness.caFingerprints(t) + "\n")
						if runbookState != "" {
							require.NoError(t, os.WriteFile(statePath, state, 0o600))
						} else {
							require.NoError(t, os.RemoveAll(harness.stateDir))
						}
						if runbookName == "restart_pending" {
							require.NoError(t, os.Rename(filepath.Join(harness.manifestsDir, "etcd.yaml"), filepath.Join(harness.stagingDir, "etcd.yaml")))
						} else {
							require.NoError(t, os.RemoveAll(harness.stagingDir))
						}
						if agentState != "" {
							require.NoError(t, os.WriteFile(harness.agentState, []byte(agentState), 0o600))
						}
						legacyIntent := runbookName == "renewing" || runbookName == "restart_pending"
						pendingIntent := legacyIntent || agentName == "renewing" || agentName == "restart_pending"
						output, err := harness.run(t)
						switch {
						case legacyIntent && (agentName == "renewing" || agentName == "restart_pending"):
							require.NoError(t, err, output)
							require.Equal(t, "RENEWAL_REQUIRED=legacy_then_typed\n", output)
						case legacyIntent:
							require.NoError(t, err, output)
							require.Equal(t, "RENEWAL_REQUIRED=legacy\n", output)
						case pendingIntent:
							require.NoError(t, err, output)
							require.Equal(t, "RENEWAL_REQUIRED=typed\n", output)
						case !force && residual == "unknown":
							require.Error(t, err, output)
							require.NotContains(t, output, "RENEWAL_REQUIRED=")
						default:
							require.NoError(t, err, output)
							require.Equal(t, fmt.Sprintf("RENEWAL_REQUIRED=%t\n", force || residual == "1d"), output)
						}
						require.NoFileExists(t, filepath.Join(harness.runtimeDir, "renewed"))
						require.NoFileExists(t, filepath.Join(harness.runtimeDir, "restarts"))
						require.NoFileExists(t, filepath.Join(filepath.Dir(harness.scriptPath), "renewal.log"))
						require.NoFileExists(t, filepath.Join(harness.stateDir, "lock"))
						require.NoDirExists(t, filepath.Join(harness.stateDir, "completed"))
						if runbookState != "" {
							after, err := os.ReadFile(statePath)
							require.NoError(t, err)
							require.Equal(t, state, after)
						} else {
							require.NoDirExists(t, harness.stateDir)
						}
						if runbookName == "restart_pending" {
							require.FileExists(t, filepath.Join(harness.stagingDir, "etcd.yaml"))
							require.NoFileExists(t, filepath.Join(harness.manifestsDir, "etcd.yaml"))
						} else {
							require.NoDirExists(t, harness.stagingDir)
						}
						if agentState != "" {
							after, err := os.ReadFile(harness.agentState)
							require.NoError(t, err)
							require.Equal(t, agentState, string(after))
						} else {
							require.NoFileExists(t, harness.agentState)
						}
					})
				}
			}
		}
	}
}

// Check-only selection fails closed on agent state it cannot interpret, as the
// agent itself refuses an unknown persisted certificate-renewal state, unless
// this runbook has pending intent: then only the legacy executor is selected so
// its restarts still finish. Diagnostics share the agent's combined output with
// the result marker.
func TestCertificateRenewalRunbookCheckOnlyRejectsInvalidAgentState(t *testing.T) {
	for _, test := range []struct {
		name         string
		agentState   string
		runbookPhase string
		wantOutput   string
	}{
		{name: "malformed", agentState: `{"k8sCertRenewalState":`},
		{name: "unknown", agentState: `{"k8sCertRenewalState":"bogus"}`},
		{name: "malformed with pending legacy restart", agentState: `{"k8sCertRenewalState":`, runbookPhase: "restart_pending\netcd", wantOutput: "RENEWAL_REQUIRED=legacy\n"},
		{name: "unknown with pending legacy renewal", agentState: `{"k8sCertRenewalState":"bogus"}`, runbookPhase: "renewing\n", wantOutput: "RENEWAL_REQUIRED=legacy\n"},
		{name: "valid agent intent with pending legacy restart", agentState: `{"k8sCertRenewalState":"restart_pending"}`, runbookPhase: "restart_pending\netcd", wantOutput: "RENEWAL_REQUIRED=legacy_then_typed\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newCertificateRenewalHarness(t)
			harness.setEnv("CHECK_ONLY", "true")
			harness.setEnv("FAKE_RESIDUAL", "365d")
			require.NoError(t, os.WriteFile(harness.agentState, []byte(test.agentState), 0o600))
			if test.runbookPhase != "" {
				state := "pending-cycle\n" + test.runbookPhase + "\nrenewed\n" + harness.caFingerprints(t) + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(harness.stateDir, "state"), []byte(state), 0o600))
			}
			output, err := harness.run(t)
			if test.wantOutput == "" {
				require.Error(t, err, output)
				require.NotContains(t, output, "RENEWAL_REQUIRED=")
				return
			}
			require.NoError(t, err, output)
			var markers []string
			for line := range strings.SplitSeq(output, "\n") {
				if strings.HasPrefix(line, "RENEWAL_REQUIRED=") {
					markers = append(markers, line)
				}
			}
			require.Equal(t, []string{strings.TrimSuffix(test.wantOutput, "\n")}, markers)
			if !strings.Contains(test.name, "valid agent intent") {
				require.Contains(t, output, "WARNING:")
			}
			if strings.HasPrefix(test.name, "malformed") {
				require.Contains(t, output, "parse error")
			}
		})
	}
}
