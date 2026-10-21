// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checks

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// Inputs carries the facts the static tier runs on. Inspection tiers
// (guestfs, boot) add their own fact sources as they land.
type Inputs struct {
	// ImageSHA256 is the computed bare-hex digest of the image file.
	ImageSHA256 string
	// Checksums maps artifact name to its bare-hex sha256, as parsed from the
	// release's attested checksums.txt.
	Checksums map[string]string
	// Evidence reports whether (and how) the release evidence was verified
	// before the engine ran.
	Evidence EvidenceStatus
	// FS reads facts from inside the image (the guestfs tier). Nil means the
	// tier is not enabled for this run: every method=guestfs check reports a
	// visible skip rather than running or silently passing.
	FS ImageFS
	// SBOMPackages is the deb package set (name -> version) from the release's
	// SBOM for this artifact, verified by digest against the attested
	// checksums. Nil means no SBOM was available.
	SBOMPackages map[string]string
	// BOM is the target bill of materials: the component versions (deb package
	// name -> version) the validation target expects on the image. Nil means
	// the caller supplied none: the COMPAT checks that consume it report a
	// visible skip.
	BOM map[string]string
	// Boot runs probes inside a booted throwaway copy of the image (the boot
	// tier). Nil means the tier is not enabled: every method=boot check
	// reports a visible skip.
	Boot Prober
}

// ProbeResult is one command's outcome inside the booted image.
type ProbeResult struct {
	Stdout   string
	ExitCode int
}

// Prober executes a command inside the booted image and reports its output
// and exit code. A non-zero exit is data, not an error; the error return is
// for transport failures only. Implemented by pkg/inspect/qemu; faked in
// tests.
type Prober interface {
	Probe(ctx context.Context, command string) (ProbeResult, error)
}

// FileStat is the subset of stat facts checks assert on.
type FileStat struct {
	// Perm is the permission bits only (no type bits).
	Perm uint32
	UID  int
	GID  int
}

// ImageFS reads facts from an image's filesystems, read-only. Implemented by
// pkg/inspect/guestfs; faked in tests.
type ImageFS interface {
	// Exists reports whether a path exists (any file type).
	Exists(path string) (bool, error)
	// Cat returns a file's full contents.
	Cat(path string) (string, error)
	// ReadLink returns a symlink's target.
	ReadLink(path string) (string, error)
	// Stat returns permission bits and ownership for a path.
	Stat(path string) (FileStat, error)
	// HasLUKS reports whether the image carried a LUKS container.
	HasLUKS() bool
}

// EvidenceStatus is the outcome of verifying the release evidence bundle.
type EvidenceStatus struct {
	// Checked is false when the run never attempted verification (digest-only
	// --checksums mode); INT-002 then reports a skip, not a pass.
	Checked bool
	// Err is the verification failure, nil on success.
	Err error
	// SignerSAN is the verified signer identity on success.
	SignerSAN string
}

// Outcome is what a check implementation reports; the engine folds in the
// registry's ID and severity.
type Outcome struct {
	Status Status
	// Reason is required when Status is skip; the engine rejects a
	// reasonless skip so it can never read as coverage.
	Reason   SkipReason
	Evidence string
}

// Func is one check implementation. It never prints and never exits; it
// reports an Outcome or an error (which the engine records as StatusError).
type Func func(ctx context.Context, t Target, in Inputs) (Outcome, error)

// Engine binds check IDs to implementations and runs a registry in order.
type Engine struct {
	impls map[string]Func
}

// NewEngine builds an engine over the given implementations.
func NewEngine(impls map[string]Func) *Engine {
	return &Engine{impls: impls}
}

// Run evaluates every check in registry order against one target. A check
// whose appliesTo excludes the target is reported as StatusSkip; a check with
// no implementation is StatusError — a registry/engine version drift is a
// defect, not a silent pass.
func (e *Engine) Run(ctx context.Context, reg *Registry, t Target, in Inputs) []Result {
	results := make([]Result, 0, len(reg.Checks))
	for _, def := range reg.Checks {
		res := Result{ID: def.ID, Severity: def.Severity}
		switch impl, ok := e.impls[def.ID]; {
		case !def.AppliesTo.Matches(t):
			res.Status = StatusSkip
			res.Reason = SkipNotApplicable
			res.Evidence = "target outside appliesTo"
		case def.Method == MethodGuestfs && in.FS == nil:
			res.Status = StatusSkip
			res.Reason = SkipTierOff
			res.Evidence = "guestfs tier not enabled for this run"
		case def.Method == MethodBoot && in.Boot == nil:
			res.Status = StatusSkip
			res.Reason = SkipTierOff
			res.Evidence = "boot tier not enabled for this run"
		case !ok:
			res.Status = StatusError
			res.Evidence = fmt.Sprintf("no implementation bound for %s", def.ID)
		default:
			out, err := impl(ctx, t, in)
			switch {
			case err != nil:
				res.Status = StatusError
				res.Evidence = err.Error()
			case !validStatus(out.Status):
				// A status outside the vocabulary must never flow into a
				// report (and from there into a signed verdict) as if it were
				// a word consumers know: downstream policy matches on exact
				// strings, and an unknown one would match nothing — including
				// the checks that treat non-fail as acceptable.
				res.Status = StatusError
				res.Evidence = fmt.Sprintf("check %s returned unknown status %q: %s", def.ID, out.Status, out.Evidence)
			case out.Status == StatusSkip && out.Reason == "":
				// An unexplained skip cannot be policy-evaluated.
				res.Status = StatusError
				res.Evidence = fmt.Sprintf("check %s skipped without a reason: %s", def.ID, out.Evidence)
			case out.Status == StatusSkip && !validSkipReason(out.Reason):
				// Same contract for the reason taxonomy: consumers decide
				// whether a skip satisfies a profile by exact reason, so an
				// invented one is an engine error, not a new category.
				res.Status = StatusError
				res.Evidence = fmt.Sprintf("check %s skipped with unknown reason %q: %s", def.ID, out.Reason, out.Evidence)
			default:
				res.Status = out.Status
				res.Reason = out.Reason
				res.Evidence = out.Evidence
			}
		}
		results = append(results, res)
	}
	return results
}

// validStatus reports whether s is a word of the report vocabulary. The enum
// lives in types.go; a new status is added there and here together.
func validStatus(s Status) bool {
	switch s {
	case StatusPass, StatusFail, StatusSkip, StatusError:
		return true
	}
	return false
}

// validSkipReason reports whether r is a word of the skip-reason taxonomy.
func validSkipReason(r SkipReason) bool {
	switch r {
	case SkipNotApplicable, SkipTierOff, SkipInputMissing:
		return true
	}
	return false
}

// newBytesReader exists so registry.go needs no direct bytes import juggling.
func newBytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
