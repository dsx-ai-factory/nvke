// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"flag"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/environment"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/qualification"
)

var (
	cfgPath string

	// envName selects infrastructure that exists. It has no default: an
	// environment cannot be inferred, so a backend needing one fails without
	// it rather than falling through to something sensible. The simulator
	// backends need none and ignore it.
	envName string

	// versionsFlag and capabilitiesFlag carry what a qualification run is
	// qualifying and what it needs to run on. Both are empty for a developer
	// running the suite by hand against the simulators.
	versionsFlag     string
	capabilitiesFlag string
)

func init() {
	flag.StringVar(&cfgPath, "e2e.config", "e2e-config.yaml", "path to the E2E config file (missing file -> all defaults)")
	flag.StringVar(&envName, "e2e.environment", "", "name of an environment in environments/ describing real infrastructure (required by nico-forge)")
	flag.StringVar(&versionsFlag, "e2e.versions", "", "component versions under qualification, e.g. controller=v1.4.0-rc.1,os-image=v0.9.1,cli=v0.3.0")
	flag.StringVar(&capabilitiesFlag, "e2e.capabilities", "", "capabilities the run requires, e.g. gpu=gb200,arch=amd64,kube=1.37")
}

// The flags are parsed once and shared. Specs read them through the accessors
// below rather than re-parsing, so a malformed flag is one failure rather than
// one per entry in the table.
var (
	selectionOnce    sync.Once
	selectionValue   qualification.Selection
	errSelection     error
	environmentOnce  sync.Once
	environmentValue *environment.Environment
	errEnvironment   error
)

// runSelection is what this run was asked to qualify.
//
// Parsed by internal/qualification rather than here: the format is a contract
// with the qualify tool that emits it, and a suite-local parser is a second
// place for the two sides to drift apart. Lazily rather than in BeforeSuite,
// because specs read the versions while building their tables.
func runSelection() (qualification.Selection, error) {
	selectionOnce.Do(func() {
		selectionValue, errSelection = qualification.Parse(versionsFlag, capabilitiesFlag)
		selectionValue.Environment = envName
	})
	return selectionValue, errSelection
}

// runVersions is what this run is qualifying, keyed by component name.
func runVersions() (map[string]string, error) {
	s, err := runSelection()
	return s.Versions, err
}

// runCapabilities is what this run needs to execute on. Infrastructure keys
// select a machine; the rest, such as kube, are software and select nothing.
func runCapabilities() (map[string]string, error) {
	s, err := runSelection()
	return s.Capabilities, err
}

// runEnvironment is the infrastructure this run was pointed at, read once.
//
// It stops at the generic declaration and does not decode a provider's
// dialect, because -e2e.environment names a file rather than a backend: which
// provider wrote it is in the file, and the specs that speak one provider's
// vocabulary are the ones that should project it. Loaded on first use so the
// simulator backends, which need no environment, keep running with the flag
// unset.
func runEnvironment() (*environment.Environment, error) {
	environmentOnce.Do(func() {
		environmentValue, errEnvironment = environment.Load(envName)
	})
	return environmentValue, errEnvironment
}

// requireVersion reads one component's version, naming the component when the
// run did not supply it. Qualifying a component means pinning it, so an absent
// version is a missing input rather than something to default.
func requireVersion(component string) (string, error) {
	versions, err := runVersions()
	if err != nil {
		return "", err
	}
	version, ok := versions[component]
	if !ok {
		known := make([]string, 0, len(versions))
		for name := range versions {
			known = append(known, name)
		}
		sort.Strings(known)
		if len(known) == 0 {
			return "", fmt.Errorf("-e2e.versions names no components, and %q is required", component)
		}
		return "", fmt.Errorf("-e2e.versions does not name %q; it names %s", component, strings.Join(known, ", "))
	}
	return version, nil
}

// optionalVersion reads one component's version, returning "" when the run did
// not name it, for the versions a caller can sensibly default.
// requireNodeArtifacts is the OS image and its imager for one architecture.
//
// Both are looked up as <component>-<arch> first and then unsuffixed, so a run
// qualifying one architecture states two keys and a mixed run states four. The
// unsuffixed form is not a default for the other architecture: it is what a
// single-architecture run has always passed, and dropping it would mean
// changing every existing invocation to say something it does not care about.
//
// Required rather than optional because the OS image is the component under
// qualification. A run that named none for the architecture it is about to
// boot has not said what it is qualifying.
func requireNodeArtifacts(arch string) (string, string, error) {
	bootLoaderURL, err := requireVersionForArch("os-image-bootloader", arch)
	if err != nil {
		return "", "", err
	}
	imageURL, err := requireVersionForArch("os-image", arch)
	if err != nil {
		return "", "", err
	}
	return bootLoaderURL, imageURL, nil
}

func requireVersionForArch(component, arch string) (string, error) {
	if value := optionalVersion(component + "-" + arch); value != "" {
		return value, nil
	}
	value, err := requireVersion(component)
	if err != nil {
		return "", fmt.Errorf("no %s-%s and %w", component, arch, err)
	}
	return value, nil
}

func optionalVersion(component string) string {
	versions, err := runVersions()
	if err != nil {
		return ""
	}
	return versions[component]
}

// Specs reach NKE over a port-forward they share and establish lazily; this
// closes it. A no-op when no spec ever talked to NKE.
var _ = AfterSuite(func() {
	kube.CloseSharedForwarders()
})

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NKE E2E Suite")
}
