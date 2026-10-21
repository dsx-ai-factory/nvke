// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package checks holds the validator's check registry and execution engine.
// Checks are data (checks.yaml); the engine binds each check ID to an
// implementation and reports one Result per applicable check.
package checks

import "fmt"

// Status is the outcome of one check.
type Status string

const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"
	StatusSkip  Status = "skip"
	StatusError Status = "error"
)

// Severity says what a failing check does to the run's exit code.
type Severity string

const (
	// SeverityBlocking fails the run (exit 1).
	SeverityBlocking Severity = "blocking"
	// SeverityWarn is reported but does not fail the run.
	SeverityWarn Severity = "warn"
)

// Method is the inspection tier a check runs in. v0 implements static only;
// guestfs and boot land with their inspection packages.
type Method string

const (
	MethodStatic  Method = "static"
	MethodGuestfs Method = "guestfs"
	MethodBoot    Method = "boot"
)

// Variant is the image build variant.
type Variant string

const (
	VariantFDE    Variant = "fde"
	VariantNonFDE Variant = "non-fde"
)

// Target identifies the artifact under validation and the versions it is
// validated against.
type Target struct {
	// ImagePath is the local qcow2 under validation.
	ImagePath string
	// ImageName is the artifact's release name, the key into checksums.txt
	// (e.g. nvke-node-fde-ubuntu-26.04.1-amd64.qcow2, nke-node-* before the
	// rename).
	ImageName string
	Variant   Variant
	Arch      string // amd64 | arm64
	Ubuntu    string // e.g. 24.04.5
}

// AppliesTo narrows a check to a subset of targets. An empty list means
// "every value".
type AppliesTo struct {
	Variant []Variant `yaml:"variant,omitempty"`
	Arch    []string  `yaml:"arch,omitempty"`
	Ubuntu  []string  `yaml:"ubuntu,omitempty"`
}

// Matches reports whether the target falls inside every populated selector.
func (a AppliesTo) Matches(t Target) bool {
	if len(a.Variant) > 0 && !contains(a.Variant, t.Variant) {
		return false
	}
	if len(a.Arch) > 0 && !contains(a.Arch, t.Arch) {
		return false
	}
	if len(a.Ubuntu) > 0 && !contains(a.Ubuntu, t.Ubuntu) {
		return false
	}
	return true
}

func contains[T comparable](s []T, v T) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// CheckDef is one entry of checks.yaml: what is verified and why, never how —
// the implementation is bound by ID in the engine.
type CheckDef struct {
	ID        string    `yaml:"id"`
	Family    string    `yaml:"family"`
	Severity  Severity  `yaml:"severity"`
	Method    Method    `yaml:"method"`
	AppliesTo AppliesTo `yaml:"appliesTo,omitempty"`
	Verifies  string    `yaml:"verifies"`
	Why       string    `yaml:"why"`
}

func (c CheckDef) validate() error {
	switch {
	case c.ID == "":
		return fmt.Errorf("check with empty id")
	case c.Verifies == "" || c.Why == "":
		return fmt.Errorf("check %s: verifies and why are both required", c.ID)
	}
	switch c.Severity {
	case SeverityBlocking, SeverityWarn:
	default:
		return fmt.Errorf("check %s: unknown severity %q", c.ID, c.Severity)
	}
	switch c.Method {
	case MethodStatic, MethodGuestfs, MethodBoot:
	default:
		return fmt.Errorf("check %s: unknown method %q", c.ID, c.Method)
	}
	return nil
}

// SkipReason says why a skipped check did not evaluate: not-applicable
// satisfies a consumer policy, tier-off and input-missing are omitted
// validation and never count as coverage.
type SkipReason string

const (
	// SkipNotApplicable: the target falls outside the check's appliesTo.
	SkipNotApplicable SkipReason = "not-applicable"
	// SkipTierOff: the check's inspection tier was not enabled for this run.
	SkipTierOff SkipReason = "tier-off"
	// SkipInputMissing: an input the check needs (evidence bundle, target
	// BOM, SBOM) was not supplied to this run.
	SkipInputMissing SkipReason = "input-missing"
)

// Result is the outcome of one check against one target.
type Result struct {
	ID       string   `json:"id"`
	Status   Status   `json:"status"`
	Severity Severity `json:"severity"`
	// Reason is set when Status is skip and says why, machine-readably.
	Reason SkipReason `json:"reason,omitempty"`
	// Evidence is one machine-grepable line saying what was observed.
	Evidence string `json:"evidence,omitempty"`
}
