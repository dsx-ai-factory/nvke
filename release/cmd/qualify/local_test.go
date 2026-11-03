// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// The steps append, so a value resolved twice is read back as the last one,
// which is what $GITHUB_ENV does in the workflow.
func TestReadEnvFileKeepsTheOrderWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.env")
	if err := os.WriteFile(path, []byte("CLI_VERSION=v1\nnot a value\nSITE_ARCH=amd64\nCLI_VERSION=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CLI_VERSION=v1", "SITE_ARCH=amd64", "CLI_VERSION="}; !slices.Equal(got, want) {
		t.Errorf("readEnvFile = %q, want %q", got, want)
	}
}

func TestLocalBuildsOnlyWhatTheLaneCanBuild(t *testing.T) {
	err := runLocal(context.Background(), io.Discard, localOptions{build: []string{"os-image"}})
	if err == nil || !strings.Contains(err.Error(), "only controller, cli, capi-provisioned") {
		t.Errorf("runLocal(--build os-image) = %v, want it refused", err)
	}
}

func TestLocalRefusesAnUnknownStage(t *testing.T) {
	err := runLocal(context.Background(), io.Discard, localOptions{stage: "fll"})
	if err == nil || !strings.Contains(err.Error(), "provision, bootstrap, deployment, full") {
		t.Errorf("runLocal(--stage fll) = %v, want it refused", err)
	}
}

// Planning and the Teleport checks read the environment, not the options, so
// what the flags and the env file give must be there before either runs.
func TestLocalSettlesTheEnvironmentFirst(t *testing.T) {
	t.Setenv("QUALIFY_E2E_NGC_ORG", "from-env")
	t.Setenv("TELEPORT_PROXY", "")
	t.Setenv("FORGE_TELEPORT_PROXY", "proxy.example:443")
	_ = runLocal(context.Background(), io.Discard, localOptions{ngcOrg: "from-flag", build: []string{"os-image"}})
	if got := os.Getenv("QUALIFY_E2E_NGC_ORG"); got != "from-flag" {
		t.Errorf("QUALIFY_E2E_NGC_ORG = %q, want --ngc-org's from-flag", got)
	}
	if got := os.Getenv("TELEPORT_PROXY"); got != "proxy.example:443" {
		t.Errorf("TELEPORT_PROXY = %q, want FORGE_TELEPORT_PROXY's", got)
	}

	t.Setenv("TELEPORT_PROXY", "set.example:443")
	_ = runLocal(context.Background(), io.Discard, localOptions{build: []string{"os-image"}})
	if got := os.Getenv("TELEPORT_PROXY"); got != "set.example:443" {
		t.Errorf("TELEPORT_PROXY = %q, want the one already set kept", got)
	}
}

func TestResolveBuild(t *testing.T) {
	all := []string{"controller", "cli", "capi-provisioned"}
	for _, tc := range []struct {
		name                string
		in                  localOptions
		wantBuild           []string
		wantRegistry, wantE string
	}{
		{name: "the default pulls from nvcr", in: localOptions{}, wantRegistry: "nvcr"},
		{name: "a build keeps the registry it names", in: localOptions{build: []string{"capi-provisioned"}, registry: "ghcr"}, wantBuild: []string{"capi-provisioned"}, wantRegistry: "ghcr"},
		{name: "from source builds all and names them locally", in: localOptions{fromSource: true}, wantBuild: all, wantRegistry: "local"},
		{name: "from source refuses a registry to pull from", in: localOptions{fromSource: true, registry: "nvcr"}, wantE: "pulls nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.in
			err := o.resolveBuild()
			if tc.wantE != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantE) {
					t.Fatalf("resolveBuild = %v, want %q", err, tc.wantE)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(o.build, tc.wantBuild) || o.registry != tc.wantRegistry {
				t.Errorf("resolveBuild = build %q registry %q, want %q %q", o.build, o.registry, tc.wantBuild, tc.wantRegistry)
			}
		})
	}
}

// The result is written from what ran: a built component names the commit it
// was built at and no location, and what was pulled stays as planned.
func TestWriteAsBuiltNamesWhatRan(t *testing.T) {
	dir := t.TempDir()
	planned := qualification.Plan{Subject: "nvke", Version: "v1.0.0", Scope: "full", Combinations: []qualification.Combination{{
		ID: "c1",
		Components: []qualification.ComponentVersion{
			{Name: "controller", Version: "v1.0.0", URL: "oci://example.com/nvke-controller:v1.0.0", VersionStrategy: "gitTag"},
			{Name: "cli", Version: "v1.0.0", VersionStrategy: "gitTag"},
		},
		Dependencies: []qualification.ComponentVersion{{Name: "kube", Version: "1.36.3"}},
		Streams: []qualification.Stream{{Name: "s1", Dependencies: []qualification.ComponentVersion{
			{Name: "cli-arm64", URL: "oci://example.com/nvkectl:v1.0.0-linux-arm64"},
		}}},
	}}}
	b, err := json.Marshal(planned)
	if err != nil {
		t.Fatal(err)
	}
	planAt, outAt, envAt := filepath.Join(dir, "plan.json"), filepath.Join(dir, "as-run.json"), filepath.Join(dir, "steps.env")
	if err := os.WriteFile(planAt, b, 0o600); err != nil {
		t.Fatal(err)
	}
	resolved := "SITE_NVKE_CONTROLLER_VERSION=v1.0.0\nSITE_NVKE_CONTROLLER_VERSION=\nSITE_NVKE_CONTROLLER_VERSION=0123456789ab\nCLI_VERSION=v1.0.0\n"
	if err := os.WriteFile(envAt, []byte(resolved), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAsBuilt(planAt, outAt, envAt, []string{"controller"}); err != nil {
		t.Fatal(err)
	}
	got, err := qualification.LoadPlan(outAt)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Combinations[0]
	want := qualification.ComponentVersion{Name: "controller", Version: "0123456789ab", VersionStrategy: builtStrategy}
	if c.Components[0] != want {
		t.Errorf("controller = %+v, want %+v", c.Components[0], want)
	}
	if c.Components[1] != planned.Combinations[0].Components[1] || c.Streams[0].Dependencies[0] != planned.Combinations[0].Streams[0].Dependencies[0] {
		t.Errorf("what was pulled changed: %+v", c)
	}

	if err := os.WriteFile(envAt, []byte("CLI_VERSION=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAsBuilt(planAt, outAt, envAt, []string{"cli"}); err == nil || !strings.Contains(err.Error(), "recorded no CLI_VERSION") {
		t.Errorf("writeAsBuilt with no recorded version = %v", err)
	}
}

func TestCheckPlanInputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    localOptions
		wantE string
	}{
		{name: "nothing chosen", in: localOptions{}},
		{name: "a version and a pulled override", in: localOptions{version: "v1.0.0", overrides: "os-image-arm64=https://example.com/a.qcow2,controller=v1.0.0"}},
		{name: "a version with a plan to read", in: localOptions{plan: "plan.json", version: "v1.0.0"}, wantE: "--plan skips planning"},
		{name: "overrides with a plan to read", in: localOptions{plan: "plan.json", overrides: "kube=1.36.3"}, wantE: "--plan skips planning"},
		{name: "a malformed override", in: localOptions{overrides: "kube"}, wantE: "not name=version"},
		{name: "an override of a built component", in: localOptions{build: []string{"cli"}, overrides: "cli-arm64=v1.0.0"}, wantE: "cli-arm64 is built from this checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.checkPlanInputs()
			if tc.wantE == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantE) {
				t.Fatalf("checkPlanInputs = %v, want %q", err, tc.wantE)
			}
		})
	}
}

// A build stands in for every plan entry of that component, and only those:
// a location still holding something pulled is still asked about.
func TestPlannedCoversWhatIsBuilt(t *testing.T) {
	o := localOptions{build: []string{"cli", "capi-provisioned"}}
	got := o.planned()
	if want := []string{"cli", "cli-amd64", "cli-arm64", "capi-provisioned"}; !slices.Equal(got, want) {
		t.Errorf("planned = %q, want %q", got, want)
	}
	if !subset([]string{"cli-amd64"}, got) || subset([]string{"cli-amd64", "controller"}, got) {
		t.Errorf("subset: a location is skipped only when everything it holds is built")
	}
}

func TestJumpHostTakesTheFirstSiteSet(t *testing.T) {
	t.Setenv("FORGE_JUMP_HOST", "")
	t.Setenv("FORGE_SITE_2_JUMP_HOST", "two.example")
	t.Setenv("FORGE_SITE_2_TELEPORT_LOGIN", "root")
	t.Setenv("DSX_DATAHALL_1_JUMP_HOST", "dsx.example")
	if v, h, l := jumpHost(); v != "FORGE_SITE_2_JUMP_HOST" || h != "two.example" || l != "root" {
		t.Errorf("jumpHost = %q, %q, %q; want site 2's", v, h, l)
	}
}
