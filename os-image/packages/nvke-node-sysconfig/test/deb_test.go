// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkenodesysconfig_test

import (
	"io/fs"
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
	deb := filepath.Join(out, "nvke-node-sysconfig_"+debVersion+"_all.deb")
	require.FileExists(t, deb)
	return deb
}

func TestBuildDebMetadata(t *testing.T) {
	deb := buildDeb(t)

	for field, want := range map[string]string{
		"Package":      "nvke-node-sysconfig",
		"Version":      debVersion,
		"Architecture": "all",
		"Depends":      "systemd",
	} {
		got, err := exec.CommandContext(t.Context(), "dpkg-deb", "-f", deb, field).Output()
		require.NoError(t, err)
		assert.Equal(t, want, strings.TrimSpace(string(got)), field)
	}

	// Regular files go under /usr/lib or /lib, never /etc, so no file is a
	// conffile and an operator's override always wins without a dpkg prompt.
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
	assert.Equal(t, []string{"postinst", "postrm", "preinst", "prerm"}, scripts)
}

// TestBuildDebContents checks every file the package owns: its path, its mode,
// and that its bytes are the checkout's own copy under files/.
func TestBuildDebContents(t *testing.T) {
	deb := buildDeb(t)

	extract := filepath.Join(t.TempDir(), "root")
	require.NoError(t, exec.CommandContext(t.Context(), "dpkg-deb", "-x", deb, extract).Run())

	for _, tc := range []struct {
		name   string
		source string
		path   string
		mode   os.FileMode
	}{
		{"conntrack script", "configure-conntrack.sh", "usr/lib/nvke/configure-conntrack.sh", 0o755},
		{"conntrack unit", "conntrack-tune.service", "lib/systemd/system/conntrack-tune.service", 0o644},
		{"TIME_WAIT timeout", "50-nvke-conntrack-timeouts.conf", "usr/lib/sysctl.d/50-nvke-conntrack-timeouts.conf", 0o644},
		{"module loading", "50-nvke-conntrack-modules.conf", "usr/lib/modules-load.d/50-nvke-conntrack.conf", 0o644},
		{"etcd user", "50-nvke-etcd-sysusers.conf", "usr/lib/sysusers.d/50-nvke-etcd.conf", 0o644},
		{"etcd directory", "50-nvke-etcd-tmpfiles.conf", "usr/lib/tmpfiles.d/50-nvke-etcd.conf", 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installed := filepath.Join(extract, tc.path)
			info, err := os.Stat(installed)
			require.NoError(t, err)
			assert.Equal(t, tc.mode, info.Mode().Perm())

			got, err := os.ReadFile(installed)
			require.NoError(t, err)
			want, err := os.ReadFile(filepath.Join(filesDir, tc.source))
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))
		})
	}

	// The only thing under /etc is the mask links. A regular file there would
	// collide with the copies provision.sh left, and would be a conffile.
	var regular []string
	require.NoError(t, filepath.WalkDir(filepath.Join(extract, "etc"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Type()&fs.ModeSymlink == 0 {
			regular = append(regular, path)
		}
		return nil
	}))
	assert.Empty(t, regular, "no regular file may be installed under /etc")

	// The package must not own the etcd data directory: its owner does not exist
	// when dpkg unpacks, and a package must never carry a node's etcd data.
	assert.NoDirExists(t, filepath.Join(extract, "var", "lib", "etcd"))
}

// maskedUnits is every unit the package masks: a link to /dev/null that systemd
// reads as masked. See the comments in packaging/nfpm.yaml for why each is here.
var maskedUnits = []string{
	"apt-daily-upgrade.service",
	"apt-daily-upgrade.timer",
	"apt-daily.service",
	"apt-daily.timer",
	"multipathd.service",
	"multipathd.socket",
	"ovs-exporter.service",
	"unattended-upgrades.service",
}

func TestBuildDebMasksEveryUnitItShouldMask(t *testing.T) {
	deb := buildDeb(t)
	extract := filepath.Join(t.TempDir(), "root")
	require.NoError(t, exec.CommandContext(t.Context(), "dpkg-deb", "-x", deb, extract).Run())

	var got []string
	entries, err := os.ReadDir(filepath.Join(extract, "etc", "systemd", "system"))
	require.NoError(t, err)
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(extract, "etc", "systemd", "system", e.Name()))
		require.NoErrorf(t, err, "%s is not a link", e.Name())
		assert.Equalf(t, "/dev/null", target, "%s must be a mask", e.Name())
		got = append(got, e.Name())
	}
	sort.Strings(got)
	assert.Equal(t, maskedUnits, got)
}

// TestMaintainerScriptsParse runs every maintainer script through the shell's
// parser, so a quoting slip fails here rather than at dpkg time on a node.
func TestMaintainerScriptsParse(t *testing.T) {
	for _, name := range []string{"preinst", "postinst", "prerm", "postrm"} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), "sh", "-n", filepath.Join("..", "packaging", name)).CombinedOutput()
			assert.NoErrorf(t, err, "%s", out)
		})
	}
}
