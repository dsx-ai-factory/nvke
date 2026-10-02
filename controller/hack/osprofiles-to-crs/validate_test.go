// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crdDir is the generated CRD directory the chart installs, relative to this package.
const crdDir = "../../" + defaultCRDDir

// fixtureProfiles is the in-repo profiles tree the local stack and e2e use, and
// fixtureReleases is the committed set of manifests generated from it.
const (
	fixtureProfiles = "../../test/fixtures/profiles"
	fixtureReleases = "../../test/fixtures/releases"
	exampleProfiles = "../../profiles/example/profiles"
)

func TestConvertKeepsVariantAsWritten(t *testing.T) {
	meta := strings.ReplaceAll(activeMetadata(), "variant: fde", "variant: na-0.1.29")
	root := writeTree(t, meta, "", map[string][2]string{activeName: {testIPXE, testUser}})

	docs := mustConvert(t, root)
	image := decodeImage(t, docs[manifestName(activeName, "MachineImage")])
	assert.Equal(t, "na-0.1.29", image.Spec.Variant)

	schemas, err := loadSchemas(crdDir)
	require.NoError(t, err)
	require.NoError(t, validateDocuments(schemas, docs))
}

func TestValidateDocumentsAdmitsGeneratedManifests(t *testing.T) {
	root := writeTree(t, activeMetadata(), deprecatedMetadata(), map[string][2]string{
		activeName: {testIPXE, testUser},
		oldName:    {"#!ipxe\necho deprecated\n", testUser},
	})
	schemas, err := loadSchemas(crdDir)
	require.NoError(t, err)
	require.NoError(t, validateDocuments(schemas, mustConvert(t, root)))
}

func TestValidateDocumentsAdmitsRepositoryProfiles(t *testing.T) {
	schemas, err := loadSchemas(crdDir)
	require.NoError(t, err)
	for name, dir := range map[string]string{"test fixtures": fixtureProfiles, "example profiles": filledExample(t)} {
		t.Run(name, func(t *testing.T) {
			docs, err := convert(dir)
			require.NoError(t, err)
			require.NotEmpty(t, docs)
			require.NoError(t, validateDocuments(schemas, docs))
		})
	}
}

// filledExample copies the OSS example profiles with every REPLACE_ value
// filled in, the way an operator does before building the content image.
func filledExample(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	replacements := strings.NewReplacer(
		"REPLACE_KUBERNETES_VERSION", "1.34.2",
		"REPLACE_BUILD_DATE", "2026-01-01",
		"REPLACE_BOOT_ASSET_BASE_URL", "https://example.test/boot",
		"REPLACE_UBUNTU_24_04_SERVER_ISO_URL", "https://example.test/ubuntu.iso",
	)
	require.NoError(t, filepath.WalkDir(exampleProfiles, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(exampleProfiles, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, []byte(replacements.Replace(string(data))), 0o644)
	}))
	return dst
}

// The fixture manifests are what Tilt, the CI lanes and the gofail e2e install
// with kubectl or the API, never Argo CD, so it carries no Argo annotations.
// It is committed, so it has to follow the profiles it is generated from.
func TestFixtureReleasesMatchProfiles(t *testing.T) {
	err := run([]string{"--profiles", fixtureProfiles, "--crds", crdDir, "--output", fixtureReleases, "--license", licenseApache, "--gitops=false", "--check"})
	require.NoError(t, err, "regenerate with `make fixture-releases`")
}

func TestRelicense(t *testing.T) {
	docs := map[string][]byte{"a.yaml": []byte(fileHeader + "kind: MachineImage\n")}

	require.NoError(t, relicense(docs, licenseProprietary))
	assert.Contains(t, string(docs["a.yaml"]), licenseProprietary)

	require.NoError(t, relicense(docs, licenseApache))
	assert.Contains(t, string(docs["a.yaml"]), "SPDX-License-Identifier: Apache-2.0\n")
	assert.NotContains(t, string(docs["a.yaml"]), licenseProprietary)

	require.Error(t, relicense(docs, "MIT"))
}

func TestValidateDocumentsRejectsWhatTheAPIServerRejects(t *testing.T) {
	root := writeTree(t, activeMetadata(), "", map[string][2]string{activeName: {testIPXE, testUser}})
	valid := mustConvert(t, root)
	imageName := manifestName(activeName, "MachineImage")
	poolName := manifestName(activeName, "NodePoolRelease")

	tests := []struct {
		name    string
		file    string
		from    string
		to      string
		wantErr string
	}{
		{name: "variant outside the label pattern", file: imageName, from: "variant: fde", to: "variant: NA_0", wantErr: "variant"},
		{name: "ipxe script without shebang", file: imageName, from: "#!ipxe", to: "boot", wantErr: "ipxe"},
		{name: "user data without cloud-config header", file: poolName, from: "#cloud-config", to: "hostname", wantErr: "cloud-init header"},
		{name: "unbalanced template braces", file: poolName, from: "{{ .Hostname }}", to: "{{ .Hostname", wantErr: "{{"},
		{name: "os version not numeric", file: imageName, from: `version: "22.04"`, to: `version: "jammy"`, wantErr: "version"},
	}
	schemas, err := loadSchemas(crdDir)
	require.NoError(t, err)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := map[string][]byte{}
			for name, body := range valid {
				docs[name] = body
			}
			require.Contains(t, string(docs[tt.file]), tt.from)
			docs[tt.file] = []byte(strings.Replace(string(docs[tt.file]), tt.from, tt.to, 1))

			err := validateDocuments(schemas, docs)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.file)
			assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tt.wantErr))
		})
	}
}

func TestValidateDocumentRejectsUnknownKind(t *testing.T) {
	schemas, err := loadSchemas(crdDir)
	require.NoError(t, err)
	err = validateDocument(schemas, []byte("apiVersion: internal.nke.nvidia.com/v1\nkind: Widget\nmetadata:\n  name: x\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no CRD for kind")
}

func TestLoadSchemasRequiresCRDs(t *testing.T) {
	_, err := loadSchemas(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no CRD manifests")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("kind: [unterminated"), 0o644))
	_, err = loadSchemas(dir)
	require.Error(t, err)
}

func TestRunValidatesAgainstCRDs(t *testing.T) {
	out := t.TempDir()
	require.NoError(t, run([]string{"--profiles", fixtureProfiles, "--crds", crdDir, "--output", out}))
	require.NoError(t, run([]string{"--profiles", fixtureProfiles, "--crds", crdDir, "--output", out, "--check"}))
	require.Error(t, run([]string{"--profiles", fixtureProfiles, "--crds", crdDir, "--output", out, "--license", "MIT"}))

	err := run([]string{"--profiles", fixtureProfiles, "--crds", t.TempDir(), "--output", out})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no CRD manifests")
}
