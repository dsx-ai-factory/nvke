// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command osprofiles-to-crs reads an OS profile tree and writes reviewable YAML
// for one MachineImage, one NodePoolRelease, and one ClusterRelease per profile.
//
// Both metadata.yaml and optional deprecated.yaml are imported, the latter
// reading its profile files from osProfiles/deprecated/. Every profile keeps
// its name, so a pool that already selects a deprecated profile still
// resolves it. Deprecated imports set spec.lifecycle.deprecated on both
// releases; upgrade-target search excludes that mark. kubernetes_version and
// kubernetes_version_injected are copied onto the release annotations.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

// defaultCRDDir is the generated CRD directory, relative to the controller
// module, which is where the converter runs from.
const (
	defaultCRDDir = "../management-engine/config/crd/bases"
)

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "osprofiles-to-crs: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("osprofiles-to-crs", flag.ContinueOnError)
	profiles := flags.String("profiles", "", "Profiles root containing osProfiles (the controller's profilesDir)")
	output := flags.String("output", "", "Flat directory for generated release manifests")
	crds := flags.String("crds", defaultCRDDir, "Directory of the CRD manifests every generated manifest is validated against")
	license := flags.String("license", licenseProprietary, "SPDX license identifier of the generated headers: "+licenseProprietary+" or "+licenseApache)
	gitops := flags.Bool("gitops", true, "Annotate each manifest with its Argo CD sync wave and Prune=false; use --gitops=false for manifests applied another way")
	check := flags.Bool("check", false, "Fail when the output directory is not an exact regeneration")
	flags.Usage = func() {
		out := flags.Output()
		if _, err := fmt.Fprint(out, `Usage: osprofiles-to-crs --profiles <dir> --output <dir>

Reads osProfiles/metadata.yaml, optional deprecated.yaml, and each profile's
ipxeScript and userData.tmpl, from osProfiles/<profile>/ when the profile is
listed in metadata.yaml and osProfiles/deprecated/<profile>/ when it is listed
in deprecated.yaml. Writes three manifests per profile:
<profile>-machineimage.yaml, <profile>-nodepoolrelease.yaml, and
<profile>-clusterrelease.yaml. Deprecated profiles keep their names so existing
pools still resolve them, and spec.lifecycle.deprecated excludes them from
upgrade-target search. Regeneration may update that deprecation mark. It fails
if a published spec would change. Every manifest is first admitted against the
CRDs (schema, CEL rules) in --crds, so a value the API server would refuse, such
as an invalid variant, fails here. --check writes nothing.

`); err != nil {
			return
		}
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *profiles == "" || *output == "" {
		return errors.New("both --profiles and --output are required")
	}

	docs, err := convertWith(*profiles, *gitops)
	if err != nil {
		return err
	}
	if err := relicense(docs, *license); err != nil {
		return err
	}
	schemas, err := loadSchemas(*crds)
	if err != nil {
		return err
	}
	if err := validateDocuments(schemas, docs); err != nil {
		return fmt.Errorf("generated manifests do not satisfy the CRDs in %s: %w", *crds, err)
	}
	if err := publish(*output, docs, *check); err != nil {
		return err
	}
	if *check {
		fmt.Fprintf(os.Stdout, "%d manifests match the profiles\n", len(docs))
		return nil
	}
	fmt.Fprintf(os.Stdout, "wrote %d manifests\n", len(docs))
	return nil
}
