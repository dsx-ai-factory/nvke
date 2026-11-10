// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerdeploy

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
)

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
