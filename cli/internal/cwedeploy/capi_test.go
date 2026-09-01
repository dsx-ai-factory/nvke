// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwedeploy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestInstallCAPI_InstallsCoreThenTheProvider pins the two things that are easy
// to get wrong and impossible to see afterwards: clusterctl is asked for all
// three upstream providers at the build-supplied version, and the provisioned
// provider's placeholder image is replaced with the one the caller built.
func TestInstallCAPI_InstallsCoreThenTheProvider(t *testing.T) {
	const buildCAPIVersion = "v-test-build-version"
	previousCAPIVersion := capiVersion
	capiVersion = buildCAPIVersion
	t.Cleanup(func() { capiVersion = previousCAPIVersion })

	runner := &fakeRunner{}
	cfg := testConfig(t)

	if err := New(runner, nil).installCAPI(context.Background(), "/tmp/kubeconfig", cfg, time.Minute); err != nil {
		t.Fatalf("installCAPI returned error: %v", err)
	}

	init := ""
	var applied []string
	for _, call := range runner.calls {
		args := joinArgs(call.args)
		switch {
		case call.name == "clusterctl":
			init = args
		case call.name == "kubectl" && strings.Contains(args, "apply"):
			applied = append(applied, args)
		}
	}

	for _, want := range []string{
		"--core cluster-api:" + buildCAPIVersion,
		"--bootstrap kubeadm:" + buildCAPIVersion,
		"--control-plane kubeadm:" + buildCAPIVersion,
		"--kubeconfig /tmp/kubeconfig",
		"--wait-providers",
	} {
		if !strings.Contains(init, want) {
			t.Errorf("clusterctl init = %q, want it to contain %q", init, want)
		}
	}

	if len(applied) != 1 {
		t.Fatalf("expected one kubectl apply for the provider, got %d: %v", len(applied), applied)
	}
	// Server-side: the provider's CRDs are large enough that a client-side
	// apply can blow the last-applied-configuration annotation limit.
	if !strings.Contains(applied[0], "--server-side") {
		t.Errorf("provider apply = %q, want a server-side apply", applied[0])
	}
}

// TestInstallProvisionedProvider_RewritesTheImage checks the substitution
// itself, on the rendered bytes rather than on the command line: the image is
// inside the manifest, so an apply that "succeeded" with the placeholder still
// runs a provider image that does not exist.
func TestInstallProvisionedProvider_RewritesTheImage(t *testing.T) {
	runner := &fakeRunner{}
	cfg := testConfig(t)
	cfg.ProvisionedProviderImage = "ghcr.io/nvidia/nke/cluster-api-provider-provisioned:v0.1.0"

	if err := New(runner, nil).installProvisionedProvider(
		context.Background(), "/tmp/kubeconfig", cfg, time.Minute); err != nil {
		t.Fatalf("installProvisionedProvider returned error: %v", err)
	}

	if len(runner.applied) != 1 {
		t.Fatalf("expected one applied manifest, got %d", len(runner.applied))
	}
	manifest := runner.applied[0]
	if !strings.Contains(manifest, cfg.ProvisionedProviderImage) {
		t.Errorf("applied manifest does not run the built image %q:\n%s",
			cfg.ProvisionedProviderImage, manifest)
	}
	if strings.Contains(manifest, provisionedDefaultImage) {
		t.Errorf("applied manifest still carries the placeholder image %q:\n%s",
			provisionedDefaultImage, manifest)
	}
}

// TestInstallProvisionedProvider_RejectsAnUnexpectedRender is the guard for the
// substitution silently doing nothing. If the provider's manager manifest stops
// carrying the placeholder image, a blind ReplaceAll leaves the rendered YAML
// unchanged and the cluster runs an image nobody published — with the install
// reporting success.
func TestInstallProvisionedProvider_RejectsAnUnexpectedRender(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{
		{output: "image: some.registry/something-else:v1\n"},
	}}
	cfg := testConfig(t)

	err := New(runner, nil).installProvisionedProvider(
		context.Background(), "/tmp/kubeconfig", cfg, time.Minute)
	if err == nil {
		t.Fatal("expected an error when the render does not contain the placeholder image")
	}
	if !strings.Contains(err.Error(), provisionedDefaultImage) {
		t.Errorf("error %q does not name the image it looked for", err)
	}
}

// TestInstallCAPICore_SkipsWhenAlreadyInstalled: clusterctl refuses a second
// init, so a re-run against a kept bootstrap cluster has to probe first.
func TestInstallCAPICore_SkipsWhenAlreadyInstalled(t *testing.T) {
	runner := &fakeRunner{outputs: []fakeOutput{{output: "deployment.apps/capi-controller-manager"}}}

	if err := New(runner, nil).installCAPICore(context.Background(), "/tmp/kubeconfig"); err != nil {
		t.Fatalf("installCAPICore returned error: %v", err)
	}
	for _, call := range runner.calls {
		if call.name == "clusterctl" {
			t.Errorf("clusterctl was run against a cluster that already has CAPI: %s", joinArgs(call.args))
		}
	}
}

// TestDeploy_InstallsCAPIBeforeCWE is the ordering that matters: CWE starts
// writing CAPI objects as soon as a cluster is created, and an object written
// before its controller exists just sits there.
func TestDeploy_InstallsCAPIBeforeCWE(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}

	capi := callIndex(runner, "--core cluster-api:")
	provider := callIndex(runner, "--server-side")
	cwe := callIndex(runner, "upgrade --install "+cweReleaseName)

	if capi < 0 || provider < 0 || cwe < 0 {
		t.Fatalf("missing a step: capi=%d provider=%d cwe=%d; calls were %+v",
			capi, provider, cwe, runner.calls)
	}
	if capi > cwe || provider > cwe {
		t.Errorf("CAPI (%d) and the provider (%d) must be installed before CWE (%d)", capi, provider, cwe)
	}
}
