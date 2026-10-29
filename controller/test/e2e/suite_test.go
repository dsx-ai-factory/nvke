// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/klog/v2"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/environment"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/qualification"
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

// requireVersions reads several components' versions at once, naming every one
// the run did not supply. A Forge run qualifies a set of versions, so a missing
// one is reported together with the others rather than one run at a time.
func requireVersions(components ...string) (map[string]string, error) {
	versions, err := runVersions()
	if err != nil {
		return nil, err
	}
	return pickVersions(versions, components)
}

// pickVersions returns the named components' versions, or an error naming those
// that are absent and those that are present.
func pickVersions(versions map[string]string, components []string) (map[string]string, error) {
	picked := make(map[string]string, len(components))
	var missing []string
	for _, component := range components {
		if version, ok := versions[component]; ok {
			picked[component] = version
		} else {
			missing = append(missing, component)
		}
	}
	if len(missing) == 0 {
		return picked, nil
	}

	known := make([]string, 0, len(versions))
	for name := range versions {
		known = append(known, name)
	}
	sort.Strings(known)
	if len(known) == 0 {
		return nil, fmt.Errorf("-e2e.versions names no components, and %s required", strings.Join(missing, ", "))
	}
	return nil, fmt.Errorf("-e2e.versions does not name %s; it names %s", strings.Join(missing, ", "), strings.Join(known, ", "))
}

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

// optionalVersion reads one component's version, returning "" when the run did
// not name it. Only an artifact lookup that tries a more specific key first uses
// it, since a version the run qualifies is required.
func optionalVersion(component string) string {
	versions, err := runVersions()
	if err != nil {
		return ""
	}
	return versions[component]
}

// Specs reach NVKE over a port-forward they share and establish lazily; this
// closes it. A no-op when no spec ever talked to NVKE.
var _ = AfterSuite(func() {
	kube.CloseSharedForwarders()
})

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	// controller-runtime discards its own logs until a logger is set, and says
	// so once with a stack trace the first time a client is built. Point it at
	// Ginkgo's so its diagnostics land in the run log with everything else:
	// the tenant client talks to a machine through a relay, and its retries
	// and REST-layer warnings are what a slow or flaky run needs explained.
	ctrllog.SetLogger(GinkgoLogr)
	// client-go logs through klog and the port-forward through slog. Both
	// write to stderr by default, which a serial run does not capture per
	// spec, so their warnings would reach neither a spec's report nor the
	// JUnit file.
	klog.SetLogger(GinkgoLogr)
	slog.SetDefault(slog.New(logr.ToSlogHandler(GinkgoLogr)))

	suiteConfig, reporterConfig := GinkgoConfiguration()
	if !labelFilterGiven() {
		filter, err := validationLabelFilter(os.LookupEnv)
		if err != nil {
			t.Fatal(err)
		}
		if filter != "" {
			suiteConfig.LabelFilter = filter
		}
	}
	RunSpecs(t, "NVKE E2E Suite", suiteConfig, reporterConfig)
}
