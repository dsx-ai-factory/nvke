// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package runner drives a full validation run from resolved options: evidence
// loading, inspection-tier setup, engine execution, reporting and verdict
// emission. It is the shared engine behind cmd/nvke-image-validate and
// `nvkectl validate image`; flag parsing stays with each CLI.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/validate/pkg/bundle"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/boot"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/imagefs"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/static"
	"github.com/dsx-ai-factory/nvke/validate/pkg/inspect/guestfs"
	"github.com/dsx-ai-factory/nvke/validate/pkg/inspect/qemu"
	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
	"github.com/dsx-ai-factory/nvke/validate/pkg/verify"
)

// Evidence-signer identity defaults: the release workflow that attests
// NVIDIA-built node-image evidence.
const (
	DefaultCertIssuer   = "https://token.actions.githubusercontent.com"
	DefaultCertSANRegex = `^https://github\.com/dsx-ai-factory/nvke/\.github/workflows/os-image-artifact\.yml@.+$`
)

// Options is one validation run, fully resolved. Zero values take the
// documented defaults where one exists; the required fields are ImagePath,
// RegistryPath, Variant, Arch, Ubuntu and exactly one of BundleDir or
// ChecksumsPath.
type Options struct {
	// ImagePath is the qcow2 under validation.
	ImagePath string
	// Name is the artifact's release name; empty means ImagePath's basename.
	Name string
	// BundleDir is the release evidence bundle directory; verified before any
	// check runs. Exactly one of BundleDir and ChecksumsPath must be set.
	BundleDir string
	// ChecksumsPath is a bare checksums.txt: digest-only mode, with the
	// unverified evidence reported as a visible skip.
	ChecksumsPath string
	// CertIssuer and CertSANRegex pin the evidence signer's identity in bundle
	// mode; empty means the NVIDIA release workflow defaults above.
	CertIssuer   string
	CertSANRegex string
	// RegistryPath is the checks.yaml to run.
	RegistryPath string
	// Variant, Arch and Ubuntu are the validation target.
	Variant string
	Arch    string
	Ubuntu  string
	// BOMPath names the component versions the target expects; empty means the
	// BOM-driven COMPAT checks report a skip.
	BOMPath string
	// Inspect controls the guestfs tier: auto (on when guestfish is on PATH),
	// on, or off. Empty means auto.
	Inspect string
	// Boot controls the boot tier (boots a throwaway copy under QEMU): on or
	// off. Empty means off.
	Boot string
	// BootTimeout bounds the wait for the booted image's probe channel; zero
	// means the qemu package default. Real FDE images under TCG need more.
	BootTimeout time.Duration
	// LUKSPassphraseFile holds the passphrase for opening FDE images.
	LUKSPassphraseFile string
	// EmitVerdictPath writes the run's result as a DSSE-signed attestation;
	// it requires SignKeyPath (a PEM Ed25519 or ECDSA P-256 private key).
	EmitVerdictPath string
	SignKeyPath     string
	// Format is the report rendering: text or json. Empty means text.
	Format string
	// ValidatorVersion is stamped into emitted verdicts.
	ValidatorVersion string
	// TrustedRootPath is the operator-pinned trusted root that authenticates
	// the evidence bundle's attestation (one JSON root per line, the format
	// `gh attestation trusted-root` emits). Required with BundleDir, and
	// deliberately never read from the evidence directory: a root that
	// travels with the evidence it authenticates could be replaced with it.
	TrustedRootPath string
}

// Run executes one validation run, writing the report to stdout: exit 0 pass,
// 1 blocking fail, 2 could-not-evaluate. At most one document reaches stdout —
// a non-nil error means nothing was written, and a post-report failure rides
// the report as runError.
func Run(ctx context.Context, opts Options, stdout io.Writer) (int, error) {
	for field, v := range map[string]string{
		"image": opts.ImagePath, "checks registry": opts.RegistryPath,
		"target variant": opts.Variant, "target arch": opts.Arch, "target ubuntu": opts.Ubuntu,
	} {
		if v == "" {
			return report.ExitError, fmt.Errorf("the %s is required", field)
		}
	}
	if (opts.BundleDir == "") == (opts.ChecksumsPath == "") {
		return report.ExitError, fmt.Errorf("exactly one of the evidence bundle and the checksums file is required")
	}
	if opts.BundleDir != "" && opts.TrustedRootPath == "" {
		return report.ExitError, fmt.Errorf("bundle mode requires the operator-pinned trusted root (--trusted-root): the root must arrive out-of-band from the evidence it authenticates")
	}
	if opts.BundleDir == "" && opts.TrustedRootPath != "" {
		return report.ExitError, fmt.Errorf("the trusted root only applies to bundle mode: a digest-only run verifies no evidence")
	}
	if (opts.EmitVerdictPath == "") != (opts.SignKeyPath == "") {
		return report.ExitError, fmt.Errorf("verdict emission needs both the output path and the signing key")
	}
	switch opts.Format {
	case "", "text", "json":
	default:
		// Validated before any work: rejecting the format only at render time
		// would leave side effects (a signed verdict) behind an exit 2.
		return report.ExitError, fmt.Errorf("unknown report format %q (want text or json)", opts.Format)
	}
	if err := distinctSink(opts.EmitVerdictPath, stdout); err != nil {
		return report.ExitError, err
	}
	name := opts.Name
	if name == "" {
		name = filepath.Base(opts.ImagePath)
	}
	certIssuer := opts.CertIssuer
	if certIssuer == "" {
		certIssuer = DefaultCertIssuer
	}
	certSANRegex := opts.CertSANRegex
	if certSANRegex == "" {
		certSANRegex = DefaultCertSANRegex
	}

	// One read per policy input: the bytes hashed into the verdict must be
	// the bytes that were parsed and ran.
	registryRaw, err := os.ReadFile(opts.RegistryPath)
	if err != nil {
		return report.ExitError, fmt.Errorf("reading check registry: %w", err)
	}
	reg, err := checks.ParseRegistry(registryRaw)
	if err != nil {
		return report.ExitError, fmt.Errorf("check registry %s: %w", opts.RegistryPath, err)
	}
	checkSetSHA256 := sha256Hex(registryRaw)
	var sums map[string]string
	var evidence checks.EvidenceStatus
	var evidenceBundle *bundle.Bundle
	trustedRootSHA256 := ""
	if opts.BundleDir != "" {
		b, err := bundle.Load(opts.BundleDir)
		if err != nil {
			return report.ExitError, err
		}
		// The root is read from the operator-pinned path and hashed from the
		// same bytes that are parsed, like every other policy input.
		rootRaw, err := os.ReadFile(opts.TrustedRootPath)
		if err != nil {
			return report.ExitError, fmt.Errorf("reading trusted root: %w", err)
		}
		if err := b.PinTrustedRoot(rootRaw); err != nil {
			return report.ExitError, fmt.Errorf("trusted root %s: %w", opts.TrustedRootPath, err)
		}
		trustedRootSHA256 = sha256Hex(rootRaw)
		sums = b.Checksums
		evidenceBundle = b
		evidence.Checked = true
		ev, err := b.VerifyEvidence(bundle.Identity{Issuer: certIssuer, SANRegex: certSANRegex})
		if err != nil {
			evidence.Err = err
		} else {
			evidence.SignerSAN = ev.SignerSAN
		}
	} else {
		sums, err = static.ParseChecksums(opts.ChecksumsPath)
		if err != nil {
			return report.ExitError, err
		}
	}
	digest, err := static.HashFile(opts.ImagePath)
	if err != nil {
		return report.ExitError, err
	}
	var bom map[string]string
	bomSHA256 := ""
	if opts.BOMPath != "" {
		bomRaw, err := os.ReadFile(opts.BOMPath)
		if err != nil {
			return report.ExitError, fmt.Errorf("reading target BOM: %w", err)
		}
		if bom, err = static.ParseBOMBytes(bomRaw); err != nil {
			return report.ExitError, fmt.Errorf("target BOM %s: %w", opts.BOMPath, err)
		}
		bomSHA256 = sha256Hex(bomRaw)
	}

	var imgFS checks.ImageFS
	var sbom map[string]string
	runInspection := false
	switch opts.Inspect {
	case "on":
		runInspection = true
	case "auto", "":
		runInspection = guestfs.Available()
	case "off":
	default:
		return report.ExitError, fmt.Errorf("unknown inspect mode %q (want auto, on or off)", opts.Inspect)
	}
	if runInspection {
		var passphrase string
		if opts.LUKSPassphraseFile != "" {
			raw, err := os.ReadFile(opts.LUKSPassphraseFile)
			if err != nil {
				return report.ExitError, err
			}
			passphrase = strings.TrimRight(string(raw), "\n")
		}
		session, err := guestfs.Open(ctx, opts.ImagePath, guestfs.Options{LUKSPassphrase: passphrase})
		if err != nil {
			return report.ExitError, fmt.Errorf("opening image for inspection: %w", err)
		}
		defer session.Close()
		imgFS = session
	}
	// The SBOM backs INT-004 (inspection) and COMPAT-002 (target BOM), so it
	// loads whenever either consumer runs; in digest-only mode COMPAT-002
	// reports the missing SBOM itself.
	if evidenceBundle != nil && (runInspection || bom != nil) {
		sbom, err = evidenceBundle.LoadSBOM(name)
		if err != nil {
			return report.ExitError, err
		}
	}

	var prober checks.Prober
	switch opts.Boot {
	case "off", "":
	case "on":
		if !qemu.Available(opts.Arch) {
			return report.ExitError, fmt.Errorf("the boot tier needs qemu-system for %s plus cloud-localds or genisoimage", opts.Arch)
		}
		workDir, err := os.MkdirTemp("", "nvke-validate-boot-*")
		if err != nil {
			return report.ExitError, err
		}
		defer os.RemoveAll(workDir)
		keyPath, pubKey, err := qemu.GenerateSSHKey(workDir)
		if err != nil {
			return report.ExitError, err
		}
		seed, err := qemu.MakeSeed(workDir, "nvkeprobe", pubKey)
		if err != nil {
			return report.ExitError, err
		}
		vm, err := qemu.Boot(ctx, opts.ImagePath, qemu.Options{Arch: opts.Arch, SeedISO: seed, SSHKeyPath: keyPath, BootTimeout: opts.BootTimeout})
		if err != nil {
			return report.ExitError, fmt.Errorf("boot tier: %w", err)
		}
		defer vm.Close()
		prober = vm
	default:
		return report.ExitError, fmt.Errorf("unknown boot mode %q (want on or off)", opts.Boot)
	}

	target := checks.Target{
		ImagePath: opts.ImagePath,
		ImageName: name,
		Variant:   checks.Variant(opts.Variant),
		Arch:      opts.Arch,
		Ubuntu:    opts.Ubuntu,
	}
	impls := imagefs.Merge(static.Impls(), imagefs.Impls(), boot.Impls())
	results := checks.NewEngine(impls).Run(ctx, reg, target, checks.Inputs{
		ImageSHA256:  digest,
		Checksums:    sums,
		Evidence:     evidence,
		FS:           imgFS,
		SBOMPackages: sbom,
		BOM:          bom,
		Boot:         prober,
	})
	// The inspection tiers reopen the image by path; re-hash after they ran
	// so the digest in the report and verdict describes the bytes that were
	// actually inspected.
	if imgFS != nil || prober != nil {
		recheck, err := static.HashFile(opts.ImagePath)
		if err != nil {
			return report.ExitError, fmt.Errorf("re-hashing image after inspection: %w", err)
		}
		if recheck != digest {
			return report.ExitError, fmt.Errorf("image changed during validation: sha256 was %s, is now %s", digest, recheck)
		}
	}

	rep := report.New(reg, target, digest, results, time.Now())
	if opts.EmitVerdictPath != "" {
		if err := writeVerdict(opts, rep, checkSetSHA256, bomSHA256, trustedRootSHA256, stdout); err != nil {
			// The signing failure rides the one report document, never a
			// second JSON object.
			rep.RunError = fmt.Sprintf("emitting verdict: %v", err)
			rep.Summary.Error++
		}
	}

	switch opts.Format {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			return report.ExitError, err
		}
		if _, err := stdout.Write(out); err != nil {
			// Output may have started; a broken sink gets exit 2 and no
			// second document.
			return report.ExitError, nil
		}
	default: // "text" or "" — anything else was rejected up front.
		if _, err := io.WriteString(stdout, rep.Text()); err != nil {
			return report.ExitError, nil
		}
	}
	return rep.ExitCode(), nil
}

// distinctSink refuses a verdict path that aliases the report stream: both
// documents interleaved in one file is neither a report nor a verdict.
func distinctSink(verdictPath string, stdout io.Writer) error {
	if verdictPath == "" {
		return nil
	}
	f, ok := stdout.(*os.File)
	if !ok {
		return nil
	}
	vi, err := os.Stat(verdictPath)
	if err != nil {
		return nil // not created yet; a fresh file cannot alias the stream
	}
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	if os.SameFile(vi, fi) {
		return fmt.Errorf("the verdict path %s aliases the report output; write them to distinct destinations", verdictPath)
	}
	return nil
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// writeVerdict signs the run's result as an in-toto attestation over the
// image's digest. A failing or erroring run still emits its verdict: the
// signed outcome is the evidence a consumer enforces on.
func writeVerdict(opts Options, rep report.Report, checkSetSHA256, bomSHA256, trustedRootSHA256 string, reportSink io.Writer) error {
	keyPEM, err := os.ReadFile(opts.SignKeyPath)
	if err != nil {
		return fmt.Errorf("reading signing key: %w", err)
	}
	signer, err := verify.LoadSigner(keyPEM)
	if err != nil {
		return err
	}
	outcome := verify.OutcomeError
	switch rep.ExitCode() {
	case report.ExitPass:
		outcome = verify.OutcomePass
	case report.ExitFail:
		outcome = verify.OutcomeFail
	}
	validatorVersion := opts.ValidatorVersion
	if validatorVersion == "" {
		validatorVersion = "unknown"
	}
	results := make([]verify.CheckResult, 0, len(rep.Results))
	for _, r := range rep.Results {
		results = append(results, verify.CheckResult{ID: r.ID, Status: string(r.Status), Severity: string(r.Severity), Reason: string(r.Reason), Evidence: r.Evidence})
	}
	evidenceMode := verify.EvidenceModeBundle
	if opts.BundleDir == "" {
		evidenceMode = verify.EvidenceModeDigestOnly
	}
	statement := verify.Statement{
		Type:          verify.StatementType,
		Subject:       []verify.Subject{{Name: rep.Subject.Name, Digest: map[string]string{"sha256": rep.Subject.SHA256}}},
		PredicateType: verify.PredicateType,
		Predicate: verify.Predicate{
			Outcome:             outcome,
			CheckSetVersion:     rep.CheckSetVersion,
			ReportSchemaVersion: rep.SchemaVersion,
			Validator:           verify.Tool{Name: "nvke-image-validate", Version: validatorVersion},
			Target:              verify.Target{Variant: string(rep.Target.Variant), Arch: rep.Target.Arch, Ubuntu: rep.Target.Ubuntu},
			Summary: verify.Summary{
				Pass: rep.Summary.Pass, Fail: rep.Summary.Fail, BlockingFail: rep.Summary.BlockingFail,
				WarnFail: rep.Summary.WarnFail, Skip: rep.Summary.Skip, Error: rep.Summary.Error,
			},
			Results:           results,
			FinishedAt:        rep.GeneratedAt,
			EvidenceMode:      evidenceMode,
			CheckSetSHA256:    checkSetSHA256,
			BOMSHA256:         bomSHA256,
			TrustedRootSHA256: trustedRootSHA256,
		},
	}
	payload, err := json.Marshal(statement)
	if err != nil {
		return fmt.Errorf("encoding verdict statement: %w", err)
	}
	envelope, err := verify.Sign(payload, signer)
	if err != nil {
		return err
	}
	// Open first and compare descriptors: a device path like /dev/stdout
	// resolves only at open time (the path-stat preflight cannot see through
	// it on macOS), and the envelope must never land in the report stream.
	f, err := os.OpenFile(opts.EmitVerdictPath, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening verdict path: %w", err)
	}
	defer f.Close()
	if sink, ok := reportSink.(*os.File); ok {
		vi, vErr := f.Stat()
		si, sErr := sink.Stat()
		if vErr == nil && sErr == nil && os.SameFile(vi, si) {
			return fmt.Errorf("the verdict path %s aliases the report output; write them to distinct destinations", opts.EmitVerdictPath)
		}
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("writing verdict: %w", err)
	}
	if _, err := f.Write(envelope); err != nil {
		return fmt.Errorf("writing verdict: %w", err)
	}
	return nil
}
