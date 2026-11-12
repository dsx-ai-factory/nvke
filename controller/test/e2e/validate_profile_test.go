// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/authtoken"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

// validateProfileEnv names a YAML file of defaults for the validation image's
// inputs, for a site whose layout is not deploy/nico. Its keys are the input
// variables themselves, so a profile is what a site would otherwise pass as
// a list of -e flags, and every input reads the same way with or without one.
// A variable set in the environment wins over the profile.
//
// The suite ships no profiles. A site's profile describes that site, and
// lives with whatever else describes it.
const validateProfileEnv = "NVKE_VALIDATE_PROFILE"

// validateProfileSummary says what the profile set, for the report. Empty when
// no profile was given.
var validateProfileSummary string

// validateExclusionSummary names the labels the run left out, for the report.
// Empty when none were. A filtered-out spec is absent from the JUnit report,
// not skipped in it, so the report would not otherwise show it was left out.
var validateExclusionSummary string

// runNotes are the report lines that say how the run was shaped before any
// spec read its settings.
func runNotes() []string {
	var notes []string
	for _, note := range []string{validateProfileSummary, validateExclusionSummary} {
		if note != "" {
			notes = append(notes, note)
		}
	}
	return notes
}

// siteExpectationEnvs are the inputs siteExpectationsFromEnv reads, each of
// which a tenant reads too under tenantPrefix.
var siteExpectationEnvs = []string{
	siteComponentsEnv,
	siteStorageClassesEnv,
	sitePVCStorageClassEnv,
	siteWorkloadVersionsEnv,
	siteRequiredWorkloadsEnv,
	siteAPIServerFlagsEnv,
	siteEtcdEncryptedEnv,
	siteReservedMemoryEnv,
}

// profileKeys are the inputs a profile may set: what describes a site and
// the tenants built on it. Left out are the inputs that choose what a run
// does, which the image sets in its own ENV and so would always win over a
// profile, and those that name one tenant rather than describe the site.
// TestUnit_ValidateProfileCoversEveryInput lists them.
var profileKeys = slices.Concat(
	[]string{
		controller.EnvNamespace,
		controller.EnvService,
		controller.EnvPort,
		"NVKE_CONTROLLER_EXPECTED_VERSION",
		"NVKE_CONTROLLER_HEALTH_TIMEOUT",
		authtoken.EnvNamespace,
		authtoken.EnvServiceAccount,
		siteKubernetesVersionEnv,
		siteNamespacesEnv,
		siteProbeNamespaceEnv,
		siteProbeImageEnv,
		siteProbePullSecretsEnv,
		validateExcludeLabelsEnv,
	},
	siteExpectationEnvs,
	tenantTwins(siteExpectationEnvs),
	[]string{
		tenantNamespaceEnv,
		tenantNamespacesEnv,
		tenantProbeImageEnv,
		tenantProbePullSecretsEnv,
		tenantGPUNodesEnv,
		tenantGPUImageEnv,
	},
)

func tenantTwins(siteEnvs []string) []string {
	twins := make([]string, 0, len(siteEnvs))
	for _, name := range siteEnvs {
		twins = append(twins, siteExpectations{prefix: tenantPrefix}.env(name))
	}
	return twins
}

// loadValidateProfile reads a profile. A value is a scalar, taken as written so
// that 1.30 stays 1.30 rather than becoming a number, or a list of scalars,
// joined with commas. The APISERVER_FLAGS inputs separate with semicolons,
// because their values hold commas, so they take a scalar.
func loadValidateProfile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", validateProfileEnv, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: parsing %s: %w", validateProfileEnv, path, err)
	}
	values := map[string]string{}
	if len(doc.Content) == 0 {
		return values, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: %s is not a mapping of variable to value", validateProfileEnv, path)
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode, valueNode := root.Content[i], root.Content[i+1]
		key := keyNode.Value
		if !slices.Contains(profileKeys, key) {
			return nil, fmt.Errorf("%s: %s sets %s, which a profile cannot (it can set: %s)", validateProfileEnv, path, key, strings.Join(slices.Sorted(slices.Values(profileKeys)), ", "))
		}
		if _, dup := values[key]; dup {
			return nil, fmt.Errorf("%s: %s sets %s twice", validateProfileEnv, path, key)
		}
		switch valueNode.Kind {
		case yaml.ScalarNode:
			if valueNode.Tag == "!!null" {
				return nil, fmt.Errorf("%s: %s gives %s no value", validateProfileEnv, path, key)
			}
			values[key] = valueNode.Value
		case yaml.SequenceNode:
			if strings.HasSuffix(key, "_APISERVER_FLAGS") {
				return nil, fmt.Errorf("%s: %s gives %s as a list; it takes a string of name=value separated by semicolons", validateProfileEnv, path, key)
			}
			entries := make([]string, 0, len(valueNode.Content))
			for _, entry := range valueNode.Content {
				if entry.Kind != yaml.ScalarNode || entry.Tag == "!!null" {
					return nil, fmt.Errorf("%s: %s: %s holds an entry that is not a string", validateProfileEnv, path, key)
				}
				entries = append(entries, entry.Value)
			}
			values[key] = strings.Join(entries, ",")
		default:
			return nil, fmt.Errorf("%s: %s: %s is not a string or a list of strings", validateProfileEnv, path, key)
		}
	}
	return values, nil
}

// applyValidateProfile sets every profile value whose variable the environment
// leaves unset or blank, and returns the keys it set and the keys the
// environment overrode, each sorted. Blank counts as unset because every
// input reads a blank value as unset.
func applyValidateProfile(values map[string]string, getenv func(string) string, setenv func(string, string) error) (set, overridden []string, err error) {
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if strings.TrimSpace(getenv(key)) != "" {
			overridden = append(overridden, key)
			continue
		}
		if err := setenv(key, values[key]); err != nil {
			return nil, nil, fmt.Errorf("%s: setting %s: %w", validateProfileEnv, key, err)
		}
		set = append(set, key)
	}
	return set, overridden, nil
}

// useValidateProfile applies the profile validateProfileEnv names, if any, to the
// process environment. It runs before the spec tree is built, because specs
// read their inputs while building it.
func useValidateProfile() (string, error) {
	path := strings.TrimSpace(os.Getenv(validateProfileEnv))
	if path == "" {
		return "", nil
	}
	values, err := loadValidateProfile(path)
	if err != nil {
		return "", err
	}
	set, overridden, err := applyValidateProfile(values, os.Getenv, os.Setenv)
	if err != nil {
		return "", err
	}
	summary := fmt.Sprintf("%s=%s set %s", validateProfileEnv, path, strings.Join(set, ", "))
	if len(set) == 0 {
		summary = fmt.Sprintf("%s=%s set nothing", validateProfileEnv, path)
	}
	if len(overridden) > 0 {
		summary += "; the environment overrode " + strings.Join(overridden, ", ")
	}
	return summary, nil
}
