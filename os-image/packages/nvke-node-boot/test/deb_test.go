// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkenodeboot_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// debVersion is the version every test in this file packages.
const debVersion = "0.1.0"

// buildDebScript is the packaging script under test, relative to this package.
var buildDebScript = filepath.Join("..", "packaging", "build-deb.sh")

// filesDir holds the sources the package takes ownership of.
var filesDir = filepath.Join("..", "..", "..", "files")

// nfpmBin resolves the nfpm binary build-deb.sh must run against: NFPM_BIN
// (set by `make test`, which downloads the pinned version), or nfpm on PATH.
// It skips rather than fails when neither exists, because the merge gate runs
// `make -C os-image test`, a plain `go test ./...` that does not build nfpm.
func nfpmBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("NFPM_BIN"); bin != "" {
		return bin
	}
	if bin, err := exec.LookPath("nfpm"); err == nil {
		return bin
	}
	t.Skip("nfpm not found: set NFPM_BIN, or put nfpm on PATH. Run 'make test' from the package root, which builds the pinned nfpm first.")
	return ""
}

// buildDeb builds the package and returns its path.
func buildDeb(t *testing.T) string {
	t.Helper()
	nfpm := nfpmBin(t)
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}
	out := filepath.Join(t.TempDir(), "out")
	cmd := exec.CommandContext(t.Context(), buildDebScript, out)
	cmd.Env = append(os.Environ(), "DEB_VERSION="+debVersion, "NFPM_BIN="+nfpm)
	combined, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "build-deb.sh failed: %s", combined)
	deb := filepath.Join(out, "nvke-node-boot_"+debVersion+"_all.deb")
	require.FileExists(t, deb)
	return deb
}

func TestBuildDebMetadata(t *testing.T) {
	deb := buildDeb(t)

	for field, want := range map[string]string{
		"Package":      "nvke-node-boot",
		"Version":      debVersion,
		"Architecture": "all",
	} {
		got, err := exec.CommandContext(t.Context(), "dpkg-deb", "-f", deb, field).Output()
		require.NoError(t, err)
		assert.Equal(t, want, strings.TrimSpace(string(got)), field)
	}

	// Nothing is installed under /etc, so there is no conffile and no dpkg prompt.
	ctrlDir := filepath.Join(t.TempDir(), "control")
	require.NoError(t, exec.CommandContext(t.Context(), "dpkg-deb", "-e", deb, ctrlDir).Run())
	assert.NoFileExists(t, filepath.Join(ctrlDir, "conffiles"))

	entries, err := os.ReadDir(ctrlDir)
	require.NoError(t, err)
	var scripts []string
	for _, e := range entries {
		switch e.Name() {
		case "control", "md5sums":
		default:
			scripts = append(scripts, e.Name())
		}
	}
	sort.Strings(scripts)
	assert.Equal(t, []string{"postrm"}, scripts)
}

// TestBuildDebContents checks that the package owns exactly the script, at the
// right path and mode, with the checkout's own bytes, and nothing under /etc.
func TestBuildDebContents(t *testing.T) {
	deb := buildDeb(t)

	extract := filepath.Join(t.TempDir(), "root")
	require.NoError(t, exec.CommandContext(t.Context(), "dpkg-deb", "-x", deb, extract).Run())

	installed := filepath.Join(extract, "usr", "lib", "nvke", "configure-console.sh")
	info, err := os.Stat(installed)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	got, err := os.ReadFile(installed)
	require.NoError(t, err)
	want, err := os.ReadFile(filepath.Join(filesDir, "configure-console.sh"))
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))

	// The drop-in is generated for the device the build chooses, never packaged:
	// a packaged copy would be an /etc conffile fixed to one device.
	assert.NoDirExists(t, filepath.Join(extract, "etc"))
}

// TestPostrmParses keeps a syntax slip in the maintainer script from reaching a node.
func TestPostrmParses(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "sh", "-n", filepath.Join("..", "packaging", "postrm")).CombinedOutput()
	require.NoErrorf(t, err, "postrm does not parse: %s", out)
}
