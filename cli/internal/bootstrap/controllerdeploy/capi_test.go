// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/manifest"
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
		context.Background(), "/tmp/kubeconfig", cfg, time.Minute,
	); err != nil {
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
// TestEmbeddedProvisionedProviderManifest is the generated artifact's contract
// with installProvisionedProvider: it must carry the placeholder image exactly
// once, so rewriting it sets the image on the manager and nothing else, and it
// must still declare the provider's own objects. The install's placeholder
// check stays as a guard for a hand-edited file; this is what keeps it
// unreachable.
func TestEmbeddedProvisionedProviderManifest(t *testing.T) {
	raw, err := manifestsFS.ReadFile(provisionedProviderFile)
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(raw)

	if got := strings.Count(manifest, provisionedDefaultImage); got != 1 {
		t.Errorf("placeholder image %q appears %d times, want exactly 1; regenerate with `make -C cli provider-manifest`",
			provisionedDefaultImage, got)
	}
	if !strings.Contains(manifest, "GENERATED FILE") {
		t.Error("the embedded manifest has lost its generated-file banner")
	}
	for _, want := range []string{
		"kind: Namespace",
		"kind: CustomResourceDefinition",
		"kind: ClusterRole",
		"kind: Deployment",
		"name: " + provisionedDeployment,
		"namespace: " + provisionedNamespace,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the embedded manifest is missing %q", want)
		}
	}
}

// TestInstallProvisionedProvider_AppliesTheEmbeddedManifest: the image is
// rewritten and the result is what reaches kubectl, with no kustomize run.
func TestInstallProvisionedProvider_AppliesTheEmbeddedManifest(t *testing.T) {
	runner := &fakeRunner{}
	cfg := testConfig(t)

	if err := New(runner, nil).installProvisionedProvider(
		context.Background(), "/tmp/kubeconfig", cfg, time.Minute,
	); err != nil {
		t.Fatalf("installProvisionedProvider: %v", err)
	}
	for _, call := range runner.calls {
		if call.name == "kubectl" && len(call.args) > 0 && call.args[0] == "kustomize" {
			t.Error("the provider was rendered at run time; it is embedded")
		}
	}
	if len(runner.applied) != 1 {
		t.Fatalf("applied %d manifests, want 1", len(runner.applied))
	}
	applied := runner.applied[0]
	if !strings.Contains(applied, cfg.ProvisionedProviderImage) {
		t.Errorf("the applied manifest does not name the configured image %q", cfg.ProvisionedProviderImage)
	}
	if strings.Contains(applied, provisionedDefaultImage) {
		t.Errorf("the placeholder image survived into the applied manifest")
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

// TestDeploy_InstallsCAPIBeforeCWE is the ordering that matters: the controller starts
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
		t.Errorf("CAPI (%d) and the provider (%d) must be installed before the controller (%d)", capi, provider, cwe)
	}
}

func nicoTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.ProvisionedProviderImage = ""
	cfg.NicoInfrastructure = true
	cfg.NicoCredentialsFile = "/tmp/nico.env"
	cfg.CAPNICoVersion = "9.9.9"
	cfg.SPIREExternalAddress = "192.0.2.10"
	return cfg
}

// TestDeploy_NicoInstallsCAPNICoInsteadOfTheProvisionedProvider covers both
// halves of the switch: CAPNICo is there before CWE, and the provisioned
// provider is not installed at all.
func TestDeploy_NicoInstallsCAPNICoInsteadOfTheProvisionedProvider(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", nicoTestConfig(t)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	capnico := callIndex(runner, "upgrade --install "+capnicoReleaseName)
	cwe := callIndex(runner, "upgrade --install "+cweReleaseName)
	if capnico < 0 || cwe < 0 || capnico > cwe {
		t.Fatalf("want CAPNICo (%d) installed before CWE (%d)", capnico, cwe)
	}
	if got := callIndex(runner, "--server-side"); got >= 0 {
		t.Errorf("the provisioned provider was applied (call %d) in nico mode", got)
	}
}

// TestDeploy_CWEAcceptsTheProviderPayloadTheBootstrapSends: CWE rejects a
// CreateCluster carrying a provider payload it is not configured for, so the
// chart's supportedInfraProviders must follow the infrastructure in use.
func TestDeploy_CWEAcceptsTheProviderPayloadTheBootstrapSends(t *testing.T) {
	for name, tt := range map[string]struct {
		cfg  Config
		want string
	}{
		"provisioned": {testConfig(t), "supportedInfraProviders=infra"},
		"nico":        {nicoTestConfig(t), "supportedInfraProviders=nico"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", tt.cfg); err != nil {
				t.Fatalf("Deploy: %v", err)
			}
			cwe := callIndex(runner, "upgrade --install "+cweReleaseName)
			if cwe < 0 || !strings.Contains(joinArgs(runner.calls[cwe].args), tt.want) {
				t.Errorf("cwe install args do not contain %q", tt.want)
			}
		})
	}
}

// TestInstallCAPNICo_DefaultsComeFromTheBootstrapManifest: with no override, the
// chart and version are the ones the manifest pins, which is what `nkectl get
// bootstrap-manifest` tells an operator will be pulled.
func TestInstallCAPNICo_DefaultsComeFromTheBootstrapManifest(t *testing.T) {
	cfg := nicoTestConfig(t)
	cfg.CAPNICoVersion = ""
	chart, err := manifestChart(manifest.ComponentCAPNICo)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: []fakeOutput{{}, {err: errors.New("not found")}, {}}}

	if err := New(runner, nil).installCAPNICo(context.Background(), "/tmp/kubeconfig", cfg, time.Minute); err != nil {
		t.Fatalf("installCAPNICo: %v", err)
	}

	args := joinArgs(runner.calls[callIndex(runner, "upgrade --install "+capnicoReleaseName)].args)
	for _, want := range []string{strings.TrimRight(chart.Repo, "/") + "/" + chart.Name, "--version " + chart.Version} {
		if !strings.Contains(args, want) {
			t.Errorf("helm args = %q, want %q", args, want)
		}
	}
}

// TestInstallCAPNICo_Credentials covers the Secret step. The chart has to be
// installed first -- it creates capnico-system itself and fails if the
// namespace exists -- and the Secret is built from the file's path, so no
// credential value passes through a command line. A re-run against a kept
// bootstrap cluster finds the earlier Secret, which `kubectl create` would
// refuse to replace.
func TestInstallCAPNICo_Credentials(t *testing.T) {
	tests := []struct {
		name        string
		secretFound bool
	}{
		{"creates the Secret when there is none", false},
		{"deletes the old Secret before creating the new one", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getSecret := fakeOutput{err: errors.New("not found")}
			if tt.secretFound {
				getSecret = fakeOutput{}
			}
			runner := &fakeRunner{outputs: []fakeOutput{{}, getSecret, {}, {}}}

			if err := New(runner, nil).installCAPNICo(context.Background(), "/tmp/kubeconfig", nicoTestConfig(t), time.Minute); err != nil {
				t.Fatalf("installCAPNICo: %v", err)
			}

			helm := callIndex(runner, "upgrade --install "+capnicoReleaseName)
			del := callIndex(runner, "delete secret "+nicoCredentialsSecret)
			create := callIndex(runner, "create secret generic "+nicoCredentialsSecret)
			if helm < 0 || create < 0 || helm > create {
				t.Fatalf("want the chart (%d) installed before the Secret (%d); calls: %+v", helm, create, runner.calls)
			}
			if tt.secretFound && (del < 0 || del > create) {
				t.Errorf("want the old Secret deleted (%d) before the new one is created (%d)", del, create)
			}
			if !tt.secretFound && del >= 0 {
				t.Error("deleted a Secret that was not there")
			}
			helmArgs := joinArgs(runner.calls[helm].args)
			for _, want := range []string{
				"oci://ghcr.io/dsx-ai-factory/cluster-api-provider-nico/charts/capi-provider-nico",
				"--version 9.9.9", "--namespace " + capnicoNamespace, "--create-namespace", "--wait",
			} {
				if !strings.Contains(helmArgs, want) {
					t.Errorf("helm args = %q, want %q", helmArgs, want)
				}
			}
			if got := joinArgs(runner.calls[create].args); !strings.Contains(got, "--from-env-file=/tmp/nico.env") {
				t.Errorf("create args = %q, want the credentials file passed by path", got)
			}
		})
	}
}

func TestInstallCAPNICo_Errors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		outputs []fakeOutput
		wantErr string
	}{
		{
			name:    "no credentials file",
			mutate:  func(cfg *Config) { cfg.NicoCredentialsFile = "" },
			wantErr: "NicoCredentialsFile",
		},
		{
			// The chart is an OCI artifact, which Helm 3 cannot install.
			name:    "helm failure names what the chart needs",
			mutate:  func(*Config) {},
			outputs: []fakeOutput{{err: errors.New("boom")}},
			wantErr: "helm v4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := nicoTestConfig(t)
			tt.mutate(&cfg)

			err := New(&fakeRunner{outputs: tt.outputs}, nil).installCAPNICo(context.Background(), "/tmp/kubeconfig", cfg, time.Minute)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("installCAPNICo error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// spireInstallCall returns the helm arguments of the SPIRE release install.
func spireInstallCall(t *testing.T, runner *fakeRunner) string {
	t.Helper()
	i := callIndex(runner, "upgrade --install "+spireReleaseName+" ")
	if i < 0 {
		t.Fatal("SPIRE was not installed")
	}
	return joinArgs(runner.calls[i].args)
}

// TestDeploy_SPIREAttestation covers what the SPIRE install and the CWE install
// each carry, by infrastructure. The two places have to agree: nodes read the
// address CWE writes into their userdata, and reach SPIRE through the Service
// the install publishes.
func TestDeploy_SPIREAttestation(t *testing.T) {
	withThumbprints := testConfig(t)
	withThumbprints.TPMEKFingerprints = []string{strings.Repeat("a", 64)}
	noMTLS := nicoTestConfig(t)
	noMTLS.DisableMTLS = true
	noMTLS.SPIREExternalAddress = ""

	tests := []struct {
		name         string
		cfg          Config
		spireWant    []string
		spireAbsent  []string
		cweWant      []string
		wantNodePort bool
		wantSPIRE    bool
	}{
		{
			name:         "nico reads the allowlist CWE keeps, and nodes get the host address",
			cfg:          nicoTestConfig(t),
			spireWant:    []string{"nodeAttestor.tpmDirect.enabled=true", "extraVolumes[0].configMap.name=tpm-pubhashes", "extraVolumes[0].configMap.optional=true", "extraVolumeMounts[0].mountPath=/run/spire/data/tpm-direct/hashes"},
			spireAbsent:  []string{"tpmDirect.hashes["},
			cweWant:      []string{"spiffe.spire.serverAddress=192.0.2.10", "spiffe.spire.serverPort=30081"},
			wantNodePort: true,
			wantSPIRE:    true,
		},
		{
			name:         "provisioned with thumbprints keeps its static allowlist",
			cfg:          withThumbprints,
			spireWant:    []string{"tpmDirect.hashes[0]=" + strings.Repeat("a", 64)},
			spireAbsent:  []string{"extraVolumes", "extraVolumeMounts"},
			cweWant:      []string{"spiffe.spire.serverAddress=" + spireServiceHost(), "spiffe.spire.serverPort=" + spireServerPort},
			wantNodePort: true,
			wantSPIRE:    true,
		},
		{
			name:        "provisioned with no thumbprints has no TPM attestor and is not published",
			cfg:         testConfig(t),
			spireAbsent: []string{"tpmDirect", "extraVolumes"},
			cweWant:     []string{"spiffe.spire.serverAddress=" + spireServiceHost()},
			wantSPIRE:   true,
		},
		{
			name:    "nico without SPIFFE installs no SPIRE",
			cfg:     noMTLS,
			cweWant: []string{"spiffe.enabled=false"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", tt.cfg); err != nil {
				t.Fatalf("Deploy: %v", err)
			}

			if tt.wantSPIRE {
				spire := spireInstallCall(t, runner)
				for _, want := range tt.spireWant {
					if !strings.Contains(spire, want) {
						t.Errorf("SPIRE install = %q, want %q", spire, want)
					}
				}
				for _, absent := range tt.spireAbsent {
					if strings.Contains(spire, absent) {
						t.Errorf("SPIRE install = %q, must not contain %q", spire, absent)
					}
				}
			} else if callIndex(runner, "upgrade --install "+spireReleaseName+" ") >= 0 {
				t.Error("SPIRE was installed with SPIFFE disabled")
			}

			cwe := joinArgs(runner.calls[callIndex(runner, "upgrade --install "+cweReleaseName)].args)
			for _, want := range tt.cweWant {
				if !strings.Contains(cwe, want) {
					t.Errorf("cwe install = %q, want %q", cwe, want)
				}
			}
			if published := hasPublishedSpire(runner); published != tt.wantNodePort {
				t.Errorf("SPIRE published on a NodePort = %t, want %t", published, tt.wantNodePort)
			}
		})
	}
}

// hasPublishedSpire reports whether the SPIRE NodePort Service was applied.
func hasPublishedSpire(runner *fakeRunner) bool {
	for _, manifest := range runner.applied {
		if strings.Contains(manifest, "spire-server-external") {
			return true
		}
	}
	return false
}

func TestDeploy_NicoRequiresAnExternalSPIREAddress(t *testing.T) {
	cfg := nicoTestConfig(t)
	cfg.SPIREExternalAddress = ""

	err := New(&fakeRunner{}, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg)
	if err == nil || !strings.Contains(err.Error(), "SPIREExternalAddress") {
		t.Fatalf("Deploy error = %v, want one naming SPIREExternalAddress", err)
	}
}
