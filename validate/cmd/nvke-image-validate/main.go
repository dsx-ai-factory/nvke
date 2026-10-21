// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// nvke-image-validate checks a built node image against its release evidence
// and the versioned check set, entirely offline. The run itself lives in
// pkg/runner, shared with `nvkectl validate image`; this binary owns the flag
// surface and the three-way exit contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
	"github.com/dsx-ai-factory/nvke/validate/pkg/runner"
)

// version is set at build time via -X main.version=.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("nvke-image-validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	versionFlag := fs.Bool("version", false, "Print the version and exit")
	image := fs.String("image", "", "Path to the qcow2 under validation")
	bundleDir := fs.String("bundle", "", "Path to the release evidence bundle directory (checksums.txt + checksums.txt.sigstore.json); verifies the evidence before any check runs (requires --trusted-root)")
	trustedRoot := fs.String("trusted-root", "", "Path to the operator-pinned trusted root that authenticates the evidence (bundle mode); pinned out-of-band, never read from the evidence directory")
	certIssuer := fs.String("cert-issuer", runner.DefaultCertIssuer, "OIDC issuer the evidence signer's certificate must carry (bundle mode)")
	certSANRegex := fs.String("cert-san-regex", runner.DefaultCertSANRegex, "Regex the evidence signer's SubjectAlternativeName must match (bundle mode)")
	checksums := fs.String("checksums", "", "Path to a bare checksums.txt: digest-only mode, no evidence verification — INT-002 reports a skip")
	registryPath := fs.String("checks", "", "Path to checks.yaml")
	bomPath := fs.String("target-bom", "", "Path to a target BOM YAML (components: {name: version}) naming the component versions the target expects; omitted, the COMPAT checks that consume it report a skip")
	name := fs.String("name", "", "Artifact release name; defaults to the image file's basename")
	variant := fs.String("target-variant", "", "Target variant: fde | non-fde")
	arch := fs.String("target-arch", "", "Target architecture: amd64 | arm64")
	ubuntu := fs.String("target-ubuntu", "", "Target Ubuntu release, e.g. 24.04.5")
	inspect := fs.String("inspect", "auto", "guestfs inspection tier: auto (on when guestfish is on PATH) | on | off")
	bootTier := fs.String("boot", "off", "boot tier (boots a throwaway copy under QEMU, minutes): on | off")
	bootTimeout := fs.Duration("boot-timeout", 0, "Wait bound for the boot tier's probe channel (0 = default); raise for FDE images under TCG")
	luksKeyFile := fs.String("luks-passphrase-file", "", "File holding the LUKS passphrase for FDE images (never pass the passphrase itself on the command line)")
	emitVerdict := fs.String("emit-verdict", "", "Write the run's result as a DSSE-signed in-toto attestation to this path (requires --sign-key); a failing run still emits its verdict")
	signKey := fs.String("sign-key", "", "PEM private key (Ed25519 or ECDSA P-256) that signs the verdict")
	format := fs.String("format", "text", "Output format: text | json")
	if err := fs.Parse(args); err != nil {
		return report.ExitError
	}
	if *versionFlag {
		fmt.Fprintf(stdout, "nvke-image-validate version: %s\n", version)
		return report.ExitPass
	}
	for flagName, v := range map[string]string{
		"image": *image, "checks": *registryPath,
		"target-variant": *variant, "target-arch": *arch, "target-ubuntu": *ubuntu,
	} {
		if v == "" {
			fmt.Fprintf(stderr, "nvke-image-validate: --%s is required\n", flagName)
			fs.Usage()
			return report.ExitError
		}
	}
	if (*bundleDir == "") == (*checksums == "") {
		fmt.Fprintln(stderr, "nvke-image-validate: exactly one of --bundle or --checksums is required")
		fs.Usage()
		return report.ExitError
	}
	if *bundleDir != "" && *trustedRoot == "" {
		fmt.Fprintln(stderr, "nvke-image-validate: --bundle requires --trusted-root (the root is pinned out-of-band from the evidence it authenticates)")
		fs.Usage()
		return report.ExitError
	}
	if (*emitVerdict == "") != (*signKey == "") {
		fmt.Fprintln(stderr, "nvke-image-validate: --emit-verdict and --sign-key are required together")
		fs.Usage()
		return report.ExitError
	}

	exit, err := runner.Run(context.Background(), runner.Options{
		ImagePath:          *image,
		Name:               *name,
		BundleDir:          *bundleDir,
		ChecksumsPath:      *checksums,
		CertIssuer:         *certIssuer,
		CertSANRegex:       *certSANRegex,
		RegistryPath:       *registryPath,
		Variant:            *variant,
		Arch:               *arch,
		Ubuntu:             *ubuntu,
		BOMPath:            *bomPath,
		Inspect:            *inspect,
		Boot:               *bootTier,
		BootTimeout:        *bootTimeout,
		LUKSPassphraseFile: *luksKeyFile,
		EmitVerdictPath:    *emitVerdict,
		SignKeyPath:        *signKey,
		Format:             *format,
		ValidatorVersion:   version,
		TrustedRootPath:    *trustedRoot,
	}, stdout)
	if err != nil {
		// Runner contract: err != nil means nothing reached stdout, so this
		// never produces a second document.
		if *format == "json" {
			if out, jerr := report.NewRunError(err, time.Now()).JSON(); jerr == nil {
				_, _ = stdout.Write(out)
			}
		}
		fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
		return exit
	}
	if *emitVerdict != "" && *checksums != "" {
		fmt.Fprintln(stderr, "nvke-image-validate: note: this verdict covers a digest-only run — the release evidence itself was not verified (evidenceMode=digest-only)")
	}
	return exit
}
