// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
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
	cfg.CAPNICo = CAPNICoAccess{
		Endpoint: "https://nico.example",
		OrgID:    "example-org",
		APIName:  "forge",
		Token:    "example-nico-token",
	}
	cfg.DGXCTenant = TenantLogin{User: "example-tenant", Password: "example-tenant-password"}
	cfg.ReleasesPath = "/srv/releases"
	cfg.SPIREExternalAddress = "192.0.2.10"
	return cfg
}

// TestDeploy_RegistersTheNicoSecretsForRedaction: the NICo token and the node
// password are masked in diagnostics, as the Postgres password is.
func TestDeploy_RegistersTheNicoSecretsForRedaction(t *testing.T) {
	const token, password = "redact-me-nico-token-7f3a", "redact-me-node-password-7f3a"
	cfg := nicoTestConfig(t)
	cfg.CAPNICo.Token = token
	cfg.DGXCTenant.Password = password
	if err := New(&fakeRunner{}, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if got := redact.Text("log: " + token + " " + password); strings.Contains(got, token) || strings.Contains(got, password) {
		t.Errorf("redact.Text = %q; want both secrets masked", got)
	}
}

// TestDeploy_NicoInstallsCAPNICoInsteadOfTheProvisionedProvider covers both
// halves of the switch: CAPNICo is there before CWE, and the provisioned
// provider is not installed at all.
func TestDeploy_NicoInstallsCAPNICoInsteadOfTheProvisionedProvider(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", nicoTestConfig(t)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	capnico := callIndex(runner, "deployment/"+capnicoDeployment)
	cwe := callIndex(runner, "upgrade --install "+cweReleaseName)
	if capnico < 0 || cwe < 0 || capnico > cwe {
		t.Fatalf("want CAPNICo (%d) installed before CWE (%d)", capnico, cwe)
	}
	if got := callIndex(runner, "provisioned-controller-manager"); got >= 0 {
		t.Errorf("the provisioned provider was installed (call %d) in nico mode", got)
	}
}

// TestDeploy_CWEAcceptsTheProviderPayloadTheBootstrapSends: CWE rejects a
// CreateCluster carrying a provider payload it is not configured for, so the
// chart's supportedInfraProviders must follow the infrastructure in use.
func TestDeploy_CWEAcceptsTheProviderPayloadTheBootstrapSends(t *testing.T) {
	for name, tt := range map[string]struct {
		cfg                 Config
		wantProvider        string
		wantNicoControllers string
	}{
		"provisioned": {testConfig(t), "supportedInfraProviders=infra", "controllers.nico.enabled=false"},
		"nico":        {nicoTestConfig(t), "supportedInfraProviders=nico", "controllers.nico.enabled=true"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", tt.cfg); err != nil {
				t.Fatalf("Deploy: %v", err)
			}
			cwe := callIndex(runner, "upgrade --install "+cweReleaseName)
			if cwe < 0 {
				t.Fatal("controller install call not found")
			}
			args := joinArgs(runner.calls[cwe].args)
			for _, want := range []string{tt.wantProvider, tt.wantNicoControllers} {
				if !strings.Contains(args, want) {
					t.Errorf("controller install args do not contain %q", want)
				}
			}
		})
	}
}

// TestDeploy_NicoGivesTheControllerTheTenantLoginAndReleases: the controller
// refuses to render a node's user data without DGXC_TENANT_USER and
// DGXC_TENANT_PASSWORD, and cannot resolve the release without the release
// manifests. So in nico mode the Secret exists before the controller installs,
// the chart is told its name, the releases are applied after the chart, and none
// of it is true in provisioned mode.
func TestDeploy_NicoGivesTheControllerTheTenantLoginAndReleases(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).Deploy(context.Background(), "/tmp/kubeconfig", nicoTestConfig(t)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	cwe := callIndex(runner, "upgrade --install "+cweReleaseName)
	if cwe < 0 || !strings.Contains(joinArgs(runner.calls[cwe].args), "secrets.dgxcTenantCredSecretName="+dgxcTenantSecret) {
		t.Fatalf("the controller install does not name the tenant credential Secret; call %d", cwe)
	}
	secret := callIndex(runner, "nvkectl-tenant-credentials-")
	releases := callIndex(runner, "-f /srv/releases")
	if secret < 0 || secret > cwe {
		t.Errorf("want the tenant Secret (%d) applied before the controller (%d)", secret, cwe)
	}
	if releases < 0 || releases < cwe {
		t.Errorf("want the release manifests (%d) applied after the controller chart (%d)", releases, cwe)
	}
	for _, call := range runner.calls {
		if args := joinArgs(call.args); strings.Contains(args, "example-tenant-password") {
			t.Errorf("the tenant password is on a command line: %q", args)
		}
	}

	provisioned := &fakeRunner{}
	if err := New(provisioned, nil).Deploy(context.Background(), "/tmp/kubeconfig", testConfig(t)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if got := callIndex(provisioned, "dgxcTenantCredSecretName"); got >= 0 {
		t.Errorf("provisioned mode named a tenant credential Secret (call %d)", got)
	}
}

// TestDeploy_NicoRefusesWithoutTheTenantLoginOrReleases: a nico Deploy that
// would only fail at the first node, minutes later, fails at once.
func TestDeploy_NicoRefusesWithoutTheTenantLoginOrReleases(t *testing.T) {
	for name, tt := range map[string]struct {
		mutate  func(*Config)
		wantErr string
	}{
		"no tenant user":     {func(c *Config) { c.DGXCTenant.User = "" }, "Config.DGXCTenant"},
		"no tenant password": {func(c *Config) { c.DGXCTenant.Password = "" }, "Config.DGXCTenant"},
		"no releases":        {func(c *Config) { c.ReleasesPath = "" }, "Config.ReleasesPath"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := nicoTestConfig(t)
			tt.mutate(&cfg)
			err := New(&fakeRunner{}, nil).Deploy(context.Background(), "/tmp/kubeconfig", cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Deploy error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestInstallCAPNICo_AppliesSecretsThenTheManifest: the Secrets and their
// namespace go first, because the manifest's Deployment starts pulling its
// image as soon as it exists, and the manifest is applied server-side because
// its CRDs are too large for a client-side apply. The rollout is waited for.
func TestInstallCAPNICo_AppliesSecretsThenTheManifest(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner, nil).installCAPNICo(context.Background(), "/tmp/kubeconfig", nicoTestConfig(t), time.Minute); err != nil {
		t.Fatalf("installCAPNICo: %v", err)
	}

	var applies []string
	for _, call := range runner.calls {
		if args := joinArgs(call.args); strings.Contains(args, "apply") {
			applies = append(applies, args)
		}
	}
	if len(applies) != 2 {
		t.Fatalf("want two applies (secrets, manifest), got %d: %v", len(applies), applies)
	}
	for _, args := range applies {
		for _, want := range []string{"--server-side", "--force-conflicts", "--kubeconfig /tmp/kubeconfig"} {
			if !strings.Contains(args, want) {
				t.Errorf("apply args = %q, want %q", args, want)
			}
		}
		// Nothing a credential could be read from passes on a command line.
		for _, secret := range []string{"example-nico-token", "example-registry-token"} {
			if strings.Contains(args, secret) {
				t.Errorf("apply args = %q carry the credential %q", args, secret)
			}
		}
	}
	if !strings.Contains(applies[0], "nvkectl-capnico-secrets-") || !strings.Contains(applies[1], "nvkectl-capnico-") {
		t.Errorf("want the secrets applied before the manifest, got %v", applies)
	}
	rollout := callIndex(runner, "-n "+capnicoNamespace+" rollout status deployment/"+capnicoDeployment)
	if rollout < 0 {
		t.Errorf("did not wait for the CAPNICo rollout; calls: %+v", runner.calls)
	}
}

// TestInstallCAPNICo_RunsTheManifestsDigestByDefault: the embedded manifest
// names the image by tag, which a registry can move, and the manager can read
// cluster Secrets. With no --release the install applies the digest the
// bootstrap manifest lists; a release's image still replaces it.
func TestInstallCAPNICo_RunsTheManifestsDigestByDefault(t *testing.T) {
	listed := manifest.Load().Components[manifest.ComponentCAPNICo].Images[manifest.ImageCAPNICo]
	wantImage := "ghcr.io/dsx-ai-factory/cluster-api-provider-nico/controller@" + listed.Digest

	runner := &fakeRunner{}
	if err := New(runner, nil).installCAPNICo(context.Background(), "/tmp/kubeconfig", nicoTestConfig(t), time.Minute); err != nil {
		t.Fatalf("installCAPNICo: %v", err)
	}
	var applied string
	for _, doc := range runner.applied {
		if strings.Contains(doc, "image: ") {
			applied = doc
		}
	}
	if !strings.Contains(applied, "        image: "+wantImage+"\n") {
		t.Errorf("the applied manifest does not run %s", wantImage)
	}
	if strings.Contains(applied, "controller:v") {
		t.Error("the applied manifest still runs the image by tag")
	}

	const release = "registry.example/capnico/controller@sha256:" +
		"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	cfg := nicoTestConfig(t)
	cfg.CAPNICoImage = release
	runner = &fakeRunner{}
	if err := New(runner, nil).installCAPNICo(context.Background(), "/tmp/kubeconfig", cfg, time.Minute); err != nil {
		t.Fatalf("installCAPNICo with a release image: %v", err)
	}
	found := false
	for _, doc := range runner.applied {
		found = found || strings.Contains(doc, "        image: "+release+"\n")
	}
	if !found {
		t.Errorf("the release's image %s was not applied", release)
	}
}

// TestPinnedCAPNICoImage: the repository without its tag, then the digest, and
// a refusal when the bootstrap manifest lists no digest.
func TestPinnedCAPNICoImage(t *testing.T) {
	got, err := pinnedCAPNICoImage()
	if err != nil {
		t.Fatal(err)
	}
	listed := manifest.Load().Components[manifest.ComponentCAPNICo].Images[manifest.ImageCAPNICo]
	if !strings.HasSuffix(got, "@"+listed.Digest) || strings.Contains(strings.TrimSuffix(got, "@"+listed.Digest), ":") {
		t.Errorf("pinnedCAPNICoImage = %q, want the repository, no tag, then @%s", got, listed.Digest)
	}
}

// TestCAPNICoSecretsManifest: the Secrets carry the names, keys and namespace
// CAPNICo and its manifest expect, which are the ones the deploy path's
// 20-nico-credentials template renders, that no pull secret is created for the
// public image, and that values needing escaping survive it.
func TestCAPNICoSecretsManifest(t *testing.T) {
	access := nicoTestConfig(t).CAPNICo
	access.Token = `tok"en\with-escapes`

	raw, err := capnicoSecretsManifest(access)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			Kind     string
			Metadata struct{ Name, Namespace string }
			Data     map[string]string `json:"stringData"`
		}
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if len(list.Items) != 2 {
		t.Fatalf("want a Namespace and the credentials Secret, got %d items", len(list.Items))
	}
	if ns := list.Items[0]; ns.Kind != "Namespace" || ns.Metadata.Name != capnicoNamespace {
		t.Errorf("first item = %+v, want the %s Namespace", ns, capnicoNamespace)
	}
	creds := list.Items[1]
	if creds.Metadata.Name != nicoCredentialsSecret || creds.Metadata.Namespace != capnicoNamespace {
		t.Errorf("credentials Secret = %+v", creds.Metadata)
	}
	for key, want := range map[string]string{"endpoint": access.Endpoint, "orgID": access.OrgID, "apiName": access.APIName, "token": access.Token} {
		if creds.Data[key] != want {
			t.Errorf("nico-credentials[%s] = %q, want %q", key, creds.Data[key], want)
		}
	}
}

// TestEmbeddedCAPNICoManifest: the manifest names the namespace and the
// Deployment the install waits for. If a bump of deploy/nico/capnico renames
// either, this fails instead of a bootstrap hanging.
func TestEmbeddedCAPNICoManifest(t *testing.T) {
	raw, err := manifestsFS.ReadFile(capnicoManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: " + capnicoNamespace,
		"name: " + capnicoDeployment,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("embedded CAPNICo manifest lacks %q", want)
		}
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
			name:    "no NICo token",
			mutate:  func(cfg *Config) { cfg.CAPNICo.Token = "" },
			wantErr: "CAPNICo.Token",
		},
		{
			name:    "apply failure",
			mutate:  func(*Config) {},
			outputs: []fakeOutput{{err: errors.New("boom")}},
			wantErr: "applying CAPNICo",
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

// TestEmbeddedCAPNICoManifest_IsTheDeployPathsManifest: the bootstrap installs
// the same CAPNICo, at the same pin, as the site controller's deploy path. The
// embedded copy is made by `make -C cli capnico-manifest`; this fails when it
// is not the file it was copied from, so a bump of one cannot leave the other.
func TestEmbeddedCAPNICoManifest_IsTheDeployPathsManifest(t *testing.T) {
	source, err := os.ReadFile("../../../../deploy/nico/capnico/capnico.yaml")
	if err != nil {
		t.Fatalf("reading the deploy path's CAPNICo manifest: %v", err)
	}
	embedded, err := manifestsFS.ReadFile(capnicoManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, embedded) {
		t.Error("the embedded CAPNICo manifest differs from deploy/nico/capnico/capnico.yaml; run `make -C cli capnico-manifest`")
	}
}

// TestBootstrapManifest_NamesTheEmbeddedCAPNICoImage: the bootstrap manifest
// is what `nvkectl get bootstrap-manifest` and the mirroring script read, so the
// CAPNICo image it lists has to be the one the embedded manifest runs.
func TestBootstrapManifest_NamesTheEmbeddedCAPNICoImage(t *testing.T) {
	raw, err := manifestsFS.ReadFile(capnicoManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := capnicoImageLine.FindAllSubmatch(raw, -1)
	if len(lines) != 1 {
		t.Fatalf("the embedded CAPNICo manifest names %d images, want 1", len(lines))
	}
	listed := manifest.Load().Components[manifest.ComponentCAPNICo].Images[manifest.ImageCAPNICo].Ref
	if got := string(lines[0][2]); listed != got {
		t.Errorf("bootstrap-manifest.yaml lists the CAPNICo image %q, the embedded manifest runs %q; update the manifest entry and its digest", listed, got)
	}
}

// TestBootstrapManifest_CAPNICoDigestIsTheTagsDigest: the default install runs
// the digest bootstrap-manifest.yaml lists, not the tag the embedded manifest
// names, and nothing offline can resolve a tag. So the tag and the digest are
// written down together here: a bump that edits the tag in capnico.yaml and the
// manifest ref, or the digest alone, fails until this pair is updated too. Take
// the digest from the registry, for the multi-architecture index:
//
//	crane digest <ref>
func TestBootstrapManifest_CAPNICoDigestIsTheTagsDigest(t *testing.T) {
	const (
		ref    = "ghcr.io/dsx-ai-factory/cluster-api-provider-nico/controller:v0.0.45"
		digest = "sha256:bca9d77a25e605a5c39faf0a71b45b484d363edc4535f8a37aa811a639738595"
	)
	listed := manifest.Load().Components[manifest.ComponentCAPNICo].Images[manifest.ImageCAPNICo]
	if listed.Ref != ref || listed.Digest != digest {
		t.Errorf("bootstrap-manifest.yaml lists CAPNICo %s@%s, this test pins %s@%s; after a bump, check the digest with `crane digest <ref>` and update both",
			listed.Ref, listed.Digest, ref, digest)
	}
}

// TestCAPNICoManifest_ReplacesTheImage: a release's digest replaces the one image
// the manifest names, and nothing else changes. With no image the manifest is
// untouched.
func TestCAPNICoManifest_ReplacesTheImage(t *testing.T) {
	raw, err := manifestsFS.ReadFile(capnicoManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	const image = "registry.example/capnico/controller@sha256:" +
		"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	same, err := capnicoManifest(raw, "")
	if err != nil || !bytes.Equal(same, raw) {
		t.Fatalf("capnicoManifest with no image changed the manifest (err %v)", err)
	}

	got, err := capnicoManifest(raw, image)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "        image: "+image+"\n") {
		t.Errorf("manifest does not run %s", image)
	}
	if strings.Contains(string(got), "cluster-api-provider-nico/controller:v") {
		t.Error("the manifest's own image is still there")
	}
	// Only that line differs.
	gotLines, rawLines := strings.Split(string(got), "\n"), strings.Split(string(raw), "\n")
	if len(gotLines) != len(rawLines) {
		t.Fatalf("line count %d, want %d", len(gotLines), len(rawLines))
	}
	changed := 0
	for i := range gotLines {
		if gotLines[i] != rawLines[i] {
			changed++
		}
	}
	if changed != 1 {
		t.Errorf("%d lines changed, want 1", changed)
	}
}

func TestCAPNICoManifest_RefusesAmbiguousImages(t *testing.T) {
	two := []byte("image: a/b:1\nimage: c/d:2\n")
	if _, err := capnicoManifest(two, "x/y@sha256:"+strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), "2 images") {
		t.Errorf("error = %v, want one about 2 images", err)
	}
}

// TestCAPNICoArtifact_IsTheOneTheDeployPathBinds: the bootstrap asks a release
// for the artifact deploy/nico/capnico/release.yaml binds to the manifest, and
// that file's image is the one the embedded manifest runs.
func TestCAPNICoArtifact_IsTheOneTheDeployPathBinds(t *testing.T) {
	data, err := os.ReadFile("../../../../deploy/nico/capnico/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var rel struct{ Image, Artifact string }
	if err := yaml.Unmarshal(data, &rel); err != nil {
		t.Fatal(err)
	}
	if rel.Artifact != CAPNICoArtifact {
		t.Errorf("release.yaml binds artifact %q, the bootstrap asks for %q", rel.Artifact, CAPNICoArtifact)
	}
	embedded, err := manifestsFS.ReadFile(capnicoManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(embedded), "image: "+rel.Image+"\n") {
		t.Errorf("release.yaml names image %q, which the embedded manifest does not run", rel.Image)
	}
}
