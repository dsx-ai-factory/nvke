// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
	"time"

	ginkgotypes "github.com/onsi/ginkgo/v2/types"
)

// This exercises the existing shell consumer with real local files. It does
// not stand up a controller or claim external runtime acceptance.
func TestUnit_CandidateCLISmokeNeverBuildsSuppliedBinary(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("Linux SHA256 utility is required by the runtime entrypoint")
	}
	library, err := filepath.Abs("../../hack/ci/nvkectl-smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"supplied", "wrong digest", "missing digest", "symlink", "not executable", "missing candidate CLI"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			binary := filepath.Join(root, "nvkectl")
			raw := []byte("#!/bin/sh\nprintf supplied\\n\n")
			if err := os.WriteFile(binary, raw, 0o700); err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
			switch scenario {
			case "wrong digest":
				digest = "sha256:" + strings.Repeat("a", 64)
			case "missing digest":
				digest = ""
			case "symlink":
				link := filepath.Join(root, "link")
				if err := os.Symlink(binary, link); err != nil {
					t.Fatal(err)
				}
				binary = link
			case "not executable":
				if err := os.Chmod(binary, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing candidate CLI":
				binary = ""
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; log(){ printf '%s\n' "$*" >&2; }; nvkectl_smoke_build arm64; printf '%s' "$NVKECTL_BIN"`, "fixture", library)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "NVKECTL_BIN=/must-not-build", "NKE_E2E_CANDIDATE_RELEASE=/verified-release.json", "NVKECTL_SMOKE_PREBUILT=" + binary, "NVKECTL_SMOKE_SHA256=" + digest}
			out, runErr := cmd.CombinedOutput()
			if scenario == "supplied" {
				if runErr != nil || string(out) != binary {
					t.Fatalf("supplied binary selection: %s %v", out, runErr)
				}
			} else if runErr == nil || strings.Contains(string(out), "compiling nvkectl") {
				t.Fatalf("invalid candidate reached source build: %s %v", out, runErr)
			}
		})
	}
}

func candidateLibrary(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../hack/ci/nico-vms-candidate.sh")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// The current product CLI is built only as a contract-test fixture. Runtime
// candidate admission receives an already verified and supplied CLI; it never
// invokes this build or turns metadata inspection into authentication.
func TestUnit_CandidateReleaseInputsUseCurrentCLIOwner(t *testing.T) {
	cliDir, err := filepath.Abs("../../../cli")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "nvkectl")
	fixture, err := os.ReadFile("../../../deploy/testdata/nvkr-release/valid.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		wantOK bool
	}{
		{name: "complete current metadata", wantOK: true},
		{name: "lifecycle v4", mutate: func(doc map[string]any) {
			doc["schemaVersion"] = "nvkr.dev/release/v4"
			doc["artifacts"] = doc["outputs"]
			delete(doc, "outputs")
		}},
		{name: "verification wrapper", mutate: func(doc map[string]any) {
			wrapped := make(map[string]any, len(doc))
			for key, value := range doc {
				wrapped[key] = value
				delete(doc, key)
			}
			doc["release"] = wrapped
		}},
		{name: "filtered output selection", mutate: func(doc map[string]any) {
			doc["inspectionDetails"] = map[string]any{"outputSelection": "filtered"}
		}},
		{name: "selected output", mutate: func(doc map[string]any) { doc["selectedOutput"] = "cwe" }},
		{name: "incomplete output count", mutate: func(doc map[string]any) { doc["totalOutputs"] = 99 }},
		{name: "duplicate named output", mutate: func(doc map[string]any) {
			outputs := doc["outputs"].([]any)
			doc["outputs"] = append(outputs, outputs[0])
			doc["totalOutputs"] = len(outputs) + 1
		}},
		{name: "missing required role returns empty owner selection", mutate: func(doc map[string]any) {
			outputs := doc["outputs"].([]any)
			var retained []any
			for _, output := range outputs {
				if output.(map[string]any)["name"] != "temporal" {
					retained = append(retained, output)
				}
			}
			doc["outputs"] = retained
			doc["totalOutputs"] = len(retained)
		}},
		{name: "required role has wrong platform", mutate: func(doc map[string]any) {
			for _, output := range doc["outputs"].([]any) {
				artifact := output.(map[string]any)
				if artifact["name"] == "controller-profiles" {
					artifact["os"], artifact["arch"] = "linux", "amd64"
				}
			}
		}},
	}
	const probeTimeout = 15 * time.Second
	// CI compiles this suite with -race. Reuse those compiled dependencies
	// instead of rebuilding the same graph without instrumentation in a cold
	// cache. The actual test binary's metadata, not an environment guess,
	// determines the fixture's build mode.
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("current test binary omits its Go build metadata")
	}
	args := []string{"build"}
	for _, setting := range info.Settings {
		if setting.Key == "-race" && setting.Value == "true" {
			args = append(args, "-race")
		}
	}
	args = append(args, "-o", binary, "./cmd/nvkectl")
	// The enclosing test owns the cold-cache compilation budget. Reserve
	// every admission case and process cleanup, rather than imposing a short
	// independent cap on public dependency downloads and compilation. Keep
	// Go's normal ten-minute budget if the runner disables its test timeout.
	deadline := time.Now().Add(10 * time.Minute)
	if parentDeadline, timed := t.Deadline(); timed {
		deadline = parentDeadline
	}
	deadline = deadline.Add(-time.Duration(len(cases))*probeTimeout - 5*time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	build := exec.CommandContext(ctx, "go", args...)
	build.Dir = cliDir
	build.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	build.Cancel = func() error {
		err := syscall.Kill(-build.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	build.WaitDelay = time.Second
	out, buildErr := build.CombinedOutput()
	if build.Process != nil {
		if stopErr := syscall.Kill(-build.Process.Pid, syscall.SIGKILL); stopErr != nil && !errors.Is(stopErr, syscall.ESRCH) {
			t.Fatalf("stop CLI fixture compiler group: %v", stopErr)
		}
		joinDeadline := time.Now().Add(2 * time.Second)
		for {
			probeErr := syscall.Kill(-build.Process.Pid, 0)
			if errors.Is(probeErr, syscall.ESRCH) {
				break
			}
			if probeErr != nil || time.Now().After(joinDeadline) {
				t.Fatalf("CLI fixture compiler group did not join: %v", probeErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if buildErr != nil {
		t.Fatalf("build the current CLI contract fixture: %s %v", out, errors.Join(buildErr, ctx.Err()))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if decodeErr := json.Unmarshal(fixture, &doc); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if tc.mutate != nil {
				tc.mutate(doc)
			}
			raw, encodeErr := json.Marshal(doc)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			dir := t.TempDir()
			release := filepath.Join(dir, "release.json")
			if writeErr := os.WriteFile(release, raw, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			// This metadata contract runs on macOS too. The Go deadline bounds
			// the fixture; the Linux runtime's timeout utility is not required.
			probeCtx, probeCancel := context.WithTimeout(t.Context(), probeTimeout)
			defer probeCancel()
			cmd := exec.CommandContext(probeCtx, "bash", "-ec", `source "$1"; timeout(){ shift; "$@"; }; fail(){ printf '%s\n' "$*" >&2; exit 1; }; candidate_release_inputs`, "contract", candidateLibrary(t))
			// Admission invokes the CLI up to ten times. Remove only the race
			// runtime's one-second exit sleep; keep its detection and failure exit.
			raceReports := t.TempDir()
			cmd.Env = []string{"PATH=" + t.TempDir(), "GORACE=atexit_sleep_ms=0 log_path=" + filepath.Join(raceReports, "race"), "NKE_E2E_CANDIDATE_NVKECTL=" + binary, "NKE_E2E_CANDIDATE_RELEASE=" + release}
			out, runErr := cmd.CombinedOutput()
			if probeCtx.Err() != nil {
				t.Fatalf("metadata admission exceeded its deadline: %s %v", out, probeCtx.Err())
			}
			// A race is a fixture failure even when this case expects metadata
			// rejection; the race owner writes reports independently of shell output.
			reports, reportErr := os.ReadDir(raceReports)
			if reportErr != nil || len(reports) != 0 {
				t.Fatalf("CLI fixture race reports: %v, entries=%v", reportErr, reports)
			}
			if (runErr == nil) != tc.wantOK {
				t.Fatalf("metadata/platform admission: %s %v; want success=%v", out, runErr, tc.wantOK)
			}
			if tc.name == "missing required role returns empty owner selection" && !strings.Contains(string(out), "omits required role temporal") {
				t.Fatalf("absent role must fail after empty owner selection: %s", out)
			}
			unchanged, readErr := os.ReadFile(release)
			if readErr != nil || !bytes.Equal(unchanged, raw) {
				t.Fatalf("admission modified the description: %v", readErr)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil || len(entries) != 1 {
				t.Fatalf("admission wrote output before runtime: %v, entries=%v", readErr, entries)
			}
		})
	}
}

func TestUnit_CandidateNativeReportsRejectEmptyAndSkippedRuns(t *testing.T) {
	for _, scenario := range []string{"passed", "empty", "skipped", "failed", "incomplete", "suite failed"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			report := ginkgotypes.Report{SuiteSucceeded: true, PreRunStats: ginkgotypes.PreRunStats{SpecsThatWillRun: 1}, SpecReports: ginkgotypes.SpecReports{
				{LeafNodeType: ginkgotypes.NodeTypeBeforeSuite, State: ginkgotypes.SpecStatePassed},
				{LeafNodeType: ginkgotypes.NodeTypeIt, State: ginkgotypes.SpecStatePassed},
			}}
			switch scenario {
			case "empty":
				report.PreRunStats.SpecsThatWillRun = 0
			case "skipped":
				report.SpecReports[1].State = ginkgotypes.SpecStateSkipped
			case "failed":
				report.SpecReports[1].State = ginkgotypes.SpecStateFailed
			case "incomplete":
				report.PreRunStats.SpecsThatWillRun = 2
			case "suite failed":
				report.SuiteSucceeded = false
			}
			raw, err := json.Marshal([]ginkgotypes.Report{report})
			if err != nil {
				t.Fatal(err)
			}
			for name, bytes := range map[string][]byte{"ginkgo.json": raw, "junit.xml": []byte("native fixture")} {
				if writeErr := os.WriteFile(filepath.Join(dir, name), bytes, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; fail(){ exit 1; }; candidate_verify_reports`, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "STATE_DIR=" + dir}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "passed") {
				t.Fatalf("native %s report result: %s %v", scenario, out, err)
			}
		})
	}
}

// Commands below are contract doubles only. They prove cleanup selection and
// terminal status propagation; real kind, libvirt and guest acceptance belongs
// to the nightly runner.
const candidateCleanupContract = `
source "$1"
timeout(){ while [[ "$1" == -* ]]; do shift; done; shift; "$@"; }
candidate_stop_group(){ printf 'stop %s\n' "$1" >> "$STATE_DIR/actions"; }
redact_diagnostics(){ [[ "$CASE" != redaction ]]; }
virsh(){
  if [[ "$*" == *'list --all --name'* ]]; then
    [[ "$CASE" != foreign ]] || printf 'foreign-domain\n'
    return 0
  fi
  printf 'inspect %s\n' "$*" >> "$STATE_DIR/actions"
  return 1
}
kind(){
  [[ "$1" != delete || "$CASE" != delete ]] || return 1
  return 0
}
kubectl(){ return 0; }
sudo(){ shift; "$@"; }
setcap(){ return 0; }
candidate_api(){
  [[ "$CASE" != unavailable ]] || return 1
  if [[ "$*" == *DELETE* ]]; then
    printf 'delete %s\n' "$*" >> "$STATE_DIR/actions"
    touch "$STATE_DIR/deleted"
    printf '{}'
  elif [[ "$CASE" == live && ! -f "$STATE_DIR/deleted" ]]; then
    printf '[{"id":"owned-live"}]'
  else
    printf '[]'
  fi
}
candidate_state_owned=true
candidate_test_exit=0
candidate_sim_pid=123
candidate_monitor_pid='' candidate_suite_pid='' candidate_spire_pid='' candidate_oidc_pid='' candidate_dhcp_pid='' candidate_controller_pid=456
candidate_endpoint=http://owned-fixture
NICO_VMS_ORG_ID=fixture NICO_VMS_TOKEN=fixture
LIBVIRT_URI=qemu:///session KIND_CLUSTER_OWNED=true KIND_CLUSTER_NAME=owned-kind
E2E_BIN="$STATE_DIR/suite" E2E_CONFIG="$STATE_DIR/config" E2E_KUBECONFIG="$STATE_DIR/kubeconfig"
DIAGNOSTICS_MARKER="$STATE_DIR/SAFE_TO_UPLOAD"
NKE_E2E_CANDIDATE_OS_PROFILE=nvke-local-26-04-arm64-plain NKE_E2E_CANDIDATE_KUBERNETES_VERSION=1.34.2
mkdir "$STATE_DIR/sim"
printf 'owned-historical\n' > "$STATE_DIR/owned-instance-ids"
trap candidate_cleanup EXIT
if [[ "$CASE" == cancel ]]; then candidate_test_exit=null; exit 143; fi
exit 0
`

func TestUnit_CandidateCleanupPreservesFailureAndOwnsExactInstances(t *testing.T) {
	for _, scenario := range []string{"clean", "live", "delete", "foreign", "unavailable", "redaction", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", candidateCleanupContract, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "STATE_DIR=" + dir, "CASE=" + scenario}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "clean" || scenario == "live") {
				t.Fatalf("cleanup %s result: %s %v", scenario, out, err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "candidate-outcome.json"))
			if err != nil {
				t.Fatalf("missing cleanup outcome: %s %v", out, err)
			}
			var outcome struct {
				TestExit    *int `json:"test_exit"`
				CleanupExit int  `json:"cleanup_exit"`
				EntryExit   int  `json:"entry_exit"`
			}
			if decodeErr := json.Unmarshal(raw, &outcome); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			failedCleanup := scenario != "clean" && scenario != "live" && scenario != "cancel"
			if (outcome.CleanupExit != 0) != failedCleanup {
				t.Fatalf("incorrect cleanup outcome: %s", raw)
			}
			if scenario == "cancel" && (outcome.EntryExit != 143 || outcome.TestExit != nil) {
				t.Fatalf("cancellation was converted into success: %s", raw)
			}
			actions, err := os.ReadFile(filepath.Join(dir, "actions"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(actions), "delete -X DELETE http://owned-fixture/v2/org/fixture/forge/instance/owned-historical") || strings.Contains(string(actions), "destroy") {
				t.Fatalf("cleanup attempted unowned or already deleted resources: %s", actions)
			}
			if scenario == "live" && !strings.Contains(string(actions), "/instance/owned-live") {
				t.Fatalf("cleanup omitted its currently live instance: %s", actions)
			}
			if !strings.Contains(string(actions), "stop 456\n") {
				t.Fatalf("cleanup omitted the owned controller forward: %s", actions)
			}
		})
	}
}

func TestUnit_CandidateImagePinsAndContainerdReadback(t *testing.T) {
	for _, scenario := range []string{"valid", "archive changed", "wrong reference", "duplicate", "missing values", "wrong readback"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "image.tar")
			payload := []byte("transport fixture; containerd media validation belongs to the native importer")
			if writeErr := os.WriteFile(archive, payload, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			manifest := "sha256:" + strings.Repeat("a", 64)
			reference := "registry.example/cwe@" + manifest
			image := map[string]string{"name": "cwe", "reference": reference, "manifestDigest": manifest, "archive": archive, "archiveDigest": fmt.Sprintf("sha256:%x", sha256.Sum256(payload))}
			if scenario == "archive changed" {
				image["archiveDigest"] = "sha256:" + strings.Repeat("b", 64)
			}
			if scenario == "wrong reference" {
				image["reference"] = "registry.example/cwe:latest"
			}
			images := []map[string]string{image}
			if scenario == "duplicate" {
				images = append(images, image)
			}
			raw, err := json.Marshal(images)
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(dir, "images.json")
			if writeErr := os.WriteFile(input, raw, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			for _, role := range []string{"spire", "postgresql", "temporal"} {
				if scenario == "missing values" && role == "spire" {
					continue
				}
				if writeErr := os.WriteFile(filepath.Join(dir, role+".json"), []byte(`{"image":{"digest":"selected"}}`), 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			readback := reference
			if scenario == "wrong readback" {
				readback += "-other"
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `
source "$1"
fail(){ exit 1; }
timeout(){ while [[ "$1" == -* ]]; do shift; done; shift; "$@"; }
docker(){
  if [[ "$*" == *'images import'* ]]; then
    cat >/dev/null
    printf '%s\n' "$*" >> "$STATE_DIR/actions"
  else
    printf '%s' "$READBACK"
  fi
}

candidate_image_inputs
candidate_import_images
`, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "STATE_DIR=" + dir, "NKE_E2E_CANDIDATE_IMAGE_INPUTS=" + input, "NKE_E2E_CANDIDATE_VALUES=" + dir, "KIND_CLUSTER_NAME=owned", "READBACK=" + readback}
			out, runErr := cmd.CombinedOutput()
			if (runErr == nil) != (scenario == "valid") {
				t.Fatalf("image %s result: %s %v", scenario, out, runErr)
			}
			if scenario == "valid" {
				actions, readErr := os.ReadFile(filepath.Join(dir, "actions"))
				if readErr != nil || !strings.Contains(string(actions), "--platform linux/arm64 --base-name registry.example/cwe -") {
					t.Fatalf("did not invoke native selected-platform importer: %s %v", actions, readErr)
				}
			}
		})
	}
}

func TestUnit_CandidateServedDiskMatchesPin(t *testing.T) {
	for _, scenario := range []string{"exact", "different bytes", "download failed"} {
		t.Run(scenario, func(t *testing.T) {
			payload := []byte("pinned candidate disk bytes")
			served := filepath.Join(t.TempDir(), "served.qcow2")
			if scenario == "different bytes" {
				payload = []byte("another disk")
			}
			if err := os.WriteFile(served, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			pin := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("pinned candidate disk bytes")))
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1";
timeout(){ while [[ "$1" == -* ]]; do shift; done; shift; "$@"; }
curl(){ cat "$SERVED"; [[ "$CASE" != 'download failed' ]]; }
candidate_verify_disk_service`, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SERVED=" + served, "CASE=" + scenario, "NKE_E2E_CANDIDATE_QCOW2_SHA256=" + pin}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "exact") {
				t.Fatalf("served disk %s: %s %v", scenario, out, err)
			}
		})
	}
}

func TestUnit_CandidateOwnershipObservationRequiresPinnedGuestBacking(t *testing.T) {
	for _, scenario := range []string{"exact", "domain disappeared", "dump transient failure", "wrong backing", "non-KVM"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "sim", "cache", "base.qcow2")
			if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
				t.Fatal(err)
			}
			payload := []byte("pinned read-only base")
			pin := fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
			if scenario == "wrong backing" {
				payload = []byte("unselected guest base")
			}
			if err := os.WriteFile(base, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			disk := filepath.Join(dir, "sim", "instances", "owned", "disk.qcow2")
			chain, err := json.Marshal([]map[string]string{{"filename": disk, "format": "qcow2"}, {"filename": base, "format": "qcow2"}})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"
timeout(){ while [[ "$1" == -* ]]; do shift; done; shift; "$@"; }
candidate_api(){ printf '[{"id":"owned"}]'; }
virsh(){
  case "$*" in
    *dominfo*) return 0 ;;
    *dumpxml*)
      [[ "$CASE" != 'domain disappeared' && "$CASE" != 'dump transient failure' ]] || return 1
      if [[ "$CASE" == non-KVM ]]; then printf "<domain type='qemu'/>"; else printf "<domain type='kvm'/>"; fi ;;
    *'list --all --name'*) [[ "$CASE" == 'domain disappeared' ]] || printf 'nico-vm-owned\n' ;;
  esac
}
qemu-img(){ printf '%s' "$CHAIN"; }
candidate_record_instances`, "contract", candidateLibrary(t))
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"), "STATE_DIR=" + dir, "CASE=" + scenario, "CHAIN=" + string(chain),
				"LIBVIRT_URI=qemu:///session", "candidate_endpoint=http://owned", "NICO_VMS_ORG_ID=org", "NICO_VMS_TOKEN=fixture",
				"NKE_E2E_CANDIDATE_QCOW2=" + base, "NKE_E2E_CANDIDATE_QCOW2_SHA256=" + pin,
			}
			out, runErr := cmd.CombinedOutput()
			if (runErr == nil) != (scenario == "exact" || scenario == "domain disappeared") {
				t.Fatalf("observation %s: %s %v", scenario, out, runErr)
			}
			_, statErr := os.Stat(filepath.Join(dir, "observed-domain-owned.xml"))
			if (statErr == nil) != (scenario == "exact") {
				t.Fatalf("failed/unverified observation became accepted KVM evidence: %s %v", out, statErr)
			}
			ledger, readErr := os.ReadFile(filepath.Join(dir, "owned-instance-ids"))
			if readErr != nil || string(ledger) != "owned\n" {
				t.Fatalf("ownership recovery ledger lost the exact ID: %s %v", ledger, readErr)
			}
		})
	}
}

func TestUnit_CandidateOwnershipMonitorBoundsConsecutiveFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sequence string
		calls    string
		wantOK   bool
	}{
		{"transient errors recover", "fail,fail,pass,done", "4", true},
		{"persistent errors stop", "fail,fail,fail", "3", false},
		{"known wrong image stops immediately", "wrong,pass,done", "1", false},
		{"successful iteration resets budget", "fail,fail,pass,fail,fail,fail", "6", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1";
calls=0
IFS=, read -r -a attempts <<< "$SEQUENCE"
candidate_record_instances(){
  local result="${attempts[$calls]}"
  calls=$((calls + 1))
  printf 'call %s\n' "$calls"
  [[ "$result" != done ]] || exit 0
  [[ "$result" != wrong ]] || return 2
  [[ "$result" == pass ]]
}
sleep(){ :; }
candidate_monitor_instances`, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SEQUENCE=" + tc.sequence}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantOK || !strings.Contains(string(out), "call "+tc.calls+"\n") {
				t.Fatalf("bounded ownership monitor: %s %v", out, err)
			}
		})
	}
}

func TestUnit_CandidateControllerSmokeOwnsForward(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"
fail(){ exit 1; }
ss(){ return 0; }
setsid(){ printf '%s\n' "$*" > "$STATE_DIR/forward"; }
kill(){ return 0; }
curl(){ return 0; }
candidate_start_controller_forward
[[ -n "$candidate_controller_pid" ]]
wait "$candidate_controller_pid"`, "contract", candidateLibrary(t))
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "STATE_DIR=" + dir, "E2E_KUBECONFIG=/owned/kubeconfig", "NVKECTL_SMOKE_CONTROLLER_NAMESPACE=nke-controller",
		"NVKECTL_SMOKE_CONTROLLER_SERVICE=nke-controller", "NVKECTL_SMOKE_CONTROLLER_PORT=8080",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("controller forward did not become owned/ready: %s %v", out, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "forward"))
	if err != nil || string(raw) != "kubectl --kubeconfig /owned/kubeconfig -n nke-controller port-forward --address 127.0.0.1 svc/nke-controller 8080:8080\n" {
		t.Fatalf("wrong controller forwarding target: %s %v", raw, err)
	}
}

func TestUnit_CandidateCompletedLeaderStillStopsOwnedGroup(t *testing.T) {
	// Real kernel process groups: the leader exits successfully while another
	// owned member remains. Leader Wait success is insufficient cleanup proof.
	leader := exec.CommandContext(t.Context(), "sh", "-c", "read -r signal")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := leader.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if startErr := leader.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	t.Cleanup(func() {
		_ = leader.Process.Kill()
		_ = stdin.Close()
		if leader.ProcessState == nil {
			_ = leader.Wait()
		}
	})
	member := exec.CommandContext(t.Context(), "sleep", "120")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if startErr := member.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	joined := make(chan error, 1)
	go func() { joined <- member.Wait() }()
	t.Cleanup(func() {
		_ = member.Process.Kill()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned group member was not joined")
		}
	})
	if _, writeErr := stdin.Write([]byte("complete\n")); writeErr != nil {
		t.Fatal(writeErr)
	}
	if waitErr := leader.Wait(); waitErr != nil {
		t.Fatalf("leader did not succeed: %v", waitErr)
	}
	if probeErr := syscall.Kill(-leader.Process.Pid, 0); probeErr != nil {
		t.Fatalf("owned group unexpectedly disappeared with leader: %v", probeErr)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; candidate_stop_group "$2"`, "contract", candidateLibrary(t), fmt.Sprint(leader.Process.Pid))
	if out, stopErr := cmd.CombinedOutput(); stopErr != nil {
		t.Fatalf("actual owned group was not stopped/joined: %s %v", out, stopErr)
	}
	if probeErr := syscall.Kill(-leader.Process.Pid, 0); probeErr != syscall.ESRCH {
		t.Fatalf("group remained after cleanup: %v", probeErr)
	}
}

func TestUnit_CandidateTimedObservationStopsWithMonitor(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("GNU timeout is required by the Linux candidate monitor")
	}
	dir := t.TempDir()
	childPID := filepath.Join(dir, "child.pid")
	// qemu-img is a blocking subprocess fixture. It supplies no disk data:
	// the real candidate helper must stop it when the owned monitor is stopped.
	tool := filepath.Join(dir, "qemu-img")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s' \"$$\" >\"$CHILD_PID\"\nexec sleep 120\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	monitor := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; candidate_verify_guest_backing owned`, "fixture", candidateLibrary(t))
	monitor.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STATE_DIR="+dir, "CHILD_PID="+childPID)
	monitor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := monitor.Start(); err != nil {
		t.Fatal(err)
	}
	joined := make(chan error, 1)
	go func() { joined <- monitor.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-monitor.Process.Pid, syscall.SIGKILL)
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned timed observation was not joined")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	var child int
	for child == 0 {
		if raw, err := os.ReadFile(childPID); err == nil {
			if _, scanErr := fmt.Sscan(string(raw), &child); scanErr != nil {
				t.Fatal(scanErr)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed observation did not launch its subprocess")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Always reclaim the exact fixture child even if the ownership assertion
	// fails; never leave a proof of failed cleanup running on the host.
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	stop := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; candidate_stop_group "$2"`, "fixture", candidateLibrary(t), fmt.Sprint(monitor.Process.Pid))
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("nested timeout escaped owned monitor cleanup: %s %v", out, err)
	}
	if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("timed observation child remains after monitor cleanup: %v", err)
	}
}

// The actual candidate hook must forward the split configuration and keep
// credentials in the process environment rather than command arguments.
func TestUnit_CandidateStackConfigurationHandoff(t *testing.T) {
	root := t.TempDir()
	cli := filepath.Join(root, "nvkectl")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n[ \"$NVKECTL_DATABASE_PASSWORD\" = synthetic-database ]\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; candidate_render_stack`, "contract", candidateLibrary(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "NVKECTL_BIN=" + cli, "WORKSPACE_ROOT=/source", "STATE_DIR=/private/state", "NKE_E2E_CANDIDATE_SITESPEC=/private/site.json", "NKE_E2E_CANDIDATE_STACK_CONFIG=/private/stack.json", "NKE_E2E_CANDIDATE_RELEASE=/private/release.json", "NKE_E2E_CANDIDATE_LAYOUT=/private/layout", "NVKECTL_DATABASE_PASSWORD=synthetic-database"}
	out, err := cmd.CombinedOutput()
	want := "render\ndeploy-path\n--deploy-path\n/source/deploy/nico\n--sitespec\n/private/site.json\n--stack-config\n/private/stack.json\n--release\n/private/release.json\n--nvkr-run-dir\n/private/layout\n--arch\nlinux/arm64\n--out\n/private/state/rendered\n"
	if err != nil || string(out) != want {
		t.Fatalf("candidate render handoff: %s, %v", out, err)
	}
}
