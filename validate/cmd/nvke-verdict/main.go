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

	st, err := verify.Verify(envelope, subject, verify.Policy{
		Keys:               keys,
		MinCheckSetVersion: *minChecks,
		MaxAge:             *maxAge,
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
	fmt.Fprintf(stdout, "verified: %s sha256:%s\n  outcome: %s\n  check set: %s\n  validator: %s %s\n  finished: %s\n",
		name, subject,
		st.Predicate.Outcome, st.Predicate.CheckSetVersion,
		st.Predicate.Validator.Name, st.Predicate.Validator.Version,
		st.Predicate.FinishedAt.Format(time.RFC3339))
	if st.Predicate.Outcome != verify.OutcomePass {
		return exitFail
	}
	return exitPass
}
