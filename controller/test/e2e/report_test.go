// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"flag"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
)

// selectedJUnitPath is a JUnit report of the specs the run selected. Ginkgo's
// own -ginkgo.junit-report lists every spec the label filter left out as
// skipped, so a run of three specs reports nearly three hundred and a consumer
// reading the counts, as a test runner does, cannot tell a deliberate skip
// from one that was never asked for.
var selectedJUnitPath string

func init() {
	flag.StringVar(&selectedJUnitPath, "e2e.junit-report", "", "write a JUnit report of only the specs the run selected")
}

var _ = ReportAfterSuite("JUnit report of the selected specs", func(report Report) {
	if selectedJUnitPath == "" {
		return
	}
	Expect(reporters.GenerateJUnitReport(selectedSpecs(report), selectedJUnitPath)).To(Succeed())
})

// selectedSpecs drops the specs the filters excluded. A filtered spec is
// skipped with no message; a spec that called Skip carries its reason, and is
// kept, because a skip the run did ask for is a result.
// Ginkgo exits 0 when every selected spec skips, or when the filter selects
// none, which a runner of the validation image would read as a cluster that
// passed.
var _ = ReportAfterSuite("a validation run checked something", func(report Report) {
	if validationCheckedNothing(report, os.LookupEnv) {
		Fail("no check passed, so the run says nothing about the cluster; see the skips")
	}
})

// validationCheckedNothing is whether report is of a validation image run,
// not a dry run, in which no spec passed.
func validationCheckedNothing(report Report, lookupEnv func(string) (string, bool)) bool {
	if filter, err := validationLabelFilter(lookupEnv); err != nil || filter == "" || report.SuiteConfig.DryRun {
		return false
	}
	for _, spec := range report.SpecReports {
		if spec.LeafNodeType == types.NodeTypeIt && spec.State == types.SpecStatePassed {
			return false
		}
	}
	return true
}

func selectedSpecs(report Report) Report {
	kept := make(types.SpecReports, 0, len(report.SpecReports))
	for _, spec := range report.SpecReports {
		if spec.State == types.SpecStateSkipped && spec.Failure.Message == "" {
			continue
		}
		kept = append(kept, spec)
	}
	report.SpecReports = kept
	return report
}

func TestValidationCheckedNothing(t *testing.T) {
	image := func(key string) (string, bool) {
		value, ok := map[string]string{siteValidateEnv: "true", tenantValidateEnv: "false"}[key]
		return value, ok
	}
	outside := func(string) (string, bool) { return "", false }
	skipped := Report{SpecReports: types.SpecReports{
		{LeafNodeType: types.NodeTypeBeforeSuite, State: types.SpecStatePassed},
		{LeafNodeType: types.NodeTypeIt, State: types.SpecStateSkipped, Failure: types.Failure{Message: "not configured"}},
	}}
	passed := Report{SpecReports: append(types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed}}, skipped.SpecReports...)}
	dryRun := skipped
	dryRun.SuiteConfig.DryRun = true

	for name, tc := range map[string]struct {
		report Report
		env    func(string) (string, bool)
		want   bool
	}{
		"every spec skipped":       {skipped, image, true},
		"no spec selected":         {Report{}, image, true},
		"one spec passed":          {passed, image, false},
		"a dry run":                {dryRun, image, false},
		"a run outside the image":  {skipped, outside, false},
		"only a setup node passed": {Report{SpecReports: skipped.SpecReports[:1]}, image, true},
	} {
		if got := validationCheckedNothing(tc.report, tc.env); got != tc.want {
			t.Errorf("%s: validationCheckedNothing = %t, want %t", name, got, tc.want)
		}
	}
}

func TestSelectedSpecs(t *testing.T) {
	report := Report{SpecReports: types.SpecReports{
		{LeafNodeText: "passed", State: types.SpecStatePassed},
		{LeafNodeText: "filtered", State: types.SpecStateSkipped},
		{LeafNodeText: "skipped by the spec", State: types.SpecStateSkipped, Failure: types.Failure{Message: "not configured"}},
		{LeafNodeText: "failed", State: types.SpecStateFailed, Failure: types.Failure{Message: "boom"}},
	}}

	selected := selectedSpecs(report).SpecReports
	got := make([]string, 0, len(selected))
	for _, spec := range selected {
		got = append(got, spec.LeafNodeText)
	}
	want := []string{"passed", "skipped by the spec", "failed"}
	if len(got) != len(want) {
		t.Fatalf("selectedSpecs kept %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selectedSpecs kept %q, want %q", got, want)
		}
	}
}
