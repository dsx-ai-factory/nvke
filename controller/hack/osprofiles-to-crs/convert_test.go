// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	enginev1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
)

const (
	activeName = "nke-1.31.12-251119"
	oldName    = "nke-260318-22.04-amd64"
	testIPXE   = "#!ipxe\nboot\n"
	testUser   = "#cloud-config\nhostname: {{ .Hostname }}\n"
)

func TestStripLicenseHeader(t *testing.T) {
	const header = "# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.\n# SPDX-License-Identifier: Apache-2.0\n"
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{"header after shebang with blank line", "#!ipxe\n" + header + "\n# note\nboot\n", "#!ipxe\n# note\nboot\n"},
		{"header after shebang without blank line", "#!ipxe\n" + header + "boot\n", "#!ipxe\nboot\n"},
		{"header after cloud-config", "#cloud-config\n" + header + "\nhostname: x\n", "#cloud-config\nhostname: x\n"},
		{"no header is unchanged", testIPXE, testIPXE},
		{"ordinary comment is kept", "#!ipxe\n# SPDX is mentioned later\nboot\n", "#!ipxe\n# SPDX is mentioned later\nboot\n"},
		{"single line is unchanged", "#!ipxe", "#!ipxe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stripLicenseHeader(tt.script))
		})
	}
}

func TestConvertIgnoresLicenseHeaderInScripts(t *testing.T) {
	const header = "# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.\n# SPDX-License-Identifier: Apache-2.0\n\n"
	plain := mustConvert(t, writeTree(t, activeMetadata(), "", map[string][2]string{activeName: {testIPXE, testUser}}))
	tests := []struct {
		name     string
		ipxe     string
		userData string
	}{
		{"header in ipxeScript", "#!ipxe\n" + header + "boot\n", testUser},
		{"header in userData.tmpl", testIPXE, "#cloud-config\n" + header + "hostname: {{ .Hostname }}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headed := mustConvert(t, writeTree(t, activeMetadata(), "", map[string][2]string{activeName: {tt.ipxe, tt.userData}}))
			assert.Equal(t, plain, headed)
		})
	}
}

func TestConvertActiveProfile(t *testing.T) {
	root := writeTree(t, activeMetadata(), "", map[string][2]string{
		activeName: {testIPXE, testUser},
	})

	first := mustConvert(t, root)
	second := mustConvert(t, root)
	require.Equal(t, first, second)
	require.Len(t, first, 3)

	imageBody := first[manifestName(activeName, "MachineImage")]
	assert.True(t, bytes.HasPrefix(imageBody, []byte("# SPDX-FileCopyrightText:")))
	assert.NotContains(t, string(imageBody), "Deprecated import")

	image := decodeImage(t, imageBody)
	pool := decodeRelease(t, first[manifestName(activeName, "NodePoolRelease")])
	cluster := decodeRelease(t, first[manifestName(activeName, "ClusterRelease")])
	assert.Equal(t, "MachineImage", image.Kind)
	assert.Equal(t, "NodePoolRelease", pool.Kind)
	assert.Equal(t, "ClusterRelease", cluster.Kind)
	assert.Equal(t, activeName, image.Name)
	assert.Equal(t, activeName, pool.Name)
	assert.Equal(t, activeName, cluster.Name)
	assert.Equal(t, osFamily, image.Spec.OperatingSystem.Family)
	assert.Equal(t, "22.04", image.Spec.OperatingSystem.Version)
	assert.Equal(t, "amd64", image.Spec.Architecture)
	assert.Equal(t, "fde", image.Spec.Variant)
	assert.Equal(t, "2025-11-19", image.Spec.BuildDate)
	assert.Equal(t, testIPXE, image.Spec.NICo.IPXEScript)
	assert.Equal(t, syncWave, image.Annotations[syncWaveAnnotation])
	assert.Equal(t, syncOptions, image.Annotations[syncOptionsAnnotation])
	assert.Empty(t, image.Annotations[kubernetesVersionAnnotation])

	for _, release := range []releaseView{pool, cluster} {
		assert.Equal(t, activeName, release.MachineImage)
		assert.Equal(t, testUser, release.UserData)
		assert.False(t, release.Deprecated)
		assert.Equal(t, syncWave, release.Annotations[syncWaveAnnotation])
		assert.Equal(t, syncOptions, release.Annotations[syncOptionsAnnotation])
		assert.Equal(t, "1.31.12", release.Annotations[kubernetesVersionAnnotation])
		assert.Equal(t, "false", release.Annotations[kubernetesVersionInjectedAnnotation])
	}
}

func TestConvertDeprecatedProfileStaysResolvable(t *testing.T) {
	const ipxe = "#!ipxe\necho deprecated\n"
	root := writeTree(t, "profiles: {}\n", deprecatedMetadata(), map[string][2]string{
		oldName: {ipxe, testUser},
	})

	docs := mustConvert(t, root)
	body, ok := docs[manifestName(oldName, "NodePoolRelease")]
	require.True(t, ok, "deprecated profile must still be emitted under its name")
	assert.Contains(t, string(body), "Upgrade-target search excludes it")

	image := decodeImage(t, docs[manifestName(oldName, "MachineImage")])
	pool := decodeRelease(t, body)
	cluster := decodeRelease(t, docs[manifestName(oldName, "ClusterRelease")])
	assert.Equal(t, oldName, image.Name)
	assert.Empty(t, image.Spec.Variant)
	for _, release := range []releaseView{pool, cluster} {
		assert.Equal(t, oldName, release.Name)
		assert.Equal(t, oldName, release.MachineImage)
		assert.True(t, release.Deprecated)
		assert.Empty(t, release.Annotations[kubernetesVersionAnnotation])
		assert.Equal(t, "true", release.Annotations[kubernetesVersionInjectedAnnotation])
	}
}

func TestConvertReadsDeprecatedProfileFromTheDeprecatedDir(t *testing.T) {
	const ipxe = "#!ipxe\necho deprecated\n"
	root := writeTree(t, "profiles: {}\n", deprecatedMetadata(), map[string][2]string{
		oldName: {ipxe, testUser},
	})
	require.FileExists(t, filepath.Join(root, "osProfiles", "deprecated", oldName, "ipxeScript"))
	require.NoFileExists(t, filepath.Join(root, "osProfiles", oldName, "ipxeScript"))

	image := decodeImage(t, mustConvert(t, root)[manifestName(oldName, kindMachineImage)])
	assert.Equal(t, ipxe, image.Spec.NICo.IPXEScript)
}

// Deprecating a profile moves its directory as well as its index entry. A
// directory left behind in the active tree is not found, which names the
// profile rather than silently generating nothing for it.
func TestConvertRejectsDeprecatedProfileLeftInTheActiveTree(t *testing.T) {
	root := writeTree(t, "profiles: {}\n", deprecatedMetadata(), nil)
	stale := filepath.Join(root, "osProfiles", oldName)
	require.NoError(t, os.MkdirAll(stale, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "ipxeScript"), []byte(testIPXE), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "userData.tmpl"), []byte(testUser), 0o644))

	_, err := convert(root)
	require.ErrorContains(t, err, oldName)
	require.ErrorContains(t, err, "read ipxeScript")
}

func TestConvertReadsBothIndexes(t *testing.T) {
	root := writeTree(t, activeMetadata(), deprecatedMetadata(), map[string][2]string{
		activeName: {testIPXE, testUser},
		oldName:    {"#!ipxe\necho deprecated\n", testUser},
	})
	docs := mustConvert(t, root)
	require.Len(t, docs, 6)

	activePool := decodeRelease(t, docs[manifestName(activeName, "NodePoolRelease")])
	oldPool := decodeRelease(t, docs[manifestName(oldName, "NodePoolRelease")])
	assert.False(t, activePool.Deprecated)
	assert.True(t, oldPool.Deprecated)
}

func TestConvertPreservesScriptBytes(t *testing.T) {
	const ipxe = "#!ipxe\nboot"
	const userData = "#!/bin/bash\nhostnamectl set-hostname {{ .Hostname }}"
	root := writeTree(t, activeMetadata(), "", map[string][2]string{
		activeName: {ipxe, userData},
	})
	docs := mustConvert(t, root)
	image := decodeImage(t, docs[manifestName(activeName, "MachineImage")])
	pool := decodeRelease(t, docs[manifestName(activeName, "NodePoolRelease")])
	assert.Equal(t, ipxe, image.Spec.NICo.IPXEScript)
	assert.Equal(t, userData, pool.UserData)
}

func TestConvertRejectsBadInput(t *testing.T) {
	t.Run("duplicate name", func(t *testing.T) {
		root := writeTree(t, activeMetadata(), activeAsDeprecated(), map[string][2]string{
			activeName: {testIPXE, testUser},
		})
		_, err := convert(root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both metadata.yaml and deprecated.yaml")
	})
	t.Run("names that are not Kubernetes object names", func(t *testing.T) {
		for _, name := range []string{"Nke-Upper", "nke_underscore", "-leading-dash", "trailing.", "double..dot", strings.Repeat("a", 254)} {
			root := writeTree(t, strings.ReplaceAll(activeMetadata(), activeName, name), "", map[string][2]string{name: {testIPXE, testUser}})
			_, err := convert(root)
			require.Error(t, err, name)
			assert.Contains(t, err.Error(), "not a valid Kubernetes object name", name)
		}
	})
	t.Run("dots in a name are valid", func(t *testing.T) {
		name := "nke-nico-vms-24.04-amd64-fde"
		root := writeTree(t, strings.ReplaceAll(activeMetadata(), activeName, name), "", map[string][2]string{name: {testIPXE, testUser}})
		docs := mustConvert(t, root)
		assert.Equal(t, name, decodeImage(t, docs[manifestName(name, "MachineImage")]).Name)
	})
	t.Run("missing ipxe script", func(t *testing.T) {
		root := writeTree(t, activeMetadata(), "", nil)
		_, err := convert(root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ipxeScript")
	})
	t.Run("omitted ubuntu version defaults to 22.04", func(t *testing.T) {
		meta := `profiles:
  nke-1.31.12-251119:
    kubernetes_version: "1.31.12"
    architecture: amd64
    build_date: "2025-11-19"
`
		root := writeTree(t, meta, "", map[string][2]string{
			activeName: {testIPXE, testUser},
		})
		image := decodeImage(t, mustConvert(t, root)[manifestName(activeName, "MachineImage")])
		assert.Equal(t, defaultUbuntuVersion, image.Spec.OperatingSystem.Version)
	})
}

func TestWriteIsDeterministic(t *testing.T) {
	root := writeTree(t, activeMetadata(), deprecatedMetadata(), map[string][2]string{
		activeName: {testIPXE, testUser},
		oldName:    {"#!ipxe\necho deprecated\n", testUser},
	})
	docs := mustConvert(t, root)
	out := t.TempDir()
	require.NoError(t, publish(out, docs, false))
	require.NoError(t, publish(out, docs, true))

	for name, body := range docs {
		written, err := os.ReadFile(filepath.Join(out, name))
		require.NoError(t, err)
		assert.Equal(t, body, written)
	}
}

func TestPublishAllowsDeprecationAndRejectsSpecChanges(t *testing.T) {
	root := writeTree(t, activeMetadata(), "", map[string][2]string{
		activeName: {testIPXE, testUser},
	})
	out := t.TempDir()
	require.NoError(t, publish(out, mustConvert(t, root), false))

	deprecatedRoot := writeTree(t, "profiles: {}\n", activeAsDeprecated(), map[string][2]string{
		activeName: {testIPXE, testUser},
	})
	require.NoError(t, publish(out, mustConvert(t, deprecatedRoot), false))
	pool := decodeRelease(t, mustRead(t, filepath.Join(out, manifestName(activeName, "NodePoolRelease"))))
	assert.True(t, pool.Deprecated)

	changedRoot := writeTree(t, "profiles: {}\n", activeAsDeprecated(), map[string][2]string{
		activeName: {"#!ipxe\nchanged\n", testUser},
	})
	err := publish(out, mustConvert(t, changedRoot), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "published spec")
	image := decodeImage(t, mustRead(t, filepath.Join(out, manifestName(activeName, "MachineImage"))))
	assert.Equal(t, testIPXE, image.Spec.NICo.IPXEScript)
}

func activeMetadata() string {
	return `profiles:
  nke-1.31.12-251119:
    kubernetes_version: "1.31.12"
    kubernetes_version_injected: false
    architecture: amd64
    build_date: "2025-11-19"
    ubuntu_version: "22.04"
    variant: fde
    image_base_url: "https://example.test/boot"
    bundle_url: "https://example.test/bundle"
`
}

func deprecatedMetadata() string {
	return `profiles:
  nke-260318-22.04-amd64:
    kubernetes_version: ""
    kubernetes_version_injected: true
    architecture: amd64
    build_date: "2026-03-18"
    ubuntu_version: "22.04"
`
}

func activeAsDeprecated() string {
	return `profiles:
  nke-1.31.12-251119:
    kubernetes_version: "1.31.12"
    kubernetes_version_injected: false
    architecture: amd64
    build_date: "2025-11-19"
    ubuntu_version: "22.04"
    variant: fde
`
}

// writeTree lays out a profiles root the way the fleet repository does: a
// profile listed in deprecated.yaml has its directory under
// osProfiles/deprecated/, every other one directly under osProfiles/.
func writeTree(t *testing.T, metadata, deprecated string, files map[string][2]string) string {
	t.Helper()
	root := t.TempDir()
	osDir := filepath.Join(root, "osProfiles")
	require.NoError(t, os.MkdirAll(osDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(osDir, "metadata.yaml"), []byte(metadata), 0o644))
	deprecatedNames := map[string]bool{}
	if deprecated != "" {
		require.NoError(t, os.WriteFile(filepath.Join(osDir, "deprecated.yaml"), []byte(deprecated), 0o644))
		var index struct {
			Profiles map[string]any `json:"profiles"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(deprecated), &index))
		for name := range index.Profiles {
			deprecatedNames[name] = true
		}
	}
	for name, pair := range files {
		dir := filepath.Join(osDir, name)
		if deprecatedNames[name] {
			dir = filepath.Join(osDir, "deprecated", name)
		}
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ipxeScript"), []byte(pair[0]), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "userData.tmpl"), []byte(pair[1]), 0o644))
	}
	return root
}

func mustConvert(t *testing.T, root string) map[string][]byte {
	t.Helper()
	docs, err := convert(root)
	require.NoError(t, err)
	return docs
}

func decodeImage(t *testing.T, body []byte) enginev1.MachineImage {
	t.Helper()
	var image enginev1.MachineImage
	require.NoError(t, yaml.UnmarshalStrict(body, &image))
	return image
}

// releaseView is what the tests read from either kind of release, which share
// one shape.
type releaseView struct {
	Kind         string
	Name         string
	Annotations  map[string]string
	MachineImage string
	UserData     string
	Deprecated   bool
}

func decodeRelease(t *testing.T, body []byte) releaseView {
	t.Helper()
	var kind metav1.TypeMeta
	require.NoError(t, yaml.Unmarshal(body, &kind))
	switch kind.Kind {
	case "NodePoolRelease":
		var r enginev1.NodePoolRelease
		require.NoError(t, yaml.UnmarshalStrict(body, &r))
		return releaseView{kind.Kind, r.Name, r.Annotations, string(r.Spec.MachineImageRef.Name), r.Spec.MachineInit.UserDataTemplate, r.Spec.Lifecycle.Deprecated}
	case "ClusterRelease":
		var r enginev1.ClusterRelease
		require.NoError(t, yaml.UnmarshalStrict(body, &r))
		return releaseView{kind.Kind, r.Name, r.Annotations, string(r.Spec.MachineImageRef.Name), r.Spec.MachineInit.UserDataTemplate, r.Spec.Lifecycle.Deprecated}
	}
	require.Failf(t, "unexpected kind", "kind %q", kind.Kind)
	return releaseView{}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	return body
}

func TestConvertWithoutGitOpsAnnotations(t *testing.T) {
	root := writeTree(t, activeMetadata(), "", map[string][2]string{activeName: {testIPXE, testUser}})

	docs, err := convertWith(root, false)
	require.NoError(t, err)

	image := decodeImage(t, docs[manifestName(activeName, "MachineImage")])
	assert.Empty(t, image.Annotations, "the image carries only Argo annotations, so none remain")
	assert.NotContains(t, string(docs[manifestName(activeName, "MachineImage")]), "annotations:")

	for _, kind := range []string{"NodePoolRelease", "ClusterRelease"} {
		release := decodeRelease(t, docs[manifestName(activeName, kind)])
		assert.NotContains(t, release.Annotations, syncWaveAnnotation)
		assert.NotContains(t, release.Annotations, syncOptionsAnnotation)
		assert.Equal(t, "1.31.12", release.Annotations[kubernetesVersionAnnotation], "version annotations are not Argo's")
		assert.Equal(t, "false", release.Annotations[kubernetesVersionInjectedAnnotation])
	}

	schemas, err := loadSchemas(crdDir)
	require.NoError(t, err)
	require.NoError(t, validateDocuments(schemas, docs))
}
