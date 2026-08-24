// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

// errOSProfileMetadataUnavailable is returned when fleet OS profile metadata
// cannot be located (sibling nke-site-deploy / CWE_PROFILES_DIR / PROFILES_DIR).
var errOSProfileMetadataUnavailable = errors.New("OS profile fleet metadata unavailable")

func isOSProfileMetadataUnavailable(err error) bool {
	return errors.Is(err, errOSProfileMetadataUnavailable)
}

type osProfileMetadata struct {
	KubernetesVersion         string `json:"kubernetes_version"`
	KubernetesVersionInjected bool   `json:"kubernetes_version_injected"`
	Architecture              string `json:"architecture"`
}

type osProfileMetadataIndex struct {
	Profiles map[string]osProfileMetadata `json:"profiles"`
}

type osProfileExclusion struct {
	reason string
}

const legacyAutoinstallExclusionReason = "profile boots vmlinuz, initrd, and ISO directly; nico-vms currently requires a qcow-imager chain with image_url"

// nicoVMSOSProfileExclusions classifies active profiles that the local
// x86_64 nico-vms backend cannot use. New active profiles are otherwise
// required by the matrix, so an unsupported profile must be added here with
// an explicit reason before the suite will register.
var nicoVMSOSProfileExclusions = map[string]osProfileExclusion{
	"nke-1.29.7-251119": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-1.31.12-251029-osmo": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-1.34.2-251201-amd64-ytl": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-1.34.2-251205-amd64-ytl": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-1.34.2-260616-amd64-ytl": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-260313-22.04-amd64": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-260318-22.04-amd64": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-260319-22.04-amd64-osmo": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-260511-22.04-amd64": {
		reason: legacyAutoinstallExclusionReason,
	},
	"nke-1.31.9-251124-24.04-arm64": {
		reason: "nico-vms currently provisions x86_64 VMs and cannot boot arm64 images",
	},
	"nke-1.34.2-251201-arm64-ytl": {
		reason: "nico-vms currently provisions x86_64 VMs and cannot boot arm64 images",
	},
	"nke-1.34.2-260616-arm64-ytl": {
		reason: "nico-vms currently provisions x86_64 VMs and cannot boot arm64 images",
	},
	"nke-260602-24.04-arm64-fde": {
		reason: "nico-vms currently provisions x86_64 VMs and cannot boot arm64 images",
	},
	"nke-260611-24.04-arm64-fde": {
		reason: "nico-vms currently provisions x86_64 VMs and cannot boot arm64 images",
	},
	"nke-260625-24.04-arm64-fde": {
		reason: "nico-vms currently provisions x86_64 VMs and cannot boot arm64 images",
	},
	"nke-260625-24.04-arm64-fde-na-0.1.29": {
		reason: "nico-vms currently provisions x86_64 VMs and cannot boot arm64 images",
	},
	"nke-local-dev-kindest-arm64": {
		reason: "local kindest profile is an arm64 development placeholder, not a tenant OS profile",
	},
}

func mustNicoVMSOSProfileTableEntries() []ginkgo.TableEntry {
	profiles, err := nicoVMSOSProfileMatrix()
	if err != nil {
		// CAPD and other non-nico-vms suites import this package but do not ship
		// fleet profile metadata (NKX-13910 moved it to nke-site-deploy). Soft-skip
		// only that case — other matrix errors still panic so nico-vms regressions
		// stay loud.
		if isOSProfileMetadataUnavailable(err) {
			fmt.Fprintf(os.Stderr, "skipping nico-vms OS profile matrix entries: %v\n", err)
			return nil
		}
		panic(fmt.Sprintf("building nico-vms OS profile matrix: %v", err))
	}

	entries := make([]ginkgo.TableEntry, 0, len(profiles))
	for i, profile := range profiles {
		entries = append(entries, ginkgo.Entry(
			fmt.Sprintf("nico-vms OS profile %s", profile.name),
			ginkgo.Ordered,
			ginkgo.ContinueOnFailure,
			ginkgo.Label("nico-vms", "extended", "os-profiles", profile.name),
			nicoVMSOSProfileTestSpec(profile, i),
		))
	}
	return entries
}

func nicoVMSOSProfileTestSpec(profile nicoVMSOSProfileEntry, index int) testSpec {
	kubernetesVersion := defaultKubernetesVersion
	if !profile.metadata.KubernetesVersionInjected {
		kubernetesVersion = profile.metadata.KubernetesVersion
	}

	return testSpec{
		buildSpec: nicoVMSSpec,
		shape: clusterShape{
			kubernetesVersion: kubernetesVersion,
			cniPlugin:         CNIPluginCalico,
			controlPlanePools: []nke.NodePoolSpec{{
				DesiredNodeCount: 1,
				InstanceTypeID:   nicoVMSInstanceTypeID,
			}},
			workerPools: []nke.NodePoolSpec{{
				Name:             fmt.Sprintf("worker-profile-%02d", index+1),
				NodeNamePrefix:   fmt.Sprintf("wp%02d", index+1),
				DesiredNodeCount: 1,
				OSProfile:        profile.name,
				InstanceTypeID:   nicoVMSInstanceTypeID,
			}},
		},
		options: nke.Options{
			Provision: 2 * time.Hour,
			Delete:    10 * time.Minute,
			Poll:      10 * time.Second,
		},
		validate: validations{provisioningOnly: true},
	}
}

type nicoVMSOSProfileEntry struct {
	name     string
	metadata osProfileMetadata
}

func nicoVMSOSProfileMatrix() ([]nicoVMSOSProfileEntry, error) {
	index, err := loadActiveOSProfileMetadata()
	if err != nil {
		return nil, err
	}

	for name := range nicoVMSOSProfileExclusions {
		if _, ok := index.Profiles[name]; !ok {
			return nil, fmt.Errorf("exclusion %q is not an active OS profile", name)
		}
	}

	names := make([]string, 0, len(index.Profiles))
	for name := range index.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	profiles := make([]nicoVMSOSProfileEntry, 0, len(names))
	for _, name := range names {
		if _, excluded := nicoVMSOSProfileExclusions[name]; excluded {
			continue
		}

		metadata := index.Profiles[name]
		if metadata.Architecture != "amd64" {
			return nil, fmt.Errorf(
				"active profile %q has architecture %q; add a supported nico-vms entry or a documented exclusion",
				name,
				metadata.Architecture,
			)
		}
		if !metadata.KubernetesVersionInjected && metadata.KubernetesVersion == "" {
			return nil, fmt.Errorf("active profile %q has neither an embedded nor injectable Kubernetes version", name)
		}
		profiles = append(profiles, nicoVMSOSProfileEntry{name: name, metadata: metadata})
	}

	return profiles, nil
}

func loadActiveOSProfileMetadata() (osProfileMetadataIndex, error) {
	metadataPath, err := resolveOSProfileMetadataPath()
	if err != nil {
		return osProfileMetadataIndex{}, err
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return osProfileMetadataIndex{}, fmt.Errorf("reading %s: %w", metadataPath, err)
	}

	var index osProfileMetadataIndex
	if err := yaml.Unmarshal(data, &index); err != nil {
		return osProfileMetadataIndex{}, fmt.Errorf("parsing %s: %w", metadataPath, err)
	}
	return index, nil
}

// resolveOSProfileMetadataPath finds fleet OS profile metadata (NKX-13910 moved
// profiles to nke-site-deploy). Order: CWE_PROFILES_DIR, PROFILES_DIR, sibling
// nke-site-deploy checkout, in-repo mirror.
//
// The mirror is last so a real checkout always wins. It is a separate tree from
// test/fixtures/profiles, which holds synthetic entries that are not OS profiles
// and would otherwise turn into matrix specs.
func resolveOSProfileMetadataPath() (string, error) {
	candidates := make([]string, 0, 4)
	if dir := os.Getenv("CWE_PROFILES_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "osProfiles", "metadata.yaml"))
	}
	if dir := os.Getenv("PROFILES_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "osProfiles", "metadata.yaml"))
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolving matrix source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	candidates = append(candidates,
		filepath.Join(repoRoot, "..", "nke-site-deploy", "cwe-profiles", "profiles", "osProfiles", "metadata.yaml"),
		filepath.Join(repoRoot, "test", "fixtures", "fleet-profiles", "osProfiles", "metadata.yaml"),
	)

	var missing []string
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		missing = append(missing, path)
	}
	return "", fmt.Errorf("%w (set CWE_PROFILES_DIR to nke-site-deploy/cwe-profiles/profiles); tried: %s", errOSProfileMetadataUnavailable, strings.Join(missing, ", "))
}

func TestNicoVMSOSProfileMatrix(t *testing.T) {
	profiles, err := nicoVMSOSProfileMatrix()
	if err != nil {
		// Fleet metadata lives in nke-site-deploy; CI without a sibling checkout
		// cannot build the full matrix. Other matrix errors must still fail.
		if isOSProfileMetadataUnavailable(err) {
			t.Skipf("building matrix: %v", err)
		}
		t.Fatalf("building matrix: %v", err)
	}

	index, err := loadActiveOSProfileMetadata()
	if err != nil {
		if isOSProfileMetadataUnavailable(err) {
			t.Skipf("loading active metadata: %v", err)
		}
		t.Fatalf("loading active metadata: %v", err)
	}
	expectedProfiles := len(index.Profiles) - len(nicoVMSOSProfileExclusions)
	if len(profiles) != expectedProfiles {
		t.Fatalf("matrix contains %d profiles, want %d active profiles minus exclusions", len(profiles), expectedProfiles)
	}

	for name, exclusion := range nicoVMSOSProfileExclusions {
		if exclusion.reason == "" {
			t.Errorf("exclusion %s must document a reason", name)
		}
	}

	profilesByName := make(map[string]nicoVMSOSProfileEntry, len(profiles))
	for _, profile := range profiles {
		profilesByName[profile.name] = profile
	}

	fixedVersionSpec := nicoVMSOSProfileTestSpec(nicoVMSOSProfileEntry{
		name: "fixed-version-test-profile",
		metadata: osProfileMetadata{
			KubernetesVersion: "1.29.7",
		},
	}, 0)
	if fixedVersionSpec.shape.kubernetesVersion != "1.29.7" {
		t.Errorf("fixed-version profile uses Kubernetes %q, want 1.29.7", fixedVersionSpec.shape.kubernetesVersion)
	}

	productionSpec := nicoVMSOSProfileTestSpec(profilesByName["nke-260625-24.04-x86-fde"], 1)
	if productionSpec.shape.kubernetesVersion != defaultKubernetesVersion {
		t.Errorf("injected profile uses Kubernetes %q, want suite default %q", productionSpec.shape.kubernetesVersion, defaultKubernetesVersion)
	}
	if productionSpec.shape.workerPools[0].InstanceTypeID != nicoVMSInstanceTypeID {
		t.Errorf("production profile uses instance type %q, want %q", productionSpec.shape.workerPools[0].InstanceTypeID, nicoVMSInstanceTypeID)
	}
	if productionSpec.shape.controlPlanePools[0].InstanceTypeID != nicoVMSInstanceTypeID {
		t.Errorf("control plane uses instance type %q, want %q", productionSpec.shape.controlPlanePools[0].InstanceTypeID, nicoVMSInstanceTypeID)
	}
	if productionSpec.options.Provision != 2*time.Hour {
		t.Errorf("provision timeout is %s, want 2h", productionSpec.options.Provision)
	}

	nicoVMSProfile, ok := profilesByName[nicoVMSOSProfile]
	if !ok {
		t.Fatalf("matrix missing expected nico-vms profile %q", nicoVMSOSProfile)
	}
	nicoVMSProfileSpec := nicoVMSOSProfileTestSpec(nicoVMSProfile, 2)
	if nicoVMSProfileSpec.shape.workerPools[0].InstanceTypeID != nicoVMSInstanceTypeID {
		t.Errorf("nico-vms profile uses instance type %q, want %q", nicoVMSProfileSpec.shape.workerPools[0].InstanceTypeID, nicoVMSInstanceTypeID)
	}
}
