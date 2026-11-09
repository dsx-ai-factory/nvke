// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dsRetryScript is the script under test, resolved from this file's
// location so the suite works from any working directory.
var dsRetryScript = func() string {
	_, src, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(src), "..", "cloud-init-ds-retry.sh")
}()

// fixture describes the node a test boots the script on.
type fixture struct {
	cmdline string // /proc/cmdline content; a ds=nocloud[-net];s=<url> token is what makes the script care at all
	status  string // `cloud-init status --format=yaml` before any recovery
	// afterRecovery replaces status once the fake cloud-init logs a `clean`
	// call, so a recovery's effect on the datasource can be reported.
	afterRecovery string
	probeOK       bool   // whether the seed URL answers
	failCloudInit string // first argument on which the fake cloud-init exits 1 ("init", "modules", ...)
	stageBusy     bool   // fake systemctl reports a cloud-*.service stage in the activating state
	failStageQ    bool   // fake systemctl fails the list-units query outright, with no output
	doneMark      bool   // /var/lib/nvke/cloud-init-ds-retry.done already exists
	attempts      string // prior content of the recovery-attempts file; "" writes none
	probes        string // prior content of the probe-count file; "" writes none
}

// runDsRetry runs cloud-init-ds-retry.sh against f. Fake cloud-init, python3
// and systemctl binaries sit in a private bin directory and append every
// invocation to the returned call log, so tests can assert on order without
// ever touching the real host.
func runDsRetry(t *testing.T, f fixture) (calls []string, stateDir string, err error) {
	t.Helper()
	root := t.TempDir()

	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	stateDir = filepath.Join(root, "nvke")
	require.NoError(t, os.MkdirAll(stateDir, 0o755))

	cmdlineFile := filepath.Join(root, "cmdline")
	require.NoError(t, os.WriteFile(cmdlineFile, []byte(f.cmdline), 0o644))

	statusFile := filepath.Join(root, "status.yaml")
	require.NoError(t, os.WriteFile(statusFile, []byte(f.status), 0o644))
	afterFile := ""
	if f.afterRecovery != "" {
		afterFile = filepath.Join(root, "status-after.yaml")
		require.NoError(t, os.WriteFile(afterFile, []byte(f.afterRecovery), 0o644))
	}

	logFile := filepath.Join(root, "calls.log")
	writeFake(t, bin, "cloud-init", `#!/usr/bin/env bash
printf 'cloud-init %s\n' "$*" >> "${FAKE_LOG}"
if [[ "${1:-}" == "status" ]]; then
  if [[ -n "${STATUS_AFTER:-}" ]] && grep -q 'cloud-init clean' "${FAKE_LOG}" 2>/dev/null; then
    cat "${STATUS_AFTER}"
  else
    cat "${STATUS_BEFORE}"
  fi
  exit 0
fi
if [[ "${1:-}" == "${FAIL_ON:-}" ]]; then
  exit 1
fi
exit 0
`)
	writeFake(t, bin, "python3", `#!/usr/bin/env bash
printf 'python3 %s\n' "$*" >> "${FAKE_LOG}"
exit "${PROBE_RC:-1}"
`)
	// The fake models a real node's systemd, not flags: on a settled node
	// the oneshot RemainAfterExit=yes cloud-init stage units stay
	// "active (exited)" forever, so is-active always reports them active
	// -- any code path that consults is-active for idleness deadlocks
	// against this fixture, which is exactly the production bug the
	// list-units state filter exists to avoid.
	writeFake(t, bin, "systemctl", `#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >> "${FAKE_LOG}"
if [[ "${1:-}" == "is-active" ]]; then
  # active (exited) units are active: is-active can never witness idle.
  exit 0
fi
if [[ "${1:-}" == "list-units" ]]; then
  # A state filter of activating/deactivating matches nothing on a
  # settled node; a stage genuinely in flight prints its line. A dead
  # systemd fails the query with no output at all.
  if [[ "${CLOUD_STAGE_QUERY_FAIL:-0}" == "1" ]]; then
    exit 1
  fi
  if [[ "${CLOUD_STAGE_BUSY:-0}" == "1" ]]; then
    printf 'cloud-init-main.service loaded activating start   Start Message\n'
  fi
  exit 0
fi
exit 0
`)

	if f.doneMark {
		require.NoError(t, os.WriteFile(filepath.Join(stateDir, "cloud-init-ds-retry.done"), nil, 0o644))
	}
	if f.attempts != "" {
		require.NoError(t, os.WriteFile(filepath.Join(stateDir, "cloud-init-ds-retry.attempts"), []byte(f.attempts), 0o644))
	}
	if f.probes != "" {
		require.NoError(t, os.WriteFile(filepath.Join(stateDir, "cloud-init-ds-retry.probes"), []byte(f.probes), 0o644))
	}

	probeRC := "1"
	if f.probeOK {
		probeRC = "0"
	}
	stageBusy := "0"
	if f.stageBusy {
		stageBusy = "1"
	}
	failStageQ := "0"
	if f.failStageQ {
		failStageQ = "1"
	}
	cmd := exec.CommandContext(t.Context(), "bash", dsRetryScript)
	cmd.Env = append(os.Environ(),
		"NVKE_DSRETRY_TEST_CMDLINE="+cmdlineFile,
		"NVKE_DSRETRY_TEST_STATE_DIR="+stateDir,
		"NVKE_DSRETRY_TEST_CLOUD_INIT="+filepath.Join(bin, "cloud-init"),
		"NVKE_DSRETRY_TEST_PYTHON="+filepath.Join(bin, "python3"),
		"NVKE_DSRETRY_TEST_SYSTEMCTL="+filepath.Join(bin, "systemctl"),
		"FAKE_LOG="+logFile,
		"STATUS_BEFORE="+statusFile,
		"STATUS_AFTER="+afterFile,
		"FAIL_ON="+f.failCloudInit,
		"PROBE_RC="+probeRC,
		"CLOUD_STAGE_BUSY="+stageBusy,
		"CLOUD_STAGE_QUERY_FAIL="+failStageQ,
	)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	runErr := cmd.Run()

	content, readErr := os.ReadFile(logFile)
	if os.IsNotExist(readErr) {
		return nil, stateDir, runErr
	}
	require.NoError(t, readErr)
	return strings.Split(strings.TrimRight(string(content), "\n"), "\n"), stateDir, runErr
}

// writeFake installs an executable fake binary named name into dir.
func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
}

// seededCmdline is the kernel command line a production node boots with.
const seededCmdline = "BOOT_IMAGE=/vmlinuz root=LABEL=cloudimg-rootfs ds=nocloud-net;s=http://carbide-pxe.forge/api/v0/cloud-init/ console=ttyS0,115200"

// statusYAML renders the fields of `cloud-init status --format=yaml` the
// script reads. extended_status is a separate parameter because it is the
// field that carries "degraded done" and "error - done" -- plain status only
// says done/running/error.
func statusYAML(status, extended, datasource string) string {
	return fmt.Sprintf("status: %s\ndatasource: %s\nextended_status: %s\n", status, datasource, extended)
}

// TestDsRetryNoSeedIsDone asserts that a kernel command line with no
// ds=nocloud seed writes the sentinel and disables the timer without ever
// probing or cleaning.
func TestDsRetryNoSeedIsDone(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{cmdline: "BOOT_IMAGE=/vmlinuz root=LABEL=root ro"})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	assert.Contains(t, calls, "systemctl disable --now nvke-cloud-init-ds-retry.timer")
	for _, call := range calls {
		assert.NotContains(t, call, "cloud-init clean")
		assert.NotContains(t, call, "python3")
	}
}

// TestDsRetryEmptySeedIsDone asserts that an empty `s=` value is treated
// like no seed at all and disables the timer.
func TestDsRetryEmptySeedIsDone(t *testing.T) {
	// s= with no URL asks for nothing, same as no s= at all.
	calls, _, err := runDsRetry(t, fixture{cmdline: "ds=nocloud-net;s= ro"})
	require.NoError(t, err)
	assert.Contains(t, calls, "systemctl disable --now nvke-cloud-init-ds-retry.timer")
}

// TestDsRetryLocalSeedIsDone asserts that non-http(s) seeds (/cidata,
// file://, seed://) are marked done immediately instead of being probed
// forever.
func TestDsRetryLocalSeedIsDone(t *testing.T) {
	// /cidata and file:// seeds are local: there is no URL to wait for, so
	// probing forever would just keep the timer alive on a healthy node.
	for _, cmdline := range []string{
		"ds=nocloud;s=/cidata ro",
		"ds=nocloud-net;s=file:///cidata/ ro",
		"ds=nocloud;s=seed://local ro",
	} {
		t.Run(cmdline, func(t *testing.T) {
			calls, stateDir, err := runDsRetry(t, fixture{cmdline: cmdline})
			require.NoError(t, err)
			assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
			assert.Contains(t, calls, "systemctl disable --now nvke-cloud-init-ds-retry.timer")
			for _, call := range calls {
				assert.NotContains(t, call, "python3")
			}
		})
	}
}

// TestDsRetryHealthyDatasourceIsDone asserts that a settled cloud-init
// with a real `nocloud` datasource writes the sentinel without probing.
func TestDsRetryHealthyDatasourceIsDone(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("done", "done", "nocloud"),
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	assert.Contains(t, calls, "systemctl disable --now nvke-cloud-init-ds-retry.timer")
	assert.NotContains(t, calls, "python3 - http://carbide-pxe.forge/api/v0/cloud-init/meta-data")
}

// TestDsRetryRealDatasourceNameIsDone asserts that the long-form
// `DataSourceNoCloudNet [seed=...]` datasource spelling reads as healthy,
// not as "none".
func TestDsRetryRealDatasourceNameIsDone(t *testing.T) {
	// cloud-init also reports the datasource as the class name with seed
	// attributes; that spelling must read as healthy, not as "none".
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("done", "done", "DataSourceNoCloudNet [seed=http://carbide-pxe.forge/api/v0/cloud-init/][dsmode=net]"),
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	for _, call := range calls {
		assert.NotContains(t, call, "python3")
	}
}

// TestDsRetryWaitsForCloudInit asserts that unsettled statuses (running,
// not started) defer the tick: no probe, no clean, no sentinel.
func TestDsRetryWaitsForCloudInit(t *testing.T) {
	for _, status := range []string{"running", "not started"} {
		t.Run(status, func(t *testing.T) {
			calls, stateDir, err := runDsRetry(t, fixture{
				cmdline: seededCmdline,
				status:  statusYAML(status, status, ""),
			})
			require.NoError(t, err)
			assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
			for _, call := range calls {
				assert.NotContains(t, call, "python3")
				assert.NotContains(t, call, "cloud-init clean")
			}
		})
	}
}

// TestDsRetryDisabledCloudInitIsDone asserts that a disabled cloud-init
// retires the mechanism by disabling the timer.
func TestDsRetryDisabledCloudInitIsDone(t *testing.T) {
	calls, _, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("disabled", "disabled", ""),
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "systemctl disable --now nvke-cloud-init-ds-retry.timer")
}

// TestDsRetryUnsetDatasourceKeepsChecking asserts that a settled run with
// no datasource recorded ("", null, ”, ~) neither probes nor finishes.
func TestDsRetryUnsetDatasourceKeepsChecking(t *testing.T) {
	// "done" with no datasource recorded is ambiguous; only a real name or
	// an explicit none marker is allowed to end the mechanism.
	for _, datasource := range []string{"", "null", "''", "~"} {
		t.Run(datasource, func(t *testing.T) {
			calls, stateDir, err := runDsRetry(t, fixture{
				cmdline: seededCmdline,
				status:  statusYAML("done", "done", datasource),
			})
			require.NoError(t, err)
			assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
			for _, call := range calls {
				assert.NotContains(t, call, "python3")
			}
		})
	}
}

// TestDsRetryStrandedSeedDown asserts the stranded case: datasource none
// plus an unreachable seed probes meta-data but neither cleans nor
// finishes.
func TestDsRetryStrandedSeedDown(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("done", "degraded done", "none"),
		probeOK: false,
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "python3 - http://carbide-pxe.forge/api/v0/cloud-init/meta-data")
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	for _, call := range calls {
		assert.NotContains(t, call, "cloud-init clean")
		assert.NotContains(t, call, "systemctl disable")
	}
}

// TestDsRetryProbeBudgetExhaustedIsDone asserts that a spent probe budget
// writes the sentinel and disables the timer without another recovery.
func TestDsRetryProbeBudgetExhaustedIsDone(t *testing.T) {
	// A permanently dark seed must not keep the timer alive: the probe
	// count is bounded and exhaustion records itself done.
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("done", "degraded done", "none"),
		probes:  "60\n",
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	assert.Contains(t, calls, "systemctl disable --now nvke-cloud-init-ds-retry.timer")
	// Deciding clears the spent budgets so the documented re-arm (delete
	// the sentinel, re-enable the timer) starts a fresh cycle rather than
	// instantly giving up again.
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.probes"))
	for _, call := range calls {
		assert.NotContains(t, call, "cloud-init clean")
	}
}

// TestDsRetryProbeCountsTicks asserts that each probed tick increments the
// probes state file.
func TestDsRetryProbeCountsTicks(t *testing.T) {
	_, stateDir, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("done", "degraded done", "none"),
		probeOK: false,
	})
	require.NoError(t, err)
	content, readErr := os.ReadFile(filepath.Join(stateDir, "cloud-init-ds-retry.probes"))
	require.NoError(t, readErr)
	assert.Equal(t, "1\n", string(content))
}

// TestDsRetryStrandedSeedUpRecovers asserts the full happy-path call
// order: status, meta-data probe, in-flight guard, clean, init, both
// module stages, final status, timer disable, sentinel.
func TestDsRetryStrandedSeedUpRecovers(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline:       seededCmdline,
		status:        statusYAML("done", "degraded done", "none"),
		afterRecovery: statusYAML("done", "done", "nocloud"),
		probeOK:       true,
	})
	require.NoError(t, err)
	// The recovery is the documented operator sequence: the in-flight
	// guard, clean, then init and both module stages, in that order. The
	// fake's is-active reports the settled units active throughout -- an
	// is-active-based idle check would deadlock here.
	want := []string{
		"cloud-init status --format=yaml",
		"python3 - http://carbide-pxe.forge/api/v0/cloud-init/meta-data",
		"systemctl list-units --type=service --state=activating,deactivating cloud-*.service --quiet",
		"cloud-init clean",
		"cloud-init init",
		"cloud-init modules --mode=config",
		"cloud-init modules --mode=final",
		"cloud-init status --format=yaml",
		"systemctl disable --now nvke-cloud-init-ds-retry.timer",
	}
	require.Equal(t, len(want), len(calls), calls)
	for i, call := range want {
		assert.Equal(t, call, calls[i])
	}
	assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	// The deciding run clears its spent budgets too, so a re-arm starts at
	// zero rather than inheriting this cycle's counters.
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.attempts"))
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.probes"))
}

// TestDsRetryCloudInitStageInFlightWaits asserts that a cloud-init stage
// in the activating state defers clean: the guard call is made but no
// recovery attempt or sentinel happens.
func TestDsRetryCloudInitStageInFlightWaits(t *testing.T) {
	// extended_status said done but a stage went back to work while the
	// probe ran: clean must not delete /var/lib/cloud under a live stage.
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline:   seededCmdline,
		status:    statusYAML("done", "degraded done", "none"),
		probeOK:   true,
		stageBusy: true,
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "systemctl list-units --type=service --state=activating,deactivating cloud-*.service --quiet")
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.attempts"))
	for _, call := range calls {
		assert.NotContains(t, call, "cloud-init clean")
	}
}

// TestDsRetryStageQueryFailureWaits asserts that a failed busy-state query
// counts as busy, not idle: a dead systemd must never let cloud-init clean
// delete /var/lib/cloud under a possibly live stage.
func TestDsRetryStageQueryFailureWaits(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline:    seededCmdline,
		status:     statusYAML("done", "degraded done", "none"),
		probeOK:    true,
		failStageQ: true,
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "systemctl list-units --type=service --state=activating,deactivating cloud-*.service --quiet")
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.attempts"))
	for _, call := range calls {
		assert.NotContains(t, call, "cloud-init clean")
	}
}

// TestDsRetryJoinsSeedWithoutTrailingSlash asserts the probe URL joins a
// seed without trailing slash as <seed>/meta-data.
func TestDsRetryJoinsSeedWithoutTrailingSlash(t *testing.T) {
	calls, _, err := runDsRetry(t, fixture{
		cmdline: "ds=nocloud-net;s=http://seed.example/nocloud ro",
		status:  statusYAML("done", "degraded done", "none"),
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "python3 - http://seed.example/nocloud/meta-data")
}

// TestDsRetryStripsSeedQueryAndFragment asserts that query and fragment
// components are stripped from the seed before meta-data is joined, while
// the path and port survive.
func TestDsRetryStripsSeedQueryAndFragment(t *testing.T) {
	// cloud-init joins meta-data onto the seed prefix; a query or fragment
	// in s= is not part of it and would otherwise corrupt the probe URL.
	for cmdline, want := range map[string]string{
		"ds=nocloud-net;s=http://seed.example/nocloud/?token=abc ro":    "python3 - http://seed.example/nocloud/meta-data",
		"ds=nocloud-net;s=http://seed.example/nocloud/#frag ro":         "python3 - http://seed.example/nocloud/meta-data",
		"ds=nocloud-net;s=http://seed.example/nocloud?tok=1#frag ro":    "python3 - http://seed.example/nocloud/meta-data",
		"ds=nocloud-net;s=http://seed.example:8443/nocloud/?a=b&c=d ro": "python3 - http://seed.example:8443/nocloud/meta-data",
	} {
		t.Run(cmdline, func(t *testing.T) {
			calls, _, err := runDsRetry(t, fixture{
				cmdline: cmdline,
				status:  statusYAML("done", "degraded done", "none"),
			})
			require.NoError(t, err)
			assert.Contains(t, calls, want)
		})
	}
}

// TestDsRetryDataSourceNoneSpellingProbes asserts that the long-form
// "DataSourceNone" datasource value takes the probe path like "none".
func TestDsRetryDataSourceNoneSpellingProbes(t *testing.T) {
	// "DataSourceNone" with optional attributes is the long form of the
	// absent datasource; it must trigger the probe path like "none" does.
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("done", "degraded done", "DataSourceNone"),
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "python3 - http://carbide-pxe.forge/api/v0/cloud-init/meta-data")
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
}

// TestDsRetryNocloudWithoutNetSuffix asserts that ds=nocloud (no -net
// suffix) with an http seed still recovers through clean and reaches done.
func TestDsRetryNocloudWithoutNetSuffix(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline:       "ds=nocloud;s=http://seed.example/nocloud/ ro",
		status:        statusYAML("done", "degraded done", "none"),
		afterRecovery: statusYAML("done", "done", "nocloud"),
		probeOK:       true,
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "cloud-init clean")
	assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
}

// TestDsRetryFailedInitCountsAttempt asserts that a failed `cloud-init
// init` aborts before the module stages, records attempt 1, and leaves no
// sentinel.
func TestDsRetryFailedInitCountsAttempt(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline:       seededCmdline,
		status:        statusYAML("done", "degraded done", "none"),
		probeOK:       true,
		failCloudInit: "init",
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "cloud-init clean")
	assert.NotContains(t, calls, "cloud-init modules --mode=config")
	content, readErr := os.ReadFile(filepath.Join(stateDir, "cloud-init-ds-retry.attempts"))
	require.NoError(t, readErr)
	assert.Equal(t, "1\n", string(content))
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
}

// TestDsRetryStillNoneAfterRecoveryCountsAttempt asserts that a completed
// recovery still reporting datasource none keeps the unit armed: module
// stages ran, no sentinel, no timer disable.
func TestDsRetryStillNoneAfterRecoveryCountsAttempt(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("done", "degraded done", "none"),
		probeOK: true,
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "cloud-init modules --mode=final")
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	for _, call := range calls {
		assert.NotContains(t, call, "systemctl disable")
	}
}

// TestDsRetryRecoveryNotSettledKeepsRetrying asserts that a post-recovery
// status of running withholds the sentinel even when a datasource name is
// already visible.
func TestDsRetryRecoveryNotSettledKeepsRetrying(t *testing.T) {
	// A recovery that finds a datasource but leaves cloud-init still
	// running must not write the sentinel: the next tick re-evaluates.
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline:       seededCmdline,
		status:        statusYAML("done", "degraded done", "none"),
		afterRecovery: statusYAML("running", "running", "nocloud"),
		probeOK:       true,
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "cloud-init modules --mode=final")
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
}

// TestDsRetryGivesUpAfterMaxRecoveries asserts that a spent recovery
// budget writes the sentinel and disables the timer without running clean
// again.
func TestDsRetryGivesUpAfterMaxRecoveries(t *testing.T) {
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline:  seededCmdline,
		status:   statusYAML("done", "degraded done", "none"),
		probeOK:  true,
		attempts: "6\n",
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	assert.Contains(t, calls, "systemctl disable --now nvke-cloud-init-ds-retry.timer")
	// The spent recovery budget is cleared on the deciding run, so a
	// re-armed node recovers again instead of exhausting instantly.
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.attempts"))
	for _, call := range calls {
		assert.NotContains(t, call, "cloud-init clean")
	}
}

// TestDsRetryDoneMarkShortCircuits asserts that an existing sentinel exits
// before any external command runs.
func TestDsRetryDoneMarkShortCircuits(t *testing.T) {
	calls, _, err := runDsRetry(t, fixture{
		cmdline:  seededCmdline,
		status:   statusYAML("done", "degraded done", "none"),
		probeOK:  true,
		doneMark: true,
	})
	require.NoError(t, err)
	assert.Empty(t, calls)
}

// TestDsRetryErrorDoneStillRecovers asserts that extended_status
// "error - done" still counts as settled and reaches the recovery path.
func TestDsRetryErrorDoneStillRecovers(t *testing.T) {
	// extended_status "error - done" means cloud-init finished with an
	// unrecoverable error; the datasource fallback is still worth redoing.
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline:       seededCmdline,
		status:        statusYAML("error", "error - done", "none"),
		afterRecovery: statusYAML("done", "done", "nocloud"),
		probeOK:       true,
	})
	require.NoError(t, err)
	assert.Contains(t, calls, "cloud-init clean")
	assert.FileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
}

// TestDsRetryErrorRunningWaits asserts that extended_status
// "error - running" counts as unsettled: no probe, no clean, no sentinel.
func TestDsRetryErrorRunningWaits(t *testing.T) {
	// "error - running" means a stage already failed but cloud-init is still
	// working; the tick must not clean state out from under it.
	calls, stateDir, err := runDsRetry(t, fixture{
		cmdline: seededCmdline,
		status:  statusYAML("error", "error - running", ""),
		probeOK: true,
	})
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(stateDir, "cloud-init-ds-retry.done"))
	for _, call := range calls {
		assert.NotContains(t, call, "cloud-init clean")
		assert.NotContains(t, call, "python3")
	}
}
