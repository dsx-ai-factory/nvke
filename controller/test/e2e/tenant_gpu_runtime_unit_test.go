// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

func TestPlanTenantGPURuntime(t *testing.T) {
	for _, tc := range []struct {
		name         string
		enabled      bool
		capabilities map[string]string
		accelerator  string
		skip         string
	}{
		{name: "off", capabilities: map[string]string{"gpu": "gb200", "ubuntu": "24.04"}},
		{name: "gb200 on 24.04", enabled: true, capabilities: map[string]string{"gpu": "gb200", "ubuntu": "24.04"}, accelerator: "gb200"},
		{name: "no gpu", enabled: true, capabilities: map[string]string{"ubuntu": "24.04"}, skip: "asks for no GPU"},
		{name: "gpu without a recipe", enabled: true, capabilities: map[string]string{"gpu": "l40s", "ubuntu": "24.04"}, skip: "no AICR recipe for gpu=l40s"},
		{name: "26.04", enabled: true, capabilities: map[string]string{"gpu": "gb200", "ubuntu": "26.04"}, skip: `the run is on "26.04"`},
		// A run that names no series is not assumed to be on the one AICR takes.
		{name: "no series", enabled: true, capabilities: map[string]string{"gpu": "gb200"}, skip: `the run is on ""`},
		{name: "no capabilities", enabled: true, skip: "asks for no GPU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planTenantGPURuntime(tc.enabled, tc.capabilities)
			if plan.accelerator != tc.accelerator {
				t.Errorf("accelerator = %q, want %q", plan.accelerator, tc.accelerator)
			}
			if tc.skip == "" && plan.skip != "" || !strings.Contains(plan.skip, tc.skip) {
				t.Errorf("skip = %q, want it to contain %q", plan.skip, tc.skip)
			}
		})
	}
}

func TestTenantGPURuntimePlanSetsTheGPUCountOnlyWhenItInstalls(t *testing.T) {
	shape := clusterShape{workerPools: []nvke.NodePoolSpec{{DesiredNodeCount: 1}, {DesiredNodeCount: 2}}}

	settings := tenantGPURuntimePlan{accelerator: "gb200"}.tenantSettings(laneTenantSettings, shape, 5)
	if settings.gpuNodes == nil || *settings.gpuNodes != 3 {
		t.Fatalf("gpuNodes = %v, want every worker, 3", settings.gpuNodes)
	}
	if laneTenantSettings.gpuNodes != nil {
		t.Fatal("tenantSettings changed the shared lane settings")
	}

	// The Forge and DSX tenants have no worker pools in their shape; their
	// builder makes -e2e.tenant-workers workers.
	hardware := tenantGPURuntimePlan{accelerator: "gb200"}.tenantSettings(laneTenantSettings, nicoForgeTenant().shape, 2)
	if hardware.gpuNodes == nil || *hardware.gpuNodes != 2 {
		t.Fatalf("hardware tenant gpuNodes = %v, want -e2e.tenant-workers, 2", hardware.gpuNodes)
	}

	for _, plan := range []tenantGPURuntimePlan{{}, {skip: "no recipe"}} {
		if got := plan.tenantSettings(laneTenantSettings, shape, 5); got.gpuNodes != nil {
			t.Errorf("plan %+v set gpuNodes = %d; the GPU checks need a runtime to run against", plan, *got.gpuNodes)
		}
	}
}

// TestAICRRecipeLeavesTheFabricOut renders the recipe the install spec renders
// and checks that the overlay in aicrDataDir still lands on AICR's NVKE leaf.
// An aicr bump that renames the leaf or a fabric component fails here rather
// than on hardware. Rendering is offline; it needs only aicr on PATH.
func TestAICRRecipeLeavesTheFabricOut(t *testing.T) {
	if _, err := exec.LookPath("aicr"); err != nil {
		t.Skip("aicr is not on PATH")
	}
	output := filepath.Join(t.TempDir(), "recipe.yaml")
	command := exec.CommandContext(t.Context(), "aicr", aicrRecipeArgs("gb200", aicrDataDir, output)...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("aicr recipe: %v\n%s", err, out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var recipe struct {
		Metadata struct {
			AppliedOverlays []string `json:"appliedOverlays"`
		} `json:"metadata"`
		DeploymentOrder []string `json:"deploymentOrder"`
	}
	if err := yaml.Unmarshal(data, &recipe); err != nil {
		t.Fatal(err)
	}

	for _, overlay := range []string{"gb200-nvke-ubuntu-training", "gb200-nvke-ubuntu-training-no-fabric"} {
		if !slices.Contains(recipe.Metadata.AppliedOverlays, overlay) {
			t.Errorf("applied overlays %v do not include %s", recipe.Metadata.AppliedOverlays, overlay)
		}
	}
	for _, component := range []string{"network-operator", "sriov-ib-networks", "rdma-modules"} {
		if slices.Contains(recipe.DeploymentOrder, component) {
			t.Errorf("deployment order %v still installs %s", recipe.DeploymentOrder, component)
		}
	}
	if !slices.Contains(recipe.DeploymentOrder, "gpu-operator") {
		t.Errorf("deployment order %v does not install gpu-operator", recipe.DeploymentOrder)
	}
}

func TestSummarizeCTRF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	report := `{"reportFormat":"CTRF","results":{"tool":{"name":"aicr","version":"v1.1.2"},
		"summary":{"tests":2,"passed":1,"failed":1},
		"tests":[{"name":"operator-health","status":"passed","message":"ok"},
		         {"name":"check-nvidia-smi","status":"failed","message":"no GPU on node"}]}}`
	if err := os.WriteFile(path, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := summarizeCTRF(path)
	if err != nil {
		t.Fatalf("summarizeCTRF() error = %v", err)
	}
	want := "aicr v1.1.2\npassed  operator-health\nfailed  check-nvidia-smi: no GPU on node"
	if got != want {
		t.Errorf("summarizeCTRF() =\n%s\nwant\n%s", got, want)
	}
}

func TestSummarizeCTRFRejectsAReportWithNoChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(`{"results":{"tests":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty report would otherwise read as a run where nothing failed.
	if _, err := summarizeCTRF(path); err == nil || !strings.Contains(err.Error(), "lists no checks") {
		t.Fatalf("summarizeCTRF() error = %v, want one saying the report lists no checks", err)
	}
}

func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("lastLines(3 lines, 2) = %q, want %q", got, "b\nc")
	}
	if got := lastLines("a\nb", 5); got != "a\nb" {
		t.Errorf("lastLines(2 lines, 5) = %q, want %q", got, "a\nb")
	}
}
