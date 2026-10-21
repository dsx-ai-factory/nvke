// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// nvke-verdict re-checks a signed image-validation verdict, entirely offline.
// It is the reference consumer of pkg/verify: what a registration or
// provisioning gate runs (or links) instead of ever re-reading the image.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/static"
	"github.com/dsx-ai-factory/nvke/validate/pkg/verify"
)

// version is set at build time via -X main.version=.
var version = "dev"

// Exit codes mirror the validator's contract: 0 the verdict is authentic,
// applicable and passing; 1 authentic and applicable but not passing; 2 the
// verdict could not be verified (which must never read as a pass).
const (
	exitPass   = 0
	exitFail   = 1
	exitCannot = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("nvke-verdict", flag.ContinueOnError)
	fs.SetOutput(stderr)
	versionFlag := fs.Bool("version", false, "Print the version and exit")
	attestation := fs.String("attestation", "", "Path to the DSSE verdict attestation to verify")
	keyPath := fs.String("key", "", "PEM public key(s) accepted as verdict signers (multiple PUBLIC KEY blocks support rotation)")
	digest := fs.String("digest", "", "Bare-hex sha256 the verdict must be about")
	image := fs.String("image", "", "Image file to hash instead of passing --digest")
	minChecks := fs.String("min-check-set-version", "", "Lowest acceptable check-set version")
	maxAge := fs.Duration("max-age", 0, "Reject verdicts older than this (0 = no limit)")
	requiredChecks := fs.String("required-checks", "", "Comma-separated check IDs the qualification profile demands: each must have passed or be skipped as not-applicable — a disabled tier or missing input does not count (e.g. INT-001,INT-002,INT-004,HARD-001)")
	checkSetSHA256 := fs.String("check-set-sha256", "", "Comma-separated sha256 digests of approved checks.yaml contents; without it, applicability is whatever registry the signer chose")
	trustedRootSHA256 := fs.String("trusted-root-sha256", "", "Comma-separated sha256 digests of approved trusted roots the run may have verified evidence against; a digest-only verdict (no evidence verified) fails this pin")
	expectVariant := fs.String("target-variant", "", "Variant the verdict must have been validated as (fde | non-fde)")
	expectArch := fs.String("target-arch", "", "Architecture the verdict must have been validated as")
	expectUbuntu := fs.String("target-ubuntu", "", "Ubuntu release the verdict must have been validated as")
	if err := fs.Parse(args); err != nil {
		return exitCannot
	}
	if *versionFlag {
		fmt.Fprintf(stdout, "nvke-verdict version: %s\n", version)
		return exitPass
	}
	if *attestation == "" || *keyPath == "" {
		fmt.Fprintln(stderr, "nvke-verdict: --attestation and --key are required")
		fs.Usage()
		return exitCannot
	}
	if (*digest == "") == (*image == "") {
		fmt.Fprintln(stderr, "nvke-verdict: exactly one of --digest or --image is required")
		fs.Usage()
		return exitCannot
	}
	subject := strings.ToLower(*digest)
	if *image != "" {
		var err error
		if subject, err = static.HashFile(*image); err != nil {
			fmt.Fprintf(stderr, "nvke-verdict: %v\n", err)
			return exitCannot
		}
	}
	envelope, err := os.ReadFile(*attestation)
	if err != nil {
		fmt.Fprintf(stderr, "nvke-verdict: %v\n", err)
		return exitCannot
	}
	keyPEM, err := os.ReadFile(*keyPath)
	if err != nil {
		fmt.Fprintf(stderr, "nvke-verdict: %v\n", err)
		return exitCannot
	}
	keys, err := verify.LoadPublicKeys(keyPEM)
	if err != nil {
		fmt.Fprintf(stderr, "nvke-verdict: %v\n", err)
		return exitCannot
	}

	var required []string
	for _, id := range strings.Split(*requiredChecks, ",") {
		if id = strings.TrimSpace(id); id != "" {
			required = append(required, id)
		}
	}
	var approvedCheckSets []string
	for _, d := range strings.Split(*checkSetSHA256, ",") {
		if d = strings.TrimSpace(d); d != "" {
			approvedCheckSets = append(approvedCheckSets, d)
		}
	}
	var approvedRoots []string
	for _, d := range strings.Split(*trustedRootSHA256, ",") {
		if d = strings.TrimSpace(d); d != "" {
			approvedRoots = append(approvedRoots, d)
		}
	}
	givenEmptyPin := ""
	fs.Visit(func(f *flag.Flag) {
		// A pin flag that was given but parsed to nothing is a usage error: it
		// looks like policy and enforces nothing.
		if f.Name == "check-set-sha256" && len(approvedCheckSets) == 0 {
			givenEmptyPin = f.Name
		}
		if f.Name == "trusted-root-sha256" && len(approvedRoots) == 0 {
			givenEmptyPin = f.Name
		}
	})
	if givenEmptyPin != "" {
		fmt.Fprintf(stderr, "nvke-verdict: --%s was given but holds no digests\n", givenEmptyPin)
		return exitCannot
	}
	st, err := verify.Verify(envelope, subject, verify.Policy{
		Keys:               keys,
		MinCheckSetVersion: *minChecks,
		MaxAge:             *maxAge,
		RequiredChecks:     required,
		CheckSetSHA256:     approvedCheckSets,
		ExpectedTarget:     verify.Target{Variant: *expectVariant, Arch: *expectArch, Ubuntu: *expectUbuntu},
		TrustedRootSHA256:  approvedRoots,
	}, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "nvke-verdict: %v\n", err)
		return exitCannot
	}
	name := ""
	for _, s := range st.Subject {
		if strings.ToLower(s.Digest["sha256"]) == subject {
			name = s.Name
			break
		}
	}
	evidenceMode := st.Predicate.EvidenceMode
	if evidenceMode == "" {
		evidenceMode = "not recorded (pre-evidenceMode verdict)"
	}
	fmt.Fprintf(stdout, "verified: %s sha256:%s\n  outcome: %s\n  check set: %s\n  evidence mode: %s\n  validator: %s %s\n  finished: %s\n",
		name, subject,
		st.Predicate.Outcome, st.Predicate.CheckSetVersion, evidenceMode,
		st.Predicate.Validator.Name, st.Predicate.Validator.Version,
		st.Predicate.FinishedAt.Format(time.RFC3339))
	if st.Predicate.Outcome != verify.OutcomePass {
		return exitFail
	}
	return exitPass
}
