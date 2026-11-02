// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/onsi/ginkgo/v2/reporters"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

const (
	amd64Stream = "forge-gb200-amd64-ubuntu-24.04-kube-1.36"
	arm64Stream = "forge-gb200-arm64-ubuntu-24.04-kube-1.36"
)

const summaryTestPlan = `{
  "subject": "nvke",
  "version": "v0.23.0-beta.2",
  "scope": "golden-path",
  "overrides": ["os-image-amd64"],
  "combinations": [
    {
      "id": "cli-v0.2.0-5-g48dc30932_controller-v0.2.0-7-g48dc30932",
      "components": [
        {"name": "controller", "version": "v0.2.0-7-g48dc30932", "url": "oci://registry.example/org/nvke-controller:v0.2.0-7-g48dc30932"},
        {"name": "cli", "version": "v0.2.0-5-g48dc30932"}
      ],
      "dependencies": [
        {"name": "os-image-bootloader-amd64", "url": "https://cdn.example/nke-isos/qcow/efi/x86_64/qcow-imager.efi"}
      ],
      "streams": [
        {
          "name": "forge-gb200-amd64-ubuntu-24.04-kube-1.36",
          "capabilities": {"arch": "amd64", "gpu": "gb200", "kube": "1.36", "ubuntu": "24.04"},
          "dependencies": [
            {"name": "kube", "version": "1.36.3"},
            {"name": "os-image-amd64", "url": "https://cdn.example/custom/nvke-node-non-fde-ubuntu-24.04.5-amd64.qcow2"}
          ]
        },
        {
          "name": "forge-gb200-arm64-ubuntu-24.04-kube-1.36",
          "capabilities": {"arch": "arm64", "gpu": "gb200", "kube": "1.36", "ubuntu": "24.04"},
          "dependencies": [
            {"name": "kube", "version": "1.36.3"},
            {"name": "os-image-arm64", "version": "v0.2.0-2-g48dc30932", "url": "https://cdn.example/nvke-packages/os-image/builds/v0.2.0-2-g48dc30932/arm64/nvke-node-non-fde-ubuntu-24.04.5-arm64.qcow2"}
          ]
        }
      ]
    }
  ]
}`

// summaryTestPassed is a Ginkgo JUnit report for a stream whose selected specs
// all passed. A bare "skipped" is a spec the label filter did not select.
const summaryTestPassed = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="5" disabled="0" errors="0" failures="0" time="797.6">
  <testsuite name="NVKE E2E Suite" tests="5" disabled="0" skipped="2" errors="0" failures="0" time="797.6">
    <properties>
      <property name="SuiteSucceeded" value="true"></property>
      <property name="SpecialSuiteFailureReason" value=""></property>
      <property name="LabelFilter" value="site-controller"></property>
    </properties>
    <testcase name="[It] Site controller provisions a node [site-controller]" status="passed" time="795.3">
      <system-err>captured timeline that the summary must not render</system-err>
    </testcase>
    <testcase name="[It] Site controller reaches its API server [site-controller]" status="passed" time="2.3"></testcase>
    <testcase name="[It] Tenant cluster creates a cluster [tenant]" status="skipped" time="0">
      <skipped message="skipped"></skipped>
    </testcase>
    <testcase name="[It] Tenant cluster deletes a cluster [tenant]" status="skipped" time="0">
      <skipped message="skipped"></skipped>
    </testcase>
    <testcase name="[AfterSuite]" status="passed" time="0"></testcase>
  </testsuite>
</testsuites>`

// summaryTestFailed is a Ginkgo JUnit report for a stream that timed out after
// a BeforeAll failed. &#96; is a backtick: the failure quotes a code fence.
const summaryTestFailed = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="8" disabled="0" errors="1" failures="1" time="5402.25">
  <testsuite name="NVKE E2E Suite" tests="8" disabled="0" skipped="5" errors="1" failures="1" time="5402.25">
    <properties>
      <property name="SuiteSucceeded" value="false"></property>
      <property name="SpecialSuiteFailureReason" value="Interrupted by Timeout"></property>
      <property name="LabelFilter" value="site-controller"></property>
    </properties>
    <testcase name="[It] Site controller provisions a node [site-controller]" status="failed" time="0.051">
      <failure message="Expected success, but got an error" type="failed">[FAILED] Expected success, but got an error:&#xA;    &#96;&#96;&#96;&#xA;    a quoted fence the summary must not close on&#xA;    &#96;&#96;&#96;&#xA;In [BeforeAll] at: site_controller_test.go:226&#xA;</failure>
      <system-err>captured timeline that the summary must not render</system-err>
    </testcase>
    <testcase name="[It] Site controller bootstraps the site controller [site-controller]" status="skipped" time="0">
      <skipped message="skipped - Spec skipped because an earlier spec in an ordered container failed"></skipped>
    </testcase>
    <testcase name="[It] Site controller reaches its API server [site-controller]" status="skipped" time="1.5">
      <skipped message="skipped - Spec skipped because an earlier spec in an ordered container failed"></skipped>
    </testcase>
    <testcase name="[It] Site controller installs the deploy path | and a tenant [site-controller]" status="interrupted" time="5400.5">
      <error message="Interrupted by Timeout" type="interrupted"></error>
    </testcase>
    <testcase name="[It] Site controller reports its controller version [site-controller]" status="skipped" time="0">
      <skipped message="skipped - the stack was not deployed"></skipped>
    </testcase>
    <testcase name="[It] Tenant cluster creates a cluster [tenant]" status="skipped" time="0">
      <skipped message="skipped"></skipped>
    </testcase>
    <testcase name="[It] Tenant cluster deletes a cluster [tenant]" status="skipped" time="0">
      <skipped message="skipped"></skipped>
    </testcase>
    <testcase name="[AfterSuite]" status="passed" time="0"></testcase>
  </testsuite>
</testsuites>`

func runSummaryCmd(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := summaryCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return buf.String(), err
}

// assertLines fails unless each of want is a whole line of got, in the order
// given.
func assertLines(t *testing.T, got string, want ...string) {
	t.Helper()
	lines := strings.Split(got, "\n")
	i := 0
	for _, w := range want {
		for i < len(lines) && lines[i] != w {
			i++
		}
		if i == len(lines) {
			t.Fatalf("missing, or out of order: %q\n--- got\n%s", w, got)
		}
		i++
	}
}

func TestSummaryPlan(t *testing.T) {
	got, err := runSummaryCmd(t, "plan", "--plan", writeFile(t, "plan.json", summaryTestPlan))
	if err != nil {
		t.Fatal(err)
	}
	assertLines(t, got,
		"### Qualification plan",
		"| nvke | `v0.23.0-beta.2` |",
		"| scope | `golden-path` |",
		"| overrides | `os-image-amd64` |",
		"| mode | run |",
		"#### Combination `cli-v0.2.0-5-g48dc30932_controller-v0.2.0-7-g48dc30932`",
		"| Component | Version | URL | Overridden |",
		"| controller | `v0.2.0-7-g48dc30932` | `oci://registry.example/org/nvke-controller:v0.2.0-7-g48dc30932` |  |",
		"| cli | `v0.2.0-5-g48dc30932` |  |  |",
		"| Dependency | Version | URL | Overridden |",
		"| os-image-bootloader-amd64 |  | `nke-isos/qcow/efi/x86_64/qcow-imager.efi` |  |",
		"##### Stream `"+amd64Stream+"`",
		"arch `amd64` · GPU `gb200` · Ubuntu `24.04` · Kubernetes `1.36`",
		"| kube | `1.36.3` |  |  |",
		"| os-image-amd64 |  | `custom/nvke-node-non-fde-ubuntu-24.04.5-amd64.qcow2` | yes |",
		"##### Stream `"+arm64Stream+"`",
		"arch `arm64` · GPU `gb200` · Ubuntu `24.04` · Kubernetes `1.36`",
		"| os-image-arm64 | `v0.2.0-2-g48dc30932` | `nvke-packages/os-image/builds/v0.2.0-2-g48dc30932/arm64/nvke-node-non-fde-ubuntu-24.04.5-arm64.qcow2` |  |",
	)
	if strings.Contains(got, "| Stream |") {
		t.Errorf("the plan summary has a table of streams; each stream's section shows its capabilities:\n%s", got)
	}
}

func TestCapabilities(t *testing.T) {
	tests := []struct {
		name string
		caps map[string]string
		want string
	}{
		{name: "every capability, in a fixed order", caps: map[string]string{"kube": "1.36", "ubuntu": "24.04", "gpu": "l40s", "arch": "amd64"}, want: "\narch `amd64` · GPU `l40s` · Ubuntu `24.04` · Kubernetes `1.36`\n"},
		{name: "one the stream does not declare is left out", caps: map[string]string{"arch": "arm64", "kube": "1.36"}, want: "\narch `arm64` · Kubernetes `1.36`\n"},
		{name: "none is nothing", caps: nil, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			capabilities(&b, tc.caps)
			if b.String() != tc.want {
				t.Errorf("got %q, want %q", b.String(), tc.want)
			}
		})
	}
}

func TestSummaryPlanSaysDryRun(t *testing.T) {
	got, err := runSummaryCmd(t, "plan", "--plan", writeFile(t, "plan.json", summaryTestPlan), "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "| mode | dry run, no machine is taken |") {
		t.Errorf("a dry run's plan summary does not say so:\n%s", got)
	}
}

func TestSummaryResults(t *testing.T) {
	plan := writeFile(t, "plan.json", summaryTestPlan)
	passed := writeFile(t, "passed.junit.xml", summaryTestPassed)
	failed := writeFile(t, "failed.junit.xml", summaryTestFailed)
	noReport := "**No report.** Ginkgo wrote no JUnit report: the job failed or was cancelled before the suite started, " +
		"or the suite was killed before it finished. The job log says which."

	tests := []struct {
		name    string
		reports []string
		want    []string
	}{
		{
			name:    "passing streams count what the label filter left out",
			reports: []string{amd64Stream + "=" + passed, arm64Stream + "=" + passed},
			want: []string{
				"### Test results",
				"**2 of 2 streams passed.**",
				"| Stream | Result | Ran | Passed | Failed | Took |",
				"| " + amd64Stream + " | passed | 3 | 3 | 0 | 13m18s |",
				"| " + arm64Stream + " | passed | 3 | 3 | 0 | 13m18s |",
				"#### Stream `" + amd64Stream + "`",
				"**Passed.** 3 ran: 3 passed. 2 more did not run, not selected by the label filter `site-controller`. Took 13m18s.",
				"| Spec | Result | Duration |",
				"| [It] Site controller provisions a node [site-controller] | passed | 13m15s |",
				"| [It] Site controller reaches its API server [site-controller] | passed | 2s |",
				"| [AfterSuite] | passed | 0s |",
				"#### Stream `" + arm64Stream + "`",
			},
		},
		{
			name:    "a failing stream shows each failure and why the rest skipped",
			reports: []string{amd64Stream + "=" + failed, arm64Stream + "=" + passed},
			want: []string{
				"**1 of 2 streams passed.**",
				"| " + amd64Stream + " | **failed: Interrupted by Timeout** | 6 | 1 | 2 | 1h30m2s |",
				"| " + arm64Stream + " | passed | 3 | 3 | 0 | 13m18s |",
				"#### Stream `" + amd64Stream + "`",
				"**Failed: Interrupted by Timeout.** 6 ran: 1 passed, 1 failed, 1 interrupted, 3 skipped. " +
					"2 more did not run, not selected by the label filter `site-controller` or not reached. Took 1h30m2s.",
				"| [It] Site controller provisions a node [site-controller] | **failed** | 51ms |",
				"| 2 specs | skipped: Spec skipped because an earlier spec in an ordered container failed | 2s |",
				"| [It] Site controller installs the deploy path \\| and a tenant [site-controller] | **interrupted** | 1h30m1s |",
				"| [It] Site controller reports its controller version [site-controller] | skipped: the stack was not deployed | 0s |",
				"##### Failures",
				"<summary>[It] Site controller provisions a node [site-controller] (failed)</summary>",
				"````text",
				"    a quoted fence the summary must not close on",
				"````",
				"<summary>[It] Site controller installs the deploy path | and a tenant [site-controller] (interrupted)</summary>",
				"Interrupted by Timeout",
				"#### Stream `" + arm64Stream + "`",
				"**Passed.** 3 ran: 3 passed. 2 more did not run, not selected by the label filter `site-controller`. Took 13m18s.",
			},
		},
		{
			name:    "a stream whose report is missing or not given says the suite never finished",
			reports: []string{amd64Stream + "=" + filepath.Join(t.TempDir(), "does-not-exist.junit.xml")},
			want: []string{
				"**0 of 2 streams passed.**",
				"| " + amd64Stream + " | **no report** |  |  |  |  |",
				"| " + arm64Stream + " | **no report** |  |  |  |  |",
				"#### Stream `" + amd64Stream + "`",
				noReport,
				"#### Stream `" + arm64Stream + "`",
				noReport,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"results", "--plan", plan}
			for _, r := range tc.reports {
				args = append(args, "--report", r)
			}
			got, err := runSummaryCmd(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			assertLines(t, got, tc.want...)
			if strings.Contains(got, "captured timeline") {
				t.Error("the summary rendered a test case's captured output, which stays in the artifact")
			}
			if strings.Contains(got, "Tenant cluster") {
				t.Error("the summary listed a spec the label filter did not select, which it only counts")
			}
		})
	}
}

func TestSummaryResultsRejects(t *testing.T) {
	plan := writeFile(t, "plan.json", summaryTestPlan)
	malformed := writeFile(t, "qualify-e2e.junit.xml", "<testsuites")
	passed := writeFile(t, "passed.junit.xml", summaryTestPassed)

	tests := []struct {
		name    string
		reports []string
	}{
		{name: "a report that does not parse", reports: []string{amd64Stream + "=" + malformed}},
		{name: "a report with no stream", reports: []string{passed}},
		{name: "a report with an empty path", reports: []string{amd64Stream + "="}},
		{name: "a stream the plan does not have", reports: []string{"forge-l40s-amd64=" + passed}},
		{name: "a stream given twice", reports: []string{amd64Stream + "=" + passed, amd64Stream + "=" + passed}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"results", "--plan", plan}
			for _, r := range tc.reports {
				args = append(args, "--report", r)
			}
			if _, err := runSummaryCmd(t, args...); err == nil {
				t.Fatalf("got nil, want an error for %s", tc.name)
			}
		})
	}
}

func TestLocation(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "an HTTPS URL loses its scheme and host", in: "https://cdn.example/a/b.qcow2", want: "a/b.qcow2"},
		{name: "an HTTP URL does too", in: "http://cdn.example/a/b.efi", want: "a/b.efi"},
		{name: "an OCI reference is whole", in: "oci://registry.example/org/nvkectl:v1", want: "oci://registry.example/org/nvkectl:v1"},
		{name: "a host with no path is whole", in: "https://cdn.example", want: "https://cdn.example"},
		{name: "no URL is empty", in: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := location(tc.in); got != tc.want {
				t.Errorf("location(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSpecRows(t *testing.T) {
	ordered := &reporters.JUnitSkipped{Message: "skipped - Spec skipped because a BeforeAll node failed"}
	cases := []reporters.JUnitTestCase{
		{Name: "provisions a node", Status: "failed", Time: 1},
		{Name: "bootstraps the site controller", Status: "skipped", Skipped: ordered},
		{Name: "deploys the stack", Status: "skipped", Skipped: ordered, Time: 2},
		{Name: "reports its version", Status: "skipped", Skipped: &reporters.JUnitSkipped{Message: "skipped - the stack was not deployed"}},
		{Name: "lists clusters", Status: "skipped", Skipped: ordered},
		{Name: "[AfterSuite]", Status: "passed"},
	}
	want := []specRow{
		{spec: "provisions a node", result: "**failed**", seconds: 1},
		{spec: "3 specs", result: "skipped: Spec skipped because a BeforeAll node failed", seconds: 2},
		{spec: "reports its version", result: "skipped: the stack was not deployed"},
		{spec: "[AfterSuite]", result: "passed"},
	}

	got := specRows(cases)
	if len(got) != len(want) {
		t.Fatalf("specRows() = %d rows, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPlanStreams(t *testing.T) {
	plan := &qualification.Plan{Combinations: []qualification.Combination{
		{ID: "a", Streams: []qualification.Stream{{Name: "s1"}, {Name: "s2"}}},
		{ID: "b", Streams: []qualification.Stream{{Name: "s2"}, {Name: "s3"}}},
	}}
	got := planStreams(plan)
	want := []string{"s1", "s2", "s3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("planStreams() = %v, want %v: each stream once, in the plan's order", got, want)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		limit     int
		want      string
		truncated bool
	}{
		{name: "under the limit is unchanged", in: "short", limit: 10, want: "short"},
		{name: "at the limit is unchanged", in: "exactly10!", limit: 10, want: "exactly10!"},
		{name: "over the limit is cut and says so", in: "0123456789abc", limit: 10, want: "0123456789", truncated: true},
		{name: "a cut never splits a rune", in: "abcdefghé", limit: 9, want: "abcdefgh", truncated: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.limit)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("truncate(%q, %d) = %q, want it to start %q", tc.in, tc.limit, got, tc.want)
			}
			if tc.truncated != strings.Contains(got, "truncated") {
				t.Errorf("truncate(%q, %d) = %q, truncated notice present = %v, want %v", tc.in, tc.limit, got, !tc.truncated, tc.truncated)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) = %q, which is not valid UTF-8", tc.in, tc.limit, got)
			}
		})
	}
}

func TestFence(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "no backticks takes the shortest fence", in: "plain", want: "```"},
		{name: "a code span still takes the shortest fence", in: "a `b` c", want: "```"},
		{name: "a quoted fence is outrun", in: "```\nx\n```", want: "````"},
		{name: "a longer run is outrun too", in: "`````", want: "``````"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fence(tc.in); got != tc.want {
				t.Errorf("fence(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
