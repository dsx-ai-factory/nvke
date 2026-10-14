// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

const resultTestSubject = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: Qualification
metadata:
  name: nvke
status:
  type: site-controller
spec:
  subject: nvke
  components:
    - {name: cli, selectors: [latest]}
  dependencies:
    - {name: kube, constraintFrom: {capability: kube}}
`

const resultTestPlan = `{
  "subject": "nvke",
  "version": "v1.0.0",
  "scope": "full",
  "combinations": [
    {
      "id": "cli-v1.0.0",
      "components": [{"name": "cli", "version": "v1.0.0"}],
      "dependencies": [{"name": "cni-ovn", "version": "1.4.0"}],
      "streams": [
        {
          "name": "amd64-1.34",
          "capabilities": {"arch": "amd64", "gpu": "h100", "kube": "1.34"},
          "dependencies": [{"name": "kube", "version": "1.34.2"}]
        }
      ]
    }
  ]
}`

// resultTestRoot writes resultTestSubject to <dir>/release/qualification.yaml
// and resultTestPlan to <dir>/plan.json, and returns dir -- a repo root
// `qualify result` can run against without reading the real one.
func resultTestRoot(t *testing.T) (root, planPath string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release", "qualification.yaml"), []byte(resultTestSubject), 0o644); err != nil {
		t.Fatal(err)
	}
	planPath = filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, []byte(resultTestPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, planPath
}

func writeGinkgoReport(t *testing.T, succeeded bool) string {
	t.Helper()
	state := types.SpecStateFailed
	if succeeded {
		state = types.SpecStatePassed
	}
	start := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	report := types.Report{
		SuiteSucceeded: succeeded,
		StartTime:      start,
		EndTime:        start.Add(5 * time.Minute),
		SpecReports:    types.SpecReports{{State: state}},
	}
	b, err := json.Marshal([]types.Report{report})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runResultCmd(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := resultCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return buf.String(), err
}

func TestResultCmdSucceeds(t *testing.T) {
	root, plan := resultTestRoot(t)
	report := writeGinkgoReport(t, true)

	stdout, err := runResultCmd(t,
		"--root", root,
		"--plan", plan,
		"--report", report,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--output", "-",
	)
	if err != nil {
		t.Fatal(err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if result.Spec.Subject != "nvke" || result.Spec.Type != "site-controller" {
		t.Errorf("spec = %+v, want subject=nvke type=site-controller", result.Spec)
	}
	stream := result.Status.Combinations[0].Streams[0]
	if stream.Result != "PASS" {
		t.Errorf("result = %q, want PASS", stream.Result)
	}
	if stream.Capabilities["gpu"] != "h100" || stream.Capabilities["kube"] != "1.34" {
		t.Errorf("capabilities = %v, want gpu=h100 kube=1.34 stamped from plan.json", stream.Capabilities)
	}
}

func TestResultCmdReportsFailOnFailedSuite(t *testing.T) {
	root, plan := resultTestRoot(t)
	report := writeGinkgoReport(t, false)

	stdout, err := runResultCmd(t,
		"--root", root,
		"--plan", plan,
		"--report", report,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--output", "-",
	)
	if err != nil {
		t.Fatal(err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	stream := result.Status.Combinations[0].Streams[0]
	if stream.Result != "FAIL" {
		t.Errorf("result = %q, want FAIL", stream.Result)
	}
	if strings.Contains(stream.Summary, "infrastructure failure") {
		t.Errorf("summary = %q, a failed suite is not an infrastructure failure", stream.Summary)
	}
}

// A run that never produced a report (Forge provisioning failure, a wedged
// runner) still needs a visible red entry, not a command error that leaves
// nothing published.
func TestResultCmdReportsInfrastructureFailureOnMissingReport(t *testing.T) {
	root, plan := resultTestRoot(t)

	stdout, err := runResultCmd(t,
		"--root", root,
		"--plan", plan,
		"--report", filepath.Join(t.TempDir(), "does-not-exist.json"),
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--output", "-",
	)
	if err != nil {
		t.Fatalf("RunE returned an error, want a written FAIL result: %v", err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	stream := result.Status.Combinations[0].Streams[0]
	if stream.Result != "FAIL" {
		t.Errorf("result = %q, want FAIL", stream.Result)
	}
	if !strings.Contains(stream.Summary, "infrastructure failure") {
		t.Errorf("summary = %q, want it to say infrastructure failure", stream.Summary)
	}
	// Capabilities still come from the plan even when the report is missing --
	// the plan load does not depend on the report.
	if stream.Capabilities["gpu"] != "h100" {
		t.Errorf("capabilities = %v, want gpu stamped from plan.json even on an infra failure", stream.Capabilities)
	}
}

func TestResultCmdRejectsUnknownCombination(t *testing.T) {
	root, plan := resultTestRoot(t)
	report := writeGinkgoReport(t, true)

	_, err := runResultCmd(t,
		"--root", root,
		"--plan", plan,
		"--report", report,
		"--combination", "does-not-exist",
		"--stream", "amd64-1.34",
	)
	if err == nil {
		t.Fatal("got nil, want an error for an unknown combination")
	}
}

func TestResultCmdRejectsUnknownStream(t *testing.T) {
	root, plan := resultTestRoot(t)
	report := writeGinkgoReport(t, true)

	_, err := runResultCmd(t,
		"--root", root,
		"--plan", plan,
		"--report", report,
		"--combination", "cli-v1.0.0",
		"--stream", "does-not-exist",
	)
	if err == nil {
		t.Fatal("got nil, want an error for an unknown stream")
	}
}

func TestResultCmdRejectsMissingPlan(t *testing.T) {
	root, _ := resultTestRoot(t)
	report := writeGinkgoReport(t, true)

	_, err := runResultCmd(t,
		"--root", root,
		"--plan", filepath.Join(t.TempDir(), "does-not-exist.json"),
		"--report", report,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
	)
	if err == nil || !strings.Contains(err.Error(), "--plan") {
		t.Errorf("err = %v, want it to name --plan", err)
	}
}
