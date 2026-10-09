// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/environment"
)

// checkedInDir is where the suite's environments live, relative to this
// package. Tests reach them directly so a malformed file fails here rather
// than an hour into a run on hardware.
const checkedInDir = "../../../environments"

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

			forge, err := Decode(env.Infrastructure)
			if err != nil {
				t.Fatalf("decoding %s as forge: %v", path, err)
			}
			for siteName := range forge.Sites {
				// Site validates every identifier a cluster cannot be created
				// without, so this is what stops a half-filled environment
				// reaching a run on hardware.
				site, err := forge.Site(siteName)
				if err != nil {
					t.Errorf("site %q is not usable: %v", siteName, err)
					continue
				}
				for _, instanceType := range site.InstanceTypes {
					if instanceType.ID == "" || instanceType.Name == "" {
						t.Errorf("site %q declares an instance type with no name or id: %+v", siteName, instanceType)
					}
					// Host tools are downloaded per architecture.
					if instanceType.Provides[infra.CapabilityArch] == "" {
						t.Errorf("site %q instance type %q declares no arch", siteName, instanceType.Name)
					}
				}
			}
		})
	}
}

// envReference matches the ${NAME} and ${NAME:-default} forms an environment
// file expands. A copy of the environment package's own pattern rather than a
// reason to export it: this is a test fixture, not something a caller needs.
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

func TestDecodeRejectsAnEmptyDeclaration(t *testing.T) {
	_, err := Decode(json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("decoding an environment that declares nothing should have failed")
	}
	if !strings.Contains(err.Error(), "org id") {
		t.Errorf("error should name what is missing, got %v", err)
	}
}

func TestMatchInstanceTypeTakesFirstSatisfyingType(t *testing.T) {
	site := Site{
		Key: "test",
		InstanceTypes: []InstanceType{
			{Name: "cheap-cpu", ID: "1", Provides: map[string]string{infra.CapabilityMachineType: "cpu", infra.CapabilityArch: "amd64"}},
			{Name: "gpu", ID: "2", Provides: map[string]string{infra.CapabilityMachineType: "gpu", infra.CapabilityArch: "amd64", infra.CapabilityGPU: "l40s"}},
			{Name: "second-gpu", ID: "3", Provides: map[string]string{infra.CapabilityMachineType: "gpu", infra.CapabilityArch: "amd64", infra.CapabilityGPU: "l40s"}},
		},
	}

	for _, tc := range []struct {
		name         string
		capabilities map[string]string
		want         string
	}{
		{"cheapest first", map[string]string{infra.CapabilityMachineType: "gpu"}, "gpu"},
		{"cpu request", map[string]string{infra.CapabilityMachineType: "cpu"}, "cheap-cpu"},
		{"gpu model narrows", map[string]string{infra.CapabilityGPU: "l40s"}, "gpu"},
		// kube names a software version, so it must not exclude every machine.
		{"software keys ignored", map[string]string{infra.CapabilityMachineType: "cpu", "kube": "1.37"}, "cheap-cpu"},
		{"no capabilities takes the first", map[string]string{}, "cheap-cpu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := site.MatchInstanceType(tc.capabilities)
			if err != nil {
				t.Fatalf("matching %v: %v", tc.capabilities, err)
			}
			if got.Name != tc.want {
				t.Errorf("matched %q, want %q", got.Name, tc.want)
			}
		})
	}
}

// An arm64 CPU request must not be answered by an amd64 machine, and a CPU
// request must not be answered by a GPU machine that otherwise matches. There
// is no fallback: the site genuinely cannot host it.

func TestMatchInstanceTypeDoesNotFallBack(t *testing.T) {
	site := Site{
		Key: "test",
		InstanceTypes: []InstanceType{
			{Name: "gpu-arm", ID: "1", Provides: map[string]string{infra.CapabilityMachineType: "gpu", infra.CapabilityArch: "arm64", infra.CapabilityGPU: "gb200"}},
		},
	}

	for _, capabilities := range []map[string]string{
		{infra.CapabilityMachineType: "cpu", infra.CapabilityArch: "arm64"},
		{infra.CapabilityMachineType: "gpu", infra.CapabilityArch: "amd64"},
		{infra.CapabilityGPU: "l40s"},
	} {
		if got, err := site.MatchInstanceType(capabilities); err == nil {
			t.Errorf("%v should not have matched, got %q", capabilities, got.Name)
		}
	}
}

func TestSiteDefaultsToTheOnlyOne(t *testing.T) {
	complete := Site{
		ID: "site", VpcID: "vpc", SubnetID: "subnet", SSHKeyGroupID: "keys",
		InstanceTypes: []InstanceType{{Name: "cpu", ID: "1"}},
	}

	one := &Environment{Sites: map[string]Site{"only": complete}}
	site, err := one.Site("")
	if err != nil {
		t.Fatalf("resolving the only site: %v", err)
	}
	if site.Key != "only" {
		t.Errorf("resolved site %q, want %q", site.Key, "only")
	}

	two := &Environment{Sites: map[string]Site{"a": complete, "b": complete}}
	if _, err := two.Site(""); err == nil {
		t.Error("an unnamed site should be ambiguous when two are declared")
	}
	if _, err := two.Site("c"); err == nil {
		t.Error("an unknown site should fail")
	}
}

func TestSiteReportsEveryMissingField(t *testing.T) {
	_, err := (&Environment{Sites: map[string]Site{"bare": {}}}).Site("bare")
	if err == nil {
		t.Fatal("an empty site should not validate")
	}
	for _, field := range []string{"id", "vpcID", "subnetID", "sshKeyGroupID", "instanceTypes"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error should name %q, got %v", field, err)
		}
	}
}

func TestResolvePicksTheSiteByCapability(t *testing.T) {
	az61 := Site{
		ID: "a", VpcID: "v", SubnetID: "s", SSHKeyGroupID: "k",
		InstanceTypes: []InstanceType{
			{Name: "cpu.himem.t1", ID: "1", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
			{Name: "gpu.l40sx8", ID: "2", Provides: map[string]string{"machineType": "gpu", "arch": "amd64", "gpu": "l40s"}},
		},
	}
	ytl := Site{
		ID: "b", VpcID: "v", VpcPrefixID: "p", SSHKeyGroupID: "k",
		InstanceTypes: []InstanceType{
			{Name: "cpu.x86_64", ID: "2", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
			{Name: "gpu.gb200", ID: "3", Provides: map[string]string{"machineType": "gpu", "arch": "arm64", "gpu": "gb200"}},
		},
	}
	env := &Environment{Sites: map[string]Site{"az61": az61, "ytl": ytl}}

	// The capability picks the site. Keying on a name instead would make this
	// ambiguous the moment a second site is declared.
	for _, tc := range []struct{ name, gpu, wantSite, wantType string }{
		{"l40s is only at az61", "l40s", "az61", "gpu.l40sx8"},
		{"gb200 is only at ytl", "gb200", "ytl", "gpu.gb200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selection, err := env.Resolve(map[string]string{"gpu": tc.gpu})
			if err != nil {
				t.Fatalf("resolving: %v", err)
			}
			if selection.Site.Key != tc.wantSite {
				t.Errorf("site = %q, want %q", selection.Site.Key, tc.wantSite)
			}
			if selection.GPU.Name != tc.wantType {
				t.Errorf("gpu shape = %q, want %q", selection.GPU.Name, tc.wantType)
			}
			// `arch` never reaches the GPU match: the model decides it, and
			// the CPU shape carries the requested one independently.
			if selection.CPU.Arch() != "amd64" {
				t.Errorf("cpu arch = %q, want the requested amd64", selection.CPU.Arch())
			}
		})
	}

	// A capability no site has names every site and what each offers, because
	// the usual cause is missing capacity rather than a typo.
	_, err := env.Resolve(map[string]string{"gpu": "h100"})
	if err == nil {
		t.Fatal("a capability no site offers should fail")
	}
	for _, want := range []string{"az61", "ytl", "gpu.gb200"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestSiteRequiresOneOfSubnetOrPrefix(t *testing.T) {
	base := Site{
		ID: "a", VpcID: "v", SSHKeyGroupID: "k",
		InstanceTypes: []InstanceType{{Name: "cpu", ID: "1"}},
	}

	neither := base
	if _, err := (&Environment{Sites: map[string]Site{"s": neither}}).Site("s"); err == nil {
		t.Error("a site with neither subnetID nor vpcPrefixID should not validate")
	}

	both := base
	both.SubnetID, both.VpcPrefixID = "s", "p"
	if _, err := (&Environment{Sites: map[string]Site{"s": both}}).Site("s"); err == nil {
		t.Error("a VPC never exposes both, so declaring both should not validate")
	}

	for _, tc := range []struct {
		name string
		set  func(*Site)
	}{
		{"subnet", func(s *Site) { s.SubnetID = "s" }},
		{"prefix", func(s *Site) { s.VpcPrefixID = "p" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := base
			tc.set(&site)
			if _, err := (&Environment{Sites: map[string]Site{"s": site}}).Site("s"); err != nil {
				t.Errorf("a site declaring only a %s should validate: %v", tc.name, err)
			}
		})
	}
}

// An un-onboarded site leaves its identifiers unset. That must not break a
// run against a site the repository does have, or declaring a second site
// becomes a coordinated change rather than an additive one.

func TestResolveSkipsIncompleteSites(t *testing.T) {
	usable := Site{
		ID: "a", VpcID: "v", SubnetID: "s", SSHKeyGroupID: "k",
		InstanceTypes: []InstanceType{
			{Name: "cpu", ID: "1", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
		},
	}
	env := &Environment{Sites: map[string]Site{"az61": usable, "ytl": {}}}

	selection, err := env.Resolve(nil)
	if err != nil {
		t.Fatalf("an incomplete site should be skipped, not fatal: %v", err)
	}
	if selection.Site.Key != "az61" {
		t.Errorf("site = %q, want az61", selection.Site.Key)
	}
	if selection.HasGPU() {
		t.Error("a run asking for no GPU should resolve none")
	}

	// When nothing matches, the skipped ones are named: "no site offers gb200"
	// reads very differently if ytl was silently dropped for being unset.
	if _, err := env.Resolve(map[string]string{"gpu": "gb200"}); err == nil {
		t.Fatal("a capability no usable site offers should fail")
	} else if !strings.Contains(err.Error(), "ytl") {
		t.Errorf("the error should name the skipped site: %v", err)
	}
}

// A GPU run must not put its control plane on GPU machines: they are scarce
// enough that the cluster fails to schedule for a reason unrelated to what the
// run was qualifying.

func TestMatchCPUTakesTheCPUShapeOfTheSameArch(t *testing.T) {
	site := Site{
		Key: "ytl",
		InstanceTypes: []InstanceType{
			{Name: "cpu.amd", ID: "a", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
			{Name: "cpu.arm", ID: "b", Provides: map[string]string{"machineType": "cpu", "arch": "arm64"}},
			{Name: "gpu.gb200", ID: "c", Provides: map[string]string{"machineType": "gpu", "arch": "arm64", "gpu": "gb200"}},
		},
	}
	got, err := site.MatchCPU("arm64")
	if err != nil {
		t.Fatalf("matching a CPU shape: %v", err)
	}
	if got.Name != "cpu.arm" {
		t.Errorf("should take the arm64 CPU shape, got %s", got.Name)
	}

	if _, err := site.MatchCPU("riscv64"); err == nil {
		t.Error("should fail when the site offers no CPU shape of that arch")
	}
}

// A site declaring two architectures where only one has an id must not answer
// a request for the other with an empty identifier.

func TestSiteRequiresAnIDOnEveryInstanceType(t *testing.T) {
	site := Site{
		Key: "ytl", ID: "s", VpcID: "v", VpcPrefixID: "p", SSHKeyGroupID: "k",
		InstanceTypes: []InstanceType{
			{Name: "cpu.arm", ID: "b", Provides: map[string]string{"machineType": "cpu", "arch": "arm64"}},
			{Name: "cpu.amd", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
		},
	}
	err := site.validate()
	if err == nil {
		t.Fatal("a shape with no id should invalidate the site")
	}
	if !strings.Contains(err.Error(), "instanceTypes[cpu.amd].id") {
		t.Errorf("error should name the shape, got %v", err)
	}
}

// A GB200 run is the mixed case: arm64 workers behind an amd64 control plane.
// `arch` names the CPU side only, and the GPU shape's architecture is read off
// the shape rather than asked for, because the model already decided it.

func TestResolveMixesArchitectures(t *testing.T) {
	ytl := Site{
		ID: "b", VpcID: "v", VpcPrefixID: "p", SSHKeyGroupID: "k",
		InstanceTypes: []InstanceType{
			{Name: "cpu.x86_64", ID: "1", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
			{Name: "cpu.arm", ID: "2", Provides: map[string]string{"machineType": "cpu", "arch": "arm64"}},
			{Name: "gpu.gb200", ID: "3", Provides: map[string]string{"machineType": "gpu", "arch": "arm64", "gpu": "gb200"}},
		},
	}
	env := &Environment{Sites: map[string]Site{"ytl": ytl}}

	selection, err := env.Resolve(map[string]string{"gpu": "gb200", "arch": "amd64"})
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if got := selection.CPU.Name; got != "cpu.x86_64" {
		t.Errorf("cpu shape = %q, want the amd64 one that arch asked for", got)
	}
	if got := selection.GPU.Name; got != "gpu.gb200" {
		t.Errorf("gpu shape = %q, want gpu.gb200", got)
	}
	if got := selection.WorkerType().Arch(); got != "arm64" {
		t.Errorf("worker arch = %q, want the arm64 the GB200 comes on", got)
	}

	// arch=arm64 picks the other CPU shape and leaves the GPU one alone.
	selection, err = env.Resolve(map[string]string{"gpu": "gb200", "arch": "arm64"})
	if err != nil {
		t.Fatalf("resolving arm64: %v", err)
	}
	if got := selection.CPU.Name; got != "cpu.arm" {
		t.Errorf("cpu shape = %q, want cpu.arm", got)
	}

	// With no GPU asked for, the workers are the CPU shape.
	selection, err = env.Resolve(map[string]string{"arch": "amd64"})
	if err != nil {
		t.Fatalf("resolving cpu-only: %v", err)
	}
	if selection.HasGPU() {
		t.Error("no gpu was asked for, so none should resolve")
	}
	if got := selection.WorkerType().Name; got != "cpu.x86_64" {
		t.Errorf("worker shape = %q, want the CPU one", got)
	}
}
