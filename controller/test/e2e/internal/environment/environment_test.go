// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkedInDir is where the suite's environments live, relative to this
// package. Tests reach them directly so a malformed file fails here rather
// than an hour into a run on hardware.
const checkedInDir = "../../environments"

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

			env, err := loadPath(name, path)
			if err != nil {
				t.Fatalf("loading %s: %v", path, err)
			}
			if env.Provider != ForgeProvider {
				t.Skipf("provider %q has no decoder to check here", env.Provider)
			}

			forge, err := env.Forge()
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
					if instanceType.Provides[capabilityArch] == "" {
						t.Errorf("site %q instance type %q declares no arch", siteName, instanceType.Name)
					}
				}
			}
		})
	}
}

// setSynthetic gives every ${NAME} an environment file references a value, so
// the structural checks run wherever this test does -- on a laptop, or in a
// pipeline that has no business holding one organization's identifiers. Read
// from the file rather than listed here, so adding a reference does not need a
// matching edit in the test that would be discovered by a failure.
func setSynthetic(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, match := range reference.FindAllSubmatch(data, -1) {
		name := string(match[1])
		t.Setenv(name, "synthetic-"+strings.ToLower(name))
	}
}

// An unset reference has to fail here. Expanding it to an empty string would
// leave a file that still parses, so the run would reach Forge and ask it to
// create a cluster in org "", and the failure would name Forge's field rather
// than the variable nobody set.
func TestLoadRejectsUnsetReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.yaml")
	body := "provider: forge\ninfrastructure:\n  org:\n    id: \"${FORGE_ABSENT_ORG}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	_, err := loadPath("gap", path)
	if err == nil {
		t.Fatal("loading an environment with an unset reference should have failed")
	}
	if !strings.Contains(err.Error(), "FORGE_ABSENT_ORG") {
		t.Errorf("error should name the unset variable, got: %v", err)
	}
}

// A set-but-empty reference has to fail for the same reason an absent one
// does, and it is the shape CI actually produces: a workflow that writes
// `env: FORGE_ABSENT_ORG: ${{ vars.FORGE_ABSENT_ORG }}` for a repository
// variable nobody created sets the variable to "" rather than leaving it out.
// Checking presence alone would let exactly the run this package exists to
// prevent through, and only on the runner.
func TestLoadRejectsEmptyReference(t *testing.T) {
	t.Setenv("FORGE_ABSENT_ORG", "")

	path := filepath.Join(t.TempDir(), "empty.yaml")
	body := "provider: forge\ninfrastructure:\n  org:\n    id: \"${FORGE_ABSENT_ORG}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	_, err := loadPath("empty", path)
	if err == nil {
		t.Fatal("loading an environment with an empty reference should have failed")
	}
	if !strings.Contains(err.Error(), "FORGE_ABSENT_ORG") {
		t.Errorf("error should name the empty variable, got: %v", err)
	}
}

func TestExpandOptionalReference(t *testing.T) {
	t.Setenv("FORGE_SET_OPTIONAL", "jump.example")
	for _, tc := range []struct{ in, want string }{
		{`a: "${FORGE_ABSENT_OPTIONAL:-}"`, `a: ""`},
		{`a: "${FORGE_ABSENT_OPTIONAL:-root}"`, `a: "root"`},
		{`a: "${FORGE_SET_OPTIONAL:-}"`, `a: "jump.example"`},
	} {
		got, err := expand([]byte(tc.in))
		if err != nil {
			t.Errorf("expand(%s): %v", tc.in, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("expand(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestLoadRejectsPaths(t *testing.T) {
	for _, name := range []string{"", "../secrets", "forge/../../etc", "."} {
		if _, err := Load(name); err == nil {
			t.Errorf("Load(%q) should have failed", name)
		}
	}
}

func TestForgeRejectsAnotherProvidersEnvironment(t *testing.T) {
	env := &Environment{Name: "elsewhere", Provider: "not-forge", Infrastructure: []byte(`{}`)}
	_, err := env.Forge()
	if err == nil {
		t.Fatal("decoding a non-forge environment as forge should have failed")
	}
	if !strings.Contains(err.Error(), "not-forge") {
		t.Errorf("error should name the actual provider, got %v", err)
	}
}

func TestMatchInstanceTypeTakesFirstSatisfyingType(t *testing.T) {
	site := ForgeSite{
		Key: "test",
		InstanceTypes: []ForgeInstanceType{
			{Name: "cheap-cpu", ID: "1", Provides: map[string]string{capabilityMachineType: "cpu", capabilityArch: "amd64"}},
			{Name: "gpu", ID: "2", Provides: map[string]string{capabilityMachineType: "gpu", capabilityArch: "amd64", capabilityGPU: "l40"}},
			{Name: "second-gpu", ID: "3", Provides: map[string]string{capabilityMachineType: "gpu", capabilityArch: "amd64", capabilityGPU: "l40"}},
		},
	}

	for _, tc := range []struct {
		name         string
		capabilities map[string]string
		want         string
	}{
		{"cheapest first", map[string]string{capabilityMachineType: "gpu"}, "gpu"},
		{"cpu request", map[string]string{capabilityMachineType: "cpu"}, "cheap-cpu"},
		{"gpu model narrows", map[string]string{capabilityGPU: "l40"}, "gpu"},
		// kube names a software version, so it must not exclude every machine.
		{"software keys ignored", map[string]string{capabilityMachineType: "cpu", "kube": "1.37"}, "cheap-cpu"},
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
	site := ForgeSite{
		Key: "test",
		InstanceTypes: []ForgeInstanceType{
			{Name: "gpu-arm", ID: "1", Provides: map[string]string{capabilityMachineType: "gpu", capabilityArch: "arm64", capabilityGPU: "gb200"}},
		},
	}

	for _, capabilities := range []map[string]string{
		{capabilityMachineType: "cpu", capabilityArch: "arm64"},
		{capabilityMachineType: "gpu", capabilityArch: "amd64"},
		{capabilityGPU: "l40"},
	} {
		if got, err := site.MatchInstanceType(capabilities); err == nil {
			t.Errorf("%v should not have matched, got %q", capabilities, got.Name)
		}
	}
}

func TestSiteDefaultsToTheOnlyOne(t *testing.T) {
	complete := ForgeSite{
		ID: "site", VpcID: "vpc", SubnetID: "subnet", SSHKeyGroupID: "keys",
		InstanceTypes: []ForgeInstanceType{{Name: "cpu", ID: "1"}},
	}

	one := &Forge{Sites: map[string]ForgeSite{"only": complete}}
	site, err := one.Site("")
	if err != nil {
		t.Fatalf("resolving the only site: %v", err)
	}
	if site.Key != "only" {
		t.Errorf("resolved site %q, want %q", site.Key, "only")
	}

	two := &Forge{Sites: map[string]ForgeSite{"a": complete, "b": complete}}
	if _, err := two.Site(""); err == nil {
		t.Error("an unnamed site should be ambiguous when two are declared")
	}
	if _, err := two.Site("c"); err == nil {
		t.Error("an unknown site should fail")
	}
}

func TestSiteReportsEveryMissingField(t *testing.T) {
	_, err := (&Forge{Sites: map[string]ForgeSite{"bare": {}}}).Site("bare")
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
	az61 := ForgeSite{
		ID: "a", VpcID: "v", SubnetID: "s", SSHKeyGroupID: "k",
		InstanceTypes: []ForgeInstanceType{
			{Name: "cpu.himem.t1", ID: "1", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
			{Name: "gpu.l40sx8", ID: "2", Provides: map[string]string{"machineType": "gpu", "arch": "amd64", "gpu": "l40"}},
		},
	}
	ytl := ForgeSite{
		ID: "b", VpcID: "v", VpcPrefixID: "p", SSHKeyGroupID: "k",
		InstanceTypes: []ForgeInstanceType{
			{Name: "cpu.x86_64", ID: "2", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
			{Name: "gpu.gb200", ID: "3", Provides: map[string]string{"machineType": "gpu", "arch": "arm64", "gpu": "gb200"}},
		},
	}
	env := &Forge{Sites: map[string]ForgeSite{"az61": az61, "ytl": ytl}}

	// The capability picks the site. Keying on a name instead would make this
	// ambiguous the moment a second site is declared.
	for _, tc := range []struct{ name, gpu, wantSite, wantType string }{
		{"l40 is only at az61", "l40", "az61", "gpu.l40sx8"},
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
	base := ForgeSite{
		ID: "a", VpcID: "v", SSHKeyGroupID: "k",
		InstanceTypes: []ForgeInstanceType{{Name: "cpu", ID: "1"}},
	}

	neither := base
	if _, err := (&Forge{Sites: map[string]ForgeSite{"s": neither}}).Site("s"); err == nil {
		t.Error("a site with neither subnetID nor vpcPrefixID should not validate")
	}

	both := base
	both.SubnetID, both.VpcPrefixID = "s", "p"
	if _, err := (&Forge{Sites: map[string]ForgeSite{"s": both}}).Site("s"); err == nil {
		t.Error("a VPC never exposes both, so declaring both should not validate")
	}

	for _, tc := range []struct {
		name string
		set  func(*ForgeSite)
	}{
		{"subnet", func(s *ForgeSite) { s.SubnetID = "s" }},
		{"prefix", func(s *ForgeSite) { s.VpcPrefixID = "p" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := base
			tc.set(&site)
			if _, err := (&Forge{Sites: map[string]ForgeSite{"s": site}}).Site("s"); err != nil {
				t.Errorf("a site declaring only a %s should validate: %v", tc.name, err)
			}
		})
	}
}

// An un-onboarded site leaves its identifiers unset. That must not break a
// run against a site the repository does have, or declaring a second site
// becomes a coordinated change rather than an additive one.
func TestResolveSkipsIncompleteSites(t *testing.T) {
	usable := ForgeSite{
		ID: "a", VpcID: "v", SubnetID: "s", SSHKeyGroupID: "k",
		InstanceTypes: []ForgeInstanceType{
			{Name: "cpu", ID: "1", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
		},
	}
	env := &Forge{Sites: map[string]ForgeSite{"az61": usable, "ytl": {}}}

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
	site := ForgeSite{
		Key: "ytl",
		InstanceTypes: []ForgeInstanceType{
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
	site := ForgeSite{
		Key: "ytl", ID: "s", VpcID: "v", VpcPrefixID: "p", SSHKeyGroupID: "k",
		InstanceTypes: []ForgeInstanceType{
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
	ytl := ForgeSite{
		ID: "b", VpcID: "v", VpcPrefixID: "p", SSHKeyGroupID: "k",
		InstanceTypes: []ForgeInstanceType{
			{Name: "cpu.x86_64", ID: "1", Provides: map[string]string{"machineType": "cpu", "arch": "amd64"}},
			{Name: "cpu.arm", ID: "2", Provides: map[string]string{"machineType": "cpu", "arch": "arm64"}},
			{Name: "gpu.gb200", ID: "3", Provides: map[string]string{"machineType": "gpu", "arch": "arm64", "gpu": "gb200"}},
		},
	}
	env := &Forge{Sites: map[string]ForgeSite{"ytl": ytl}}

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
