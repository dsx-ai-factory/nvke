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

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

// errOSProfileMetadataUnavailable is returned when fleet OS profile metadata
// cannot be located (sibling nke-site-deploy / NKE_CONTROLLER_RELEASES_DIR).
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
// nico-vms backend cannot use on any host. Profiles built for another
// architecture are not listed here. nico-sim takes the guest architecture from
// its host, so nicoVMSOSProfileMatrix drops those by architecture instead. New
// active profiles are otherwise required by the matrix, so an unsupported
// profile must be added here with an explicit reason before the suite will
// register.
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
	"nvke-local-dev-kindest-arm64": {
		reason: "local kindest profile is an arm64 development placeholder, not a tenant OS profile",
	},
}

func mustNicoVMSOSProfileTableEntries() []ginkgo.TableEntry {
	profiles, err := nicoVMSOSProfileMatrix()
	if err != nil {
		// CAPD and other non-nico-vms suites import this package but do not ship
		// fleet profile metadata (moved to nke-site-deploy). Soft-skip
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
			// The profile under test is the worker's. The control plane takes the
			// nico-vms default instance, which leaves room for the controller's kubelet
			// reservation (see nicoVMSSpec).
			controlPlanePools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}},
			workerPools: []nvke.NodePoolSpec{{
				Name:             fmt.Sprintf("worker-profile-%02d", index+1),
				NodeNamePrefix:   fmt.Sprintf("wp%02d", index+1),
				DesiredNodeCount: 1,
				OSProfile:        profile.name,
				InstanceTypeID:   nicoVMSInstanceTypeID,
			}},
		},
		options: nvke.Options{
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
		if metadata.Architecture != "amd64" && metadata.Architecture != "arm64" {
			return nil, fmt.Errorf(
				"active profile %q has architecture %q; add a supported nico-vms entry or a documented exclusion",
				name,
				metadata.Architecture,
			)
		}
		if metadata.Architecture != runtime.GOARCH {
			continue
		}
		if !metadata.KubernetesVersionInjected && metadata.KubernetesVersion == "" {
			return nil, fmt.Errorf("active profile %q has neither an embedded nor injectable Kubernetes version", name)
		}
		profiles = append(profiles, nicoVMSOSProfileEntry{name: name, metadata: metadata})
	}

	return profiles, nil
}

// otherArchProfiles counts active profiles this host cannot boot because they
// are built for another architecture, ignoring any the exclusion map already
// covers so the two never count the same profile twice.
func otherArchProfiles(index osProfileMetadataIndex) int {
	count := 0
	for name, metadata := range index.Profiles {
		if _, excluded := nicoVMSOSProfileExclusions[name]; excluded {
			continue
		}
		if metadata.Architecture != runtime.GOARCH {
			count++
		}
	}
	return count
}

const (
	annotationKubernetesVersion         = "internal.nke.nvidia.com/machine-init-kubernetes-version"
	annotationKubernetesVersionInjected = "internal.nke.nvidia.com/machine-init-kubernetes-version-injected"
	kindMachineImage                    = "MachineImage"
	kindNodePoolRelease                 = "NodePoolRelease"
)

// releaseStub is the part of a MachineImage or NodePoolRelease manifest the
// matrix reads.
type releaseStub struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Architecture    string `json:"architecture"`
		MachineImageRef struct {
			Name string `json:"name"`
		} `json:"machineImageRef"`
		Lifecycle struct {
			Deprecated bool `json:"deprecated"`
		} `json:"lifecycle"`
	} `json:"spec"`
}

// loadActiveOSProfileMetadata indexes the releases by the name of each
// active (not deprecated) NodePoolRelease, taking the Kubernetes version from
// its annotations and the architecture from the MachineImage it references.
func loadActiveOSProfileMetadata() (osProfileMetadataIndex, error) {
	dir, err := resolveReleasesDir()
	if err != nil {
		return osProfileMetadataIndex{}, err
	}
	stubs, err := readReleaseStubs(dir)
	if err != nil {
		return osProfileMetadataIndex{}, err
	}

	architectures := map[string]string{}
	for _, stub := range stubs {
		if stub.Kind == kindMachineImage {
			architectures[stub.Metadata.Name] = stub.Spec.Architecture
		}
	}

	index := osProfileMetadataIndex{Profiles: map[string]osProfileMetadata{}}
	for _, stub := range stubs {
		if stub.Kind != kindNodePoolRelease || stub.Spec.Lifecycle.Deprecated {
			continue
		}
		image := stub.Spec.MachineImageRef.Name
		architecture, ok := architectures[image]
		if !ok {
			return osProfileMetadataIndex{}, fmt.Errorf("NodePoolRelease %q references MachineImage %q, which is not in %s", stub.Metadata.Name, image, dir)
		}
		index.Profiles[stub.Metadata.Name] = osProfileMetadata{
			KubernetesVersion:         stub.Metadata.Annotations[annotationKubernetesVersion],
			KubernetesVersionInjected: stub.Metadata.Annotations[annotationKubernetesVersionInjected] == "true",
			Architecture:              architecture,
		}
	}
	return index, nil
}

// readReleaseStubs reads every manifest under dir. A file may hold several
// YAML documents.
func readReleaseStubs(dir string) ([]releaseStub, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	sort.Strings(paths)

	var stubs []releaseStub
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		for _, doc := range strings.Split("\n"+string(data), "\n---") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			var stub releaseStub
			if err := yaml.Unmarshal([]byte(doc), &stub); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", path, err)
			}
			if stub.Kind != "" {
				stubs = append(stubs, stub)
			}
		}
	}
	return stubs, nil
}

// The mirror is last so a real checkout always wins. It is a separate tree from
// test/fixtures/releases, which holds synthetic releases that are not fleet OS
// profiles and would otherwise turn into matrix specs.
func resolveReleasesDir() (string, error) {
	candidates := make([]string, 0, 3)
	if dir := os.Getenv("NKE_CONTROLLER_RELEASES_DIR"); dir != "" {
		candidates = append(candidates, dir)
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolving matrix source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	candidates = append(candidates,
		filepath.Join(repoRoot, "..", "nke-site-deploy", "cwe-profiles", "releases"),
		filepath.Join(repoRoot, "test", "fixtures", "fleet-releases"),
	)

	var missing []string
	for _, dir := range candidates {
		if matches, err := filepath.Glob(filepath.Join(dir, "*.yaml")); err == nil && len(matches) > 0 {
			return dir, nil
		}
		missing = append(missing, dir)
	}
	return "", fmt.Errorf("%w (set NKE_CONTROLLER_RELEASES_DIR to nke-site-deploy/cwe-profiles/releases); tried: %s", errOSProfileMetadataUnavailable, strings.Join(missing, ", "))
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
	expectedProfiles := len(index.Profiles) - len(nicoVMSOSProfileExclusions) - otherArchProfiles(index)
	if len(profiles) != expectedProfiles {
		t.Fatalf("matrix contains %d profiles, want %d active %s profiles minus exclusions", len(profiles), expectedProfiles, runtime.GOARCH)
	}
	for _, profile := range profiles {
		if profile.metadata.Architecture != runtime.GOARCH {
			t.Errorf("matrix has %s profile %s; nico-vms boots %s guests here", profile.metadata.Architecture, profile.name, runtime.GOARCH)
		}
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

	// Which profiles the matrix holds depends on the host architecture, so take
	// an injected-version profile from it rather than naming one. profiles is
	// sorted by name, so the choice is the same every run.
	var injected nicoVMSOSProfileEntry
	for _, profile := range profiles {
		if profile.metadata.KubernetesVersionInjected {
			injected = profile
			break
		}
	}
	if injected.name == "" {
		t.Fatal("matrix has no profile with an injected Kubernetes version")
	}
	productionSpec := nicoVMSOSProfileTestSpec(injected, 1)
	if productionSpec.shape.kubernetesVersion != defaultKubernetesVersion {
		t.Errorf("injected profile uses Kubernetes %q, want suite default %q", productionSpec.shape.kubernetesVersion, defaultKubernetesVersion)
	}
	if productionSpec.shape.workerPools[0].InstanceTypeID != nicoVMSInstanceTypeID {
		t.Errorf("production profile uses instance type %q, want %q", productionSpec.shape.workerPools[0].InstanceTypeID, nicoVMSInstanceTypeID)
	}
	built := nicoVMSSpec(config.Config{}, "matrix", productionSpec.shape)
	if got := builtControlPlaneInstanceType(built); got != nicoVMSLargeInstanceID {
		t.Errorf("control plane uses instance type %q, want %q", got, nicoVMSLargeInstanceID)
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
