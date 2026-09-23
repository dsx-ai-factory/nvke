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

// parseAssignments reads the comma-separated key=value form both -e2e.versions
// and -e2e.capabilities take. An empty string is an empty set, not an error.
func parseAssignments(flagName, raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}

	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, found := strings.Cut(pair, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found || key == "" || value == "" {
			return nil, fmt.Errorf("%s: %q is not key=value", flagName, pair)
		}
		if _, duplicate := out[key]; duplicate {
			return nil, fmt.Errorf("%s: %q given twice", flagName, key)
		}
		out[key] = value
	}
	return out, nil
}

// The flags are parsed once and shared. Specs read them through the accessors
// below rather than re-parsing, so a malformed flag is one failure rather than
// one per entry in the table.
var (
	versionsOnce     sync.Once
	versionsValue    map[string]string
	errVersions      error
	capabilitiesOnce sync.Once
	capabilitiesVal  map[string]string
	errCapabilities  error
	forgeOnce        sync.Once
	forgeValue       *environment.Forge
	errForge         error
)

// runVersions is what this run is qualifying, keyed by component name.
func runVersions() (map[string]string, error) {
	versionsOnce.Do(func() {
		versionsValue, errVersions = parseAssignments("-e2e.versions", versionsFlag)
	})
	return versionsValue, errVersions
}

// runCapabilities is what this run needs to execute on. Infrastructure keys
// select a machine; the rest, such as kube, are software and select nothing.
func runCapabilities() (map[string]string, error) {
	capabilitiesOnce.Do(func() {
		capabilitiesVal, errCapabilities = parseAssignments("-e2e.capabilities", capabilitiesFlag)
	})
	return capabilitiesVal, errCapabilities
}

// forgeEnvironment resolves -e2e.environment as a Forge declaration. Loaded on
// first use so the simulator backends, which need no environment, keep running
// with the flag unset.
func forgeEnvironment() (*environment.Forge, error) {
	forgeOnce.Do(func() {
		env, err := environment.Load(envName)
		if err != nil {
			errForge = err
			return
		}
		forgeValue, errForge = env.Forge()
	})
	return forgeValue, errForge
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
