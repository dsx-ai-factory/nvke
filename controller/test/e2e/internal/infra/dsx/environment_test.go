// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dsx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/environment"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

// checkedInDir is where the suite's environments live, relative to this
// package. Tests reach them directly so a malformed file fails here rather
// than an hour into a run on hardware.
const checkedInDir = "../../../environments"

var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

func setSynthetic(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, match := range envReference.FindAllSubmatch(data, -1) {
		name := string(match[1])
		t.Setenv(name, "synthetic-"+strings.ToLower(name))
	}
}

func TestCheckedInEnvironmentsParse(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(checkedInDir, "*.yaml"))
	if err != nil {
		t.Fatalf("globbing %s: %v", checkedInDir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no environments found in %s", checkedInDir)
	}

	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		t.Run(name, func(t *testing.T) {
			setSynthetic(t, path)

			env, err := environment.LoadPath(name, path)
			if err != nil {
				t.Fatalf("loading %s: %v", path, err)
			}
			if env.Provider != ProviderName {
				t.Skipf("provider %q has no decoder to check here", env.Provider)
			}

			decoded, err := Decode(env.Infrastructure)
			if err != nil {
				t.Fatalf("decoding %s as dsx: %v", path, err)
			}
			seen := map[string]string{}
			for siteName := range decoded.Sites {
				// Site validates every identifier a cluster cannot be created
				// without, so this is what stops a half-filled environment
				// reaching a run on hardware.
				site, err := decoded.Site(siteName)
				if err != nil {
					t.Errorf("site %q is not usable: %v", siteName, err)
					continue
				}
				// The key is generic, so nothing ties it to a host and the
				// check that a site addresses itself cannot be made here. What
				// can: two sites sharing one endpoint, which is a copy-paste of
				// the wrong variable and would silently run both streams
				// against one datahall.
				if other, dup := seen[site.Endpoint()]; dup {
					t.Errorf("sites %q and %q both resolve to %q", other, siteName, site.Endpoint())
				}
				seen[site.Endpoint()] = siteName
				for _, machine := range site.Machines {
					if machine.Arch() == "" {
						t.Errorf("site %q machine %q declares no arch, so nothing can select it", siteName, machine.Name)
					}
				}
				for _, instanceType := range site.InstanceTypes {
					if len(instanceType.Provides) == 0 {
						t.Errorf("site %q instance type %q provides nothing, so nothing can select it", siteName, instanceType.Name)
					}
				}
			}
		})
	}
}

func TestDecodeRejectsAnEmptyDeclaration(t *testing.T) {
	_, err := Decode(json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("decoding an environment that declares nothing should have failed")
	}
	if !strings.Contains(err.Error(), "org id") {
		t.Errorf("error should name what is missing, got %v", err)
	}
}

func TestDecodeRejectsAnUnknownField(t *testing.T) {
	// A misspelled optional key is the case this guards: without strictness it
	// would drop a jump-host pin or a node user without a word.
	_, err := Decode(json.RawMessage(`{"org":{"id":"ncx"},"sites":{"a":{"subnetID":"x"}}}`))
	if err == nil {
		t.Fatal("decoding an environment with an unknown field should have failed")
	}
	if !strings.Contains(err.Error(), "subnetID") {
		t.Errorf("error should name the unknown field, got %v", err)
	}
}

// completeSite is a datahall with nothing missing, for tests that vary one
// thing about it.
func completeSite() Site {
	return Site{
		API:           "https://carbide-rest-api-x.example",
		ID:            "site",
		TenantID:      "tenant",
		VpcID:         "vpc",
		VpcPrefixID:   "prefix",
		SSHKeyGroupID: "keygroup",
		Machines: []Machine{
			{Name: "J701L43T", ID: "m1", Provides: map[string]string{infra.CapabilityArch: "amd64"}},
		},
		InstanceTypes: []InstanceType{
			{Name: "tenant-cpu", ID: "t1", Provides: map[string]string{infra.CapabilityMachineType: "cpu", infra.CapabilityArch: "amd64"}},
		},
	}
}

// The key is generic, so nothing is derived from it: the host is declared.
// A trailing slash is trimmed because the callers append a rooted path.
func TestEndpointIsTheDeclaredAPI(t *testing.T) {
	for name, tc := range map[string]struct{ api, want string }{
		"as declared":       {api: "https://carbide-rest-api-x.example", want: "https://carbide-rest-api-x.example"},
		"trailing slash":    {api: "https://elsewhere.example/", want: "https://elsewhere.example"},
		"surrounding space": {api: "  https://elsewhere.example  ", want: "https://elsewhere.example"},
		// A run of slashes, not one: trimming a single suffix would leave the
		// endpoint slash-terminated and read as a second datahall.
		"repeated slashes": {api: "https://elsewhere.example///", want: "https://elsewhere.example"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := (Site{Key: "datahall-1", API: tc.api}).Endpoint(); got != tc.want {
				t.Errorf("Endpoint() = %q, want %q", got, tc.want)
			}
		})
	}
}

// api is required: the key names no host, so an unset one would dial nothing.
func TestValidateRequiresTheAPI(t *testing.T) {
	site := completeSite()
	site.Key = "dh"
	site.API = ""
	if err := site.validate(); err == nil || !strings.Contains(err.Error(), "api") {
		t.Errorf("validate() = %v, want it to name the missing api", err)
	}
}

func TestValidateNamesEveryMissingField(t *testing.T) {
	err := Site{Key: "bare"}.validate()
	if err == nil {
		t.Fatal("a site declaring nothing should not validate")
	}
	for _, want := range []string{"api", "tenantID", "vpcID", "vpcPrefixID", "sshKeyGroupID", "instanceTypes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got %v", want, err)
		}
	}
}

// `id` is declared and recorded but nothing sends it: the API host comes from
// the key. Requiring it would skip an otherwise usable datahall over a
// variable no request carries, which is how an onboarding fails for a reason
// that does not exist.
func TestValidateDoesNotRequireTheSiteID(t *testing.T) {
	site := completeSite()
	site.Key = "dh"
	site.ID = ""

	if err := site.validate(); err != nil {
		t.Errorf("a site with every sent field but no id should validate, got %v", err)
	}
}

func TestValidateRequiresInstanceTypesAndNotMachines(t *testing.T) {
	// Instance types place the tenant nodes, which nothing else can, and are
	// also where the site controller goes unless a machine is pinned. Machines
	// are the optional half: a file is expected to omit them, so that the
	// datahall picks a host that is free and healthy.
	t.Run("no instanceTypes", func(t *testing.T) {
		site := completeSite()
		site.Key = "dh"
		site.InstanceTypes = nil
		if err := site.validate(); err == nil {
			t.Error("a site with no instanceTypes should not validate")
		}
	})
	t.Run("no machines", func(t *testing.T) {
		site := completeSite()
		site.Key = "dh"
		site.Machines = nil
		if err := site.validate(); err != nil {
			t.Errorf("a site with no machines should validate, got %v", err)
		}
	})
}

// A datahall may declare a GPU type, and nothing orders the file: a type
// providing {machineType: gpu, arch: amd64} can sit above the CPU one and
// still answer an amd64 request. Matching on architecture alone put the site
// controller on it -- accelerator hardware the GB200 streams exist to
// exercise, taken by a run that asked for a plain machine. Resolve already
// selected the site on its CPU shape, so Place asking for less also placed the
// run somewhere Resolve had not agreed to.
func TestPlaceSkipsAGPUTypeOfTheSameArch(t *testing.T) {
	site := completeSite()
	site.Key = "dh"
	site.Machines = nil
	// First, so a match on arch alone returns it.
	site.InstanceTypes = append([]InstanceType{
		{Name: "tenant-gpu", ID: "g1", Provides: map[string]string{infra.CapabilityMachineType: "gpu", infra.CapabilityArch: "amd64", infra.CapabilityGPU: "gb200"}},
	}, site.InstanceTypes...)

	place, err := site.Place("amd64")
	if err != nil {
		t.Fatalf("placing with a GPU type declared first: %v", err)
	}
	if place.InstanceTypeID != "t1" {
		t.Errorf("placed on %q (%s), want the CPU type t1 -- a site controller must not take accelerator hardware", place.InstanceTypeID, place.Name)
	}
}

// Place is the whole point of making machines optional: with none declared the
// create carries an instanceTypeId and the datahall chooses the host, which is
// what Forge does and what a lane needs. A pinned machine still wins, so a
// hand bring-up can still name one.
func TestPlacePrefersAPinnedMachineAndFallsBackToAnInstanceType(t *testing.T) {
	site := completeSite()
	site.Key = "dh"

	pinned, err := site.Place("amd64")
	if err != nil {
		t.Fatalf("placing with machines declared: %v", err)
	}
	if pinned.MachineID == "" || pinned.InstanceTypeID != "" {
		t.Errorf("a site declaring machines should place on one, got %+v", pinned)
	}

	site.Machines = nil
	pooled, err := site.Place("amd64")
	if err != nil {
		t.Fatalf("placing with no machines: %v", err)
	}
	if pooled.InstanceTypeID == "" || pooled.MachineID != "" {
		t.Errorf("a site declaring no machines should place on an instance type, got %+v", pooled)
	}
	if pooled.Arch != "amd64" {
		t.Errorf("placement arch = %q, want amd64 -- it chooses the node images", pooled.Arch)
	}
}

func TestMatchMachineTakesTheFirstOfTheArchitecture(t *testing.T) {
	site := Site{
		Key: "dh",
		Machines: []Machine{
			{Name: "arm", ID: "1", Provides: map[string]string{infra.CapabilityArch: "arm64"}},
			{Name: "first-amd", ID: "2", Provides: map[string]string{infra.CapabilityArch: "amd64"}},
			{Name: "second-amd", ID: "3", Provides: map[string]string{infra.CapabilityArch: "amd64"}},
		},
	}
	machine, err := site.MatchMachine("amd64")
	if err != nil {
		t.Fatal(err)
	}
	if machine.Name != "first-amd" {
		t.Errorf("matched %q, want the first declared amd64 machine", machine.Name)
	}
}

func TestMatchMachineNamesWhatItOffers(t *testing.T) {
	site := Site{
		Key:      "dh",
		Machines: []Machine{{Name: "J701L43T", ID: "1", Provides: map[string]string{infra.CapabilityArch: "amd64"}}},
	}
	_, err := site.MatchMachine("arm64")
	if err == nil {
		t.Fatal("a datahall with no arm64 machine should not match one")
	}
	// The serial, because that is what a capacity conversation is held in.
	if !strings.Contains(err.Error(), "J701L43T") {
		t.Errorf("error should name the machines offered, got %v", err)
	}
}

func TestResolveNeedsATenantShapeOfTheArchitecture(t *testing.T) {
	withMachineOnly := completeSite()
	withMachineOnly.InstanceTypes = []InstanceType{
		{Name: "arm-only", ID: "t1", Provides: map[string]string{infra.CapabilityMachineType: "cpu", infra.CapabilityArch: "arm64"}},
	}
	env := &Environment{Org: Org{ID: "ncx"}, Sites: map[string]Site{"dh": withMachineOnly}}

	if _, err := env.Resolve(map[string]string{infra.CapabilityArch: "amd64"}); err == nil {
		t.Fatal("a datahall with an amd64 machine but no amd64 tenant shape should not resolve")
	}
}

func TestResolveSkipsAnIncompleteSite(t *testing.T) {
	// The onboarding case: a datahall declared but with its identifiers unset
	// is skipped, so declaring a second one stays an additive change.
	incomplete := completeSite()
	incomplete.VpcID = ""
	env := &Environment{Org: Org{ID: "ncx"}, Sites: map[string]Site{
		"aaa-incomplete": incomplete,
		"zzz-complete":   completeSite(),
	}}

	selection, err := env.Resolve(map[string]string{infra.CapabilityArch: "amd64"})
	if err != nil {
		t.Fatalf("a complete site should resolve even beside an incomplete one: %v", err)
	}
	if selection.Site.Key != "zzz-complete" {
		t.Errorf("resolved to %q, want the complete site", selection.Site.Key)
	}
}

func TestResolveReportsWhatEverySiteOffers(t *testing.T) {
	env := &Environment{Org: Org{ID: "ncx"}, Sites: map[string]Site{"dh": completeSite()}}
	_, err := env.Resolve(map[string]string{infra.CapabilityArch: "amd64", infra.CapabilityGPU: "gb200"})
	if err == nil {
		t.Fatal("a datahall offering no gb200 should not resolve one")
	}
	for _, want := range []string{"dh", "gb200", "tenant-cpu"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got %v", want, err)
		}
	}
}

func TestResolveDefaultsTheArchitecture(t *testing.T) {
	env := &Environment{Org: Org{ID: "ncx"}, Sites: map[string]Site{"dh": completeSite()}}
	selection, err := env.Resolve(nil)
	if err != nil {
		t.Fatalf("a run naming no architecture should take the default: %v", err)
	}
	if selection.Machine.Arch() != infra.DefaultArch {
		t.Errorf("machine arch = %q, want the default %q", selection.Machine.Arch(), infra.DefaultArch)
	}
}

func TestWorkerTypeFallsBackToTheCPUShape(t *testing.T) {
	selection := Selection{CPU: InstanceType{Name: "cpu"}}
	if selection.HasGPU() {
		t.Error("a selection with no GPU shape should not report one")
	}
	if selection.WorkerType().Name != "cpu" {
		t.Errorf("WorkerType() = %q, want the CPU shape when no GPU was asked for", selection.WorkerType().Name)
	}
}

// The regression this guards: machines became optional so the datahall could
// choose a free, healthy host, and Resolve still demanded one. The suite then
// failed in BeforeAll with "no declared site offers a amd64 machine", before
// any provider code ran, on a file that was correct.
func TestResolveDoesNotRequireAMachine(t *testing.T) {
	pooled := completeSite()
	pooled.Machines = nil
	env := &Environment{Org: Org{ID: "ncx"}, Sites: map[string]Site{"dh": pooled}}

	selection, err := env.Resolve(map[string]string{infra.CapabilityArch: "amd64"})
	if err != nil {
		t.Fatalf("a site declaring no machines should resolve, got %v", err)
	}
	if selection.Machine.ID != "" {
		t.Errorf("selection should carry no machine, got %+v", selection.Machine)
	}
	if selection.CPU.ID == "" {
		t.Error("selection should still carry the CPU shape the tenant nodes need")
	}

	// And it must still place: the site controller goes on the instance type.
	place, err := selection.Site.Place("amd64")
	if err != nil {
		t.Fatalf("placing on a site with no machines: %v", err)
	}
	if place.InstanceTypeID == "" || place.MachineID != "" {
		t.Errorf("should place on an instance type, got %+v", place)
	}
}
