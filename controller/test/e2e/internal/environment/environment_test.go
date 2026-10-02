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
