// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmdistro_test

import (
	"crypto/md5" //nolint:gosec // dpkg's own conffile-integrity mechanism, not a security boundary.
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixturePauseImage is the sandbox image the stubbed containerd config.toml
// carries, standing in for what bundle.sh renders from the locked image list.
// TestBuildDeb asserts the packaged config.toml still names it.
const fixturePauseImage = "registry.k8s.io/pause:3.10"

// debVersion is the Kubernetes release every test in this file packages.
const debVersion = "v1.36.3"

// baseImagesTarball is the file the .deb postinst feeds to
// `ctr -n k8s.io images import`; the name is part of that contract.
const baseImagesTarball = "k8s-base-container-images_1.36.3_linux_amd64.tgz"

// buildDebScript is the packaging script under test, relative to this package.
var buildDebScript = filepath.Join("..", "packaging", "build-deb.sh")

// writeSidecar writes the GNU-format `sha256sum -c` sidecar postinst verifies
// the image tarball against.
func writeSidecar(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	line := fmt.Sprintf("%x  %s\n", sha256.Sum256(data), filepath.Base(path))
	require.NoError(t, os.WriteFile(path+".sha256", []byte(line), 0o644))
}

// stubBundle writes a bundle tree of tiny files standing in for the real
// artifacts and the staged FHS runtime tree, and returns the bundle
// directory. withImages controls whether the container image tarball is
// present.
func stubBundle(t *testing.T, version string, withImages bool) (src string) {
	t.Helper()
	dir := t.TempDir()
	src = filepath.Join(dir, "dist", version)
	archDir := filepath.Join(src, "amd64")
	require.NoError(t, os.MkdirAll(archDir, 0o755))

	// The four matrix binaries bundle.sh chmods 0755 in the real bundle,
	// stubbed the same way here so a regression in that chmod shows up as a
	// mode mismatch in TestBuildDeb's dpkg-deb -c listing.
	for _, name := range []string{"kubeadm", "kubectl", "kubelet", "runc.amd64"} {
		require.NoError(t, os.WriteFile(filepath.Join(archDir, name), []byte("stub "+name+"\n"), 0o755))
	}
	if withImages {
		path := filepath.Join(archDir, baseImagesTarball)
		require.NoError(t, os.WriteFile(path, []byte("stub images\n"), 0o644))
		writeSidecar(t, path)
		writeStage(t, src)
	}
	return src
}

// writeStage writes the staged FHS runtime tree bundle.sh would produce from
// real containerd/CNI/crictl/helm tarballs: the extracted binaries, the CNI
// plugin directory, and the rendered containerd and crictl configs.
func writeStage(t *testing.T, src string) {
	t.Helper()
	stage := filepath.Join(src, "stage")

	binFiles := map[string]string{
		filepath.Join(stage, "usr", "local", "bin", "containerd"):              "stub containerd\n",
		filepath.Join(stage, "usr", "local", "bin", "containerd-shim-runc-v2"): "stub containerd-shim-runc-v2\n",
		filepath.Join(stage, "usr", "local", "bin", "ctr"):                     "stub ctr\n",
		filepath.Join(stage, "usr", "bin", "crictl"):                           "stub crictl\n",
		filepath.Join(stage, "usr", "bin", "helm"):                             "stub helm\n",
		filepath.Join(stage, "opt", "cni", "bin", "bridge"):                    "stub bridge\n",
		filepath.Join(stage, "opt", "cni", "bin", "loopback"):                  "stub loopback\n",
	}
	for path, content := range binFiles {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
	}

	config := "version = 3\n\n[plugins]\n  [plugins.'io.containerd.cri.v1.images']\n" +
		"    [plugins.'io.containerd.cri.v1.images'.pinned_images]\n" +
		"      sandbox = '" + fixturePauseImage + "'\n"
	configDir := filepath.Join(stage, "etc", "containerd")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(config), 0o644))

	crictlYAML := "runtime-endpoint: unix:///run/containerd/containerd.sock\nimage-endpoint: unix:///run/containerd/containerd.sock\ntimeout: 10\n"
	require.NoError(t, os.WriteFile(filepath.Join(stage, "etc", "crictl.yaml"), []byte(crictlYAML), 0o644))
}

// nfpmBin resolves the nfpm binary build-deb.sh must run against: NFPM_BIN
// (set by `make test`, which downloads the pinned version), or nfpm on PATH.
// nfpm builds the package, so its absence fails the test rather than skipping
// it. dpkg-deb, which only inspects the result, may be missing.
func nfpmBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("NFPM_BIN"); bin != "" {
		return bin
	}
	if bin, err := exec.LookPath("nfpm"); err == nil {
		return bin
	}
	// Skip rather than fail, matching the dpkg-deb check below. The merge gate
	// runs `make -C os-image test`, which is a plain `go test ./...` and does
	// not build nfpm; failing here would red the gate for a missing tool.
	// `make test` from this package root builds the pinned nfpm first.
	t.Skip("nfpm not found: set NFPM_BIN, or put nfpm on PATH. Run 'make test' from the package root, which builds the pinned nfpm first.")
	return ""
}

// TestBuildDeb runs packaging/build-deb.sh over a stubbed bundle tree of tiny
// files standing in for the real binaries, and inspects the resulting package.
// Nothing is downloaded.
func TestBuildDeb(t *testing.T) {
	nfpm := nfpmBin(t)
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}

	src := stubBundle(t, debVersion, true)
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, debVersion, src, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS=v1.36.3 v1.35.3 v1.34.2", "DEB_REVISION=1", "NFPM_BIN="+nfpm)
	combined, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "build-deb.sh failed: %s", combined)

	deb := filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb")
	require.FileExists(t, deb)

	fields := map[string]string{
		"Package":      "nke-kubeadm-distro-1.36.3",
		"Version":      "1",
		"Architecture": "amd64",
		"Provides":     "nke-kubeadm-distro",
		"Depends":      "iptables, iproute2, conntrack, socat, ethtool",
	}
	for field, want := range fields {
		got, err := exec.Command("dpkg-deb", "-f", deb, field).Output()
		require.NoError(t, err)
		assert.Equal(t, want, strings.TrimSpace(string(got)), field)
	}

	size, err := exec.Command("dpkg-deb", "-f", deb, "Installed-Size").Output()
	require.NoError(t, err)
	installedSize, err := strconv.Atoi(strings.TrimSpace(string(size)))
	require.NoError(t, err, "Installed-Size must be an integer number of KiB")
	assert.Positive(t, installedSize, "Installed-Size must cover the staged payload")

	conflicts, err := exec.Command("dpkg-deb", "-f", deb, "Conflicts").Output()
	require.NoError(t, err)
	for _, want := range []string{
		"kubeadm", "runc", "containerd", "containerd.io",
		"nke-kubeadm-distro", "nke-kubeadm-distro-1.35.3", "nke-kubeadm-distro-1.34.2",
	} {
		assert.Contains(t, string(conflicts), want)
	}
	assert.NotContains(t, string(conflicts), "nke-kubeadm-distro-1.36.3", "a package must not conflict with itself")

	// /etc/default/kubelet must be a conffile so operator edits survive an upgrade.
	ctrlDir := filepath.Join(t.TempDir(), "control")
	require.NoError(t, exec.Command("dpkg-deb", "-e", deb, ctrlDir).Run())
	conffiles, err := os.ReadFile(filepath.Join(ctrlDir, "conffiles"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/default/kubelet\n", string(conffiles))

	// dpkg owns every file in this package: the only maintainer scripts are
	// postinst, prerm and postrm.
	scripts, err := os.ReadDir(ctrlDir)
	require.NoError(t, err)
	var scriptNames []string
	for _, e := range scripts {
		switch e.Name() {
		case "control", "conffiles", "md5sums":
			continue
		default:
			scriptNames = append(scriptNames, e.Name())
		}
	}
	sort.Strings(scriptNames)
	assert.Equal(t, []string{"postinst", "postrm", "prerm"}, scriptNames)

	// DEBIAN/md5sums is what dpkg uses to tell a locally modified conffile from
	// an untouched one; a missing or stale entry defeats that on every payload
	// file, not just conffiles.
	md5sums, err := os.ReadFile(filepath.Join(ctrlDir, "md5sums"))
	require.NoError(t, err)
	require.NotEmpty(t, md5sums, "md5sums must not be empty")
	payloadDir := filepath.Join(t.TempDir(), "payload")
	require.NoError(t, exec.Command("dpkg-deb", "-x", deb, payloadDir).Run())
	for _, line := range strings.Split(strings.TrimSpace(string(md5sums)), "\n") {
		md5sumsFields := strings.SplitN(line, "  ", 2)
		require.Lenf(t, md5sumsFields, 2, "md5sums line: %q", line)
		wantSum, relPath := md5sumsFields[0], md5sumsFields[1]
		data, err := os.ReadFile(filepath.Join(payloadDir, relPath))
		require.NoErrorf(t, err, "payload file for md5sums entry %q", relPath)
		gotSum := fmt.Sprintf("%x", md5.Sum(data)) //nolint:gosec // see the import comment.
		assert.Equal(t, wantSum, gotSum, relPath)
	}

	// The rendered containerd config must still carry the pause image the
	// (fixture) locked image list named, so the sandbox image can never skew
	// from the bundled control-plane images.
	config, err := os.ReadFile(filepath.Join(payloadDir, "etc", "containerd", "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), fixturePauseImage)

	listing, err := exec.Command("dpkg-deb", "-c", deb).Output()
	require.NoError(t, err)
	contents := string(listing)
	// kubeadm's DirAvailable preflight check counts a hidden file as content, so
	// these directories have to ship genuinely empty.
	assert.NotContains(t, contents, ".kubelet-keep")
	for _, want := range []string{
		"/usr/bin/kubeadm",
		"/usr/bin/kubectl",
		"/usr/bin/kubelet",
		"/usr/bin/crictl",
		"/usr/bin/helm",
		"/usr/sbin/runc",
		"/usr/local/bin/containerd",
		"/usr/local/bin/containerd-shim-runc-v2",
		"/usr/local/bin/ctr",
		"/opt/cni/bin/bridge",
		"/opt/cni/bin/loopback",
		"/etc/containerd/config.toml",
		"/etc/crictl.yaml",
		"/lib/systemd/system/containerd.service",
		"/lib/systemd/system/kubelet.service",
		"/lib/systemd/system/kubelet.service.d/10-kubeadm.conf",
		"/etc/default/kubelet",
		"/etc/kubernetes/manifests/",
		"/var/lib/kubelet/",
		"/etc/cni/net.d/",
		"/etc/containerd/conf.d/",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/" + baseImagesTarball,
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/" + baseImagesTarball + ".sha256",
		"/usr/share/doc/nke-kubeadm-distro-1.36.3/README.Debian",
	} {
		assert.Contains(t, contents, want)
	}

	// dpkg owns everything, so k8s-matrix/<version>/amd64/ holds only the
	// image tarball and its sidecar: no node installer, no duplicated
	// binaries or tarballs, no bundle manifest.
	for _, notWant := range []string{
		"nke-k8s-installer",
		"/usr/lib/nke/k8s-matrix/v1.36.3/installer",
		"/usr/lib/nke/k8s-matrix/v1.36.3/manifest.json",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubeadm",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubectl",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubelet",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/runc.amd64",
	} {
		assert.NotContains(t, contents, notWant)
	}

	// /usr/bin/*, /usr/sbin/runc and the staged runtime binaries get 0755 from
	// nfpm.yaml's own file_info, which would mask a regression in bundle.sh's
	// chmod or in its tarball extraction mode.
	for _, want := range []string{
		"/usr/bin/kubeadm",
		"/usr/bin/kubectl",
		"/usr/bin/kubelet",
		"/usr/bin/crictl",
		"/usr/bin/helm",
		"/usr/sbin/runc",
		"/usr/local/bin/containerd",
		"/usr/local/bin/containerd-shim-runc-v2",
		"/usr/local/bin/ctr",
		"/opt/cni/bin/bridge",
		"/opt/cni/bin/loopback",
	} {
		line := findListingLine(t, contents, want)
		assert.Truef(t, strings.HasPrefix(line, "-rwxr-xr-x"), "expected 0755 for %s, got %q", want, line)
	}
}

// findListingLine returns the single `dpkg-deb -c` line for path (dpkg-deb
// lists paths as "./usr/bin/kubeadm", not absolute), or fails the test.
func findListingLine(t *testing.T, listing, path string) string {
	t.Helper()
	want := "." + path
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[len(fields)-1] == want {
			return line
		}
	}
	t.Fatalf("no dpkg-deb -c entry for %s", path)
	return ""
}

// chtimesAll sets every regular file under root to the given time. Used to
// prove the reproducibility fix actually normalises mtimes, rather than the
// two builds trivially agreeing because they read identical, untouched files.
func chtimesAll(t *testing.T, root string, when time.Time) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			return os.Chtimes(path, when, when)
		}
		return nil
	}))
}

// TestBuildDebReproducible guards nfpm.yaml's fixed mtime and build-deb.sh's
// pinning of the one `type: tree` entry (/opt/cni/bin): building the same
// input twice, with every source file at a different real mtime between the
// two builds, must still produce byte-identical .debs.
func TestBuildDebReproducible(t *testing.T) {
	nfpm := nfpmBin(t)
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}

	src := stubBundle(t, debVersion, true)

	build := func(outName string, srcTime time.Time) []byte {
		chtimesAll(t, src, srcTime)
		out := filepath.Join(t.TempDir(), outName)
		cmd := exec.Command(buildDebScript, debVersion, src, out)
		cmd.Env = append(os.Environ(), "PEER_VERSIONS="+debVersion, "DEB_REVISION=1", "NFPM_BIN="+nfpm)
		combined, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "build-deb.sh failed: %s", combined)
		data, err := os.ReadFile(filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))
		require.NoError(t, err)
		return data
	}

	first := build("out1", time.Date(2020, 1, 1, 1, 1, 0, 0, time.UTC))
	second := build("out2", time.Date(2024, 12, 31, 23, 59, 0, 0, time.UTC))
	assert.Equal(t, first, second, "building the same input twice must yield a byte-identical .deb")
}

// TestBuildDebRefusesBundleWithoutImages guards the offline contract: a
// package with no container images cannot bring a node up without a registry,
// so a bundle missing them is refused outright.
func TestBuildDebRefusesBundleWithoutImages(t *testing.T) {
	nfpm := nfpmBin(t)

	src := stubBundle(t, debVersion, false)
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, debVersion, src, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS="+debVersion, "DEB_REVISION=1", "NFPM_BIN="+nfpm)
	combined, err := cmd.CombinedOutput()
	require.Error(t, err, "build-deb.sh must refuse a bundle with no container images")
	assert.Contains(t, string(combined), "could not bring up a node offline")
	assert.NoFileExists(t, filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))
}

// TestBuildDebRefusesBundleWithoutStage guards the same offline contract for
// the staged runtime tree: a bundle whose stage step never ran is not
// buildable either.
func TestBuildDebRefusesBundleWithoutStage(t *testing.T) {
	nfpm := nfpmBin(t)

	src := stubBundle(t, debVersion, true)
	require.NoError(t, os.RemoveAll(filepath.Join(src, "stage")))
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, debVersion, src, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS="+debVersion, "DEB_REVISION=1", "NFPM_BIN="+nfpm)
	combined, err := cmd.CombinedOutput()
	require.Error(t, err, "build-deb.sh must refuse a bundle with no staged runtime tree")
	assert.Contains(t, string(combined), "missing staged file")
	assert.NoFileExists(t, filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))
}

// The postinst comments out swap entries in /etc/fstab. That line is a sed
// expression, so a syntax error in it only shows up when dpkg runs it, as
// "sed: -e expression #1, char N: Unmatched ( or \(" during Setting up, which
// fails the package and every package depending on it. Run the real line from
// the real file against a fixture.
func TestPostinstSwapSedIsValid(t *testing.T) {
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("sed is not on PATH")
	}

	postinst, err := os.ReadFile(filepath.Join(repoPackagingDir(t), "postinst"))
	if err != nil {
		t.Fatal(err)
	}

	var sedLine string
	for _, line := range strings.Split(string(postinst), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "sed ") && strings.Contains(trimmed, "swap") {
			sedLine = strings.TrimSuffix(trimmed, " /etc/fstab")
			break
		}
	}
	if sedLine == "" {
		t.Fatal("no swap sed line found in postinst; this test is guarding nothing")
	}

	fstab := filepath.Join(t.TempDir(), "fstab")
	const content = "/dev/sda1 / ext4 defaults 0 1\n" +
		"/swap.img none swap sw 0 0\n" +
		"  /indented.img none swap sw 0 0\n" +
		"#/already none swap sw 0 0\n"
	if err := os.WriteFile(fstab, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("sh", "-c", sedLine+" "+fstab).CombinedOutput()
	if err != nil {
		t.Fatalf("postinst swap sed failed: %v: %s", err, out)
	}

	got, err := os.ReadFile(fstab)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/dev/sda1 / ext4 defaults 0 1", // untouched
		"#/swap.img none swap sw 0 0",   // commented
		"#  /indented.img none swap sw 0 0",
		"#/already none swap sw 0 0", // still exactly one #
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("fstab missing %q; got:\n%s", want, got)
		}
	}
}

// repoPackagingDir returns the package's packaging/ directory.
func repoPackagingDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.Dir(wd), "packaging")
}
