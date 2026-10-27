// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"flag"
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
