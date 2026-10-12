// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmdistro_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bundleScript is the script under test, relative to this package.
var bundleScript = func() string {
	abs, err := filepath.Abs(filepath.Join("..", "bundle.sh"))
	if err != nil {
		panic(err)
	}
	return abs
}()

// The pins bundle.sh carries for Kubernetes 1.36, restated here because the
// fixture server has to serve exactly the URLs those pins produce.
const (
	fixtureVersion    = "v1.36.3"
	fixtureContainerd = "2.3.0"
	fixtureRunc       = "v1.4.2"
	fixtureHelm       = "v4.1.4"
	fixtureCriTools   = "v1.36.0"
	fixtureCNI        = "v1.6.0"
)

// fixtureImages are what the fake kubeadm lists and the fake crane resolves,
// in the order kubeadm prints them, which is unsorted so the lock's own
// sorting is exercised.
var fixtureImages = []struct{ ref, digest string }{
	{"registry.k8s.io/kube-apiserver:v1.36.3", "sha256:" + strings.Repeat("a", 64)},
	{"registry.k8s.io/pause:3.10", "sha256:" + strings.Repeat("b", 64)},
}

// requireBundleTools skips a test on a host that cannot run bundle.sh at all.
func requireBundleTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "gpg", "curl", "tar", "gzip", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}
}

// tarGz builds a gzip-compressed tar from name -> content.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body)),
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// fakeKubeadm is served as the kubeadm artifact itself: bundle.sh runs the
// binary it downloaded, not one from PATH.
func fakeKubeadm() string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	for _, img := range fixtureImages {
		fmt.Fprintf(&b, "echo %q\n", img.ref)
	}
	return b.String()
}

// fixtureServer is the local stand-in for dl.k8s.io, github.com and
// get.helm.sh, counting every request so a warm-cache run can be shown to
// make none.
type fixtureServer struct {
	*httptest.Server
	requests atomic.Int64
	// mu guards bodies and sha256: the handler reads them on the server's
	// goroutines while a case rewrites them from the test's, which -race
	// catches.
	mu     sync.RWMutex
	bodies map[string][]byte
	sha256 map[string]string
}

// newFixtureServer serves one artifact per component plus its publisher
// checksum document. signer signs runc's clearsigned checksum list.
func newFixtureServer(t *testing.T, signer *gpgKey) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{bodies: map[string][]byte{}, sha256: map[string]string{}}

	artifacts := map[string][]byte{
		fmt.Sprintf("/release/%s/bin/linux/amd64/kubeadm", fixtureVersion): []byte(fakeKubeadm()),
		fmt.Sprintf("/release/%s/bin/linux/amd64/kubectl", fixtureVersion): []byte("fake kubectl\n"),
		fmt.Sprintf("/release/%s/bin/linux/amd64/kubelet", fixtureVersion): []byte("fake kubelet\n"),
		fmt.Sprintf("/helm-%s-linux-amd64.tar.gz", fixtureHelm):            tarGz(t, map[string]string{"linux-amd64/helm": "fake helm\n"}),
		fmt.Sprintf("/containernetworking/plugins/releases/download/%s/cni-plugins-linux-amd64-%s.tgz", fixtureCNI, fixtureCNI): tarGz(t, map[string]string{
			"./bridge": "fake bridge\n", "./loopback": "fake loopback\n",
		}),
		fmt.Sprintf("/kubernetes-sigs/cri-tools/releases/download/%s/crictl-%s-linux-amd64.tar.gz", fixtureCriTools, fixtureCriTools): tarGz(t, map[string]string{
			"crictl": "fake crictl\n",
		}),
		fmt.Sprintf("/containerd/containerd/releases/download/v%s/containerd-%s-linux-amd64.tar.gz", fixtureContainerd, fixtureContainerd): tarGz(t, map[string]string{
			"bin/containerd": "fake containerd\n", "bin/containerd-shim-runc-v2": "fake shim\n", "bin/ctr": "fake ctr\n",
		}),
		fmt.Sprintf("/opencontainers/runc/releases/download/%s/runc.amd64", fixtureRunc): []byte("fake runc\n"),
	}
	for path, body := range artifacts {
		fs.bodies[path] = body
		fs.sha256[path] = fmt.Sprintf("%x", sha256.Sum256(body))
	}

	// dl.k8s.io, helm and the GitHub release assets publish a bare digest;
	// containerd publishes a GNU sha256sum list; runc's list is clearsigned.
	for path, digest := range fs.sha256 {
		if strings.Contains(path, "/containerd-") {
			fs.bodies[path+".sha256sum"] = []byte(fmt.Sprintf("%s  %s\n", digest, filepath.Base(path)))
			continue
		}
		if strings.HasSuffix(path, "/runc.amd64") {
			list := fmt.Sprintf("%s  runc.amd64\n%s  runc.arm64\n", digest, strings.Repeat("0", 64))
			fs.bodies[filepath.Dir(path)+"/runc.sha256sum"] = signer.clearsign(t, list)
			continue
		}
		fs.bodies[path+".sha256"] = []byte(digest + "\n")
	}

	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.requests.Add(1)
		fs.mu.RLock()
		body, ok := fs.bodies[r.URL.Path]
		fs.mu.RUnlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(fs.Close)
	return fs
}

// digestOf returns the sha256 the server advertises for one served artifact.
func (fs *fixtureServer) digestOf(path string) string {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.sha256[path]
}

// replaceArtifact rewrites one artifact and its bare `.sha256` document, so a
// case can make upstream move under a lock that was already written.
func (fs *fixtureServer) replaceArtifact(path string, body []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.bodies[path] = body
	fs.sha256[path] = fmt.Sprintf("%x", sha256.Sum256(body))
	fs.bodies[path+".sha256"] = []byte(fs.sha256[path] + "\n")
}

// serve replaces one served document outright, checksum shape and all.
func (fs *fixtureServer) serve(path string, body []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.bodies[path] = body
}

// helmPath, runcPath and runcChecksumPath are the fixture paths cases reach
// for by name.
func helmPath() string { return fmt.Sprintf("/helm-%s-linux-amd64.tar.gz", fixtureHelm) }

func runcPath() string {
	return fmt.Sprintf("/opencontainers/runc/releases/download/%s/runc.amd64", fixtureRunc)
}

func containerdPath() string {
	return fmt.Sprintf("/containerd/containerd/releases/download/v%s/containerd-%s-linux-amd64.tar.gz",
		fixtureContainerd, fixtureContainerd)
}

func runcChecksumPath() string {
	return fmt.Sprintf("/opencontainers/runc/releases/download/%s/runc.sha256sum", fixtureRunc)
}

// gpgKey is a throwaway signing key in its own GNUPGHOME.
type gpgKey struct {
	home string
	uid  string
}

// newGPGKey generates a throwaway key in its own GNUPGHOME.
//
// The home is deliberately not t.TempDir(). gpg-agent puts its socket inside
// GNUPGHOME, and a unix socket path is capped near 104 bytes; on macOS
// t.TempDir() is already ~70 characters of /var/folders/... plus the test name,
// which pushes the socket over the limit and gpg fails with
// "can't connect to the gpg-agent: IPC connect call failed".
func newGPGKey(t *testing.T, uid string) *gpgKey {
	t.Helper()
	home, err := os.MkdirTemp("", "nvkegpg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	require.NoError(t, os.Chmod(home, 0o700))
	k := &gpgKey{home: home, uid: uid}
	out, err := k.run(t, "--quick-generate-key", uid, "default", "default", "never")
	require.NoErrorf(t, err, "gpg --quick-generate-key: %s", out)
	t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run() })
	return k
}

func (k *gpgKey) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--homedir", k.home, "--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)
	out, err := exec.Command("gpg", full...).CombinedOutput()
	return string(out), err
}

// armoredPublicKey is the file bundle.sh imports in place of the committed
// packaging/keyrings/runc.keyring.
func (k *gpgKey) armoredPublicKey(t *testing.T) []byte {
	t.Helper()
	out, err := exec.Command("gpg", "--homedir", k.home, "--batch", "--armor", "--export", k.uid).Output()
	require.NoError(t, err)
	require.NotEmpty(t, out, "exported public key is empty")
	return out
}

// clearsign wraps text in a real OpenPGP clearsigned document.
func (k *gpgKey) clearsign(t *testing.T, text string) []byte {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "doc")
	out := filepath.Join(dir, "doc.asc")
	require.NoError(t, os.WriteFile(in, []byte(text), 0o600))
	msg, err := k.run(t, "--local-user", k.uid, "--output", out, "--clearsign", in)
	require.NoErrorf(t, err, "gpg --clearsign: %s", msg)
	signed, err := os.ReadFile(out)
	require.NoError(t, err)
	return signed
}

// writeFakeCrane writes a crane stand-in: `digest` answers from the fixture
// table, `pull` writes a small docker-archive.
func writeFakeCrane(t *testing.T, dir string) string {
	t.Helper()
	var digests strings.Builder
	for _, img := range fixtureImages {
		fmt.Fprintf(&digests, "%s %s\n", strings.SplitN(img.ref, ":", 2)[0], img.digest)
	}
	table := filepath.Join(dir, "digests")
	require.NoError(t, os.WriteFile(table, []byte(digests.String()), 0o644))

	crane := filepath.Join(dir, "crane")
	script := `#!/usr/bin/env bash
set -euo pipefail
table=` + fmt.Sprintf("%q", table) + `
case "$1" in
  digest)
    ref="${*: -1}"
    awk -v r="${ref%%:*}" '$1 == r { print $2 }' "$table"
    ;;
  pull)
    shift
    refs=()
    while (($#)); do
      case "$1" in --platform) shift 2 ;; *) refs+=("$1"); shift ;; esac
    done
    out="${refs[-1]}"
    unset 'refs[-1]'
    work="$(mktemp -d)"
    trap 'rm -rf "$work"' EXIT
    entries=""
    for ref in "${refs[@]}"; do
      repo="${ref%%@*}"
      # crane pulls by digest here, and names the image with the placeholder
      # tag it uses for digest references. bundle.sh has to put the real tag
      # back before ctr sees the archive.
      entries="${entries}${entries:+,}{\"Config\":\"config.json\",\"RepoTags\":[\"${repo}:i-was-a-digest\"],\"Layers\":[\"layer.tar.gz\"]}"
    done
    printf '[%s]' "$entries" >"$work/manifest.json"
    printf 'fake config' >"$work/config.json"
    printf 'fake layer' >"$work/layer.tar.gz"
    files=(manifest.json config.json layer.tar.gz)
    if [[ -n "${FAKE_CRANE_STRAY_PLACEHOLDER:-}" ]]; then
      printf '{"ref":"somewhere/else:i-was-a-digest"}' >"$work/stray.json"
      files+=(stray.json)
    fi
    if [[ -n "${FAKE_CRANE_REVERSE:-}" ]]; then
      reversed=()
      for ((i = ${#files[@]} - 1; i >= 0; i--)); do reversed+=("${files[i]}"); done
      files=("${reversed[@]}")
    fi
    tar -cf "$out" -C "$work" "${files[@]}"
    ;;
  *) echo "fake crane: unsupported subcommand $1" >&2; exit 1 ;;
esac
`
	require.NoError(t, os.WriteFile(crane, []byte(script), 0o755))
	return crane
}

// bundleRun is one invocation of bundle.sh against the fixtures.
type bundleRun struct {
	server  *fixtureServer
	crane   string
	keyring string
	dist    string
	lockDir string
	// env is added to bundle.sh's environment, so a case can change how the
	// fake crane behaves between two otherwise identical runs.
	env []string
}

// newBundleRun wires a fixture server, a fake crane and a keyring holding
// signer's public key into a scratch dist/lock pair.
func newBundleRun(t *testing.T, signer, keyringOwner *gpgKey) *bundleRun {
	t.Helper()
	dir := t.TempDir()
	keyring := filepath.Join(dir, "runc.keyring")
	require.NoError(t, os.WriteFile(keyring, keyringOwner.armoredPublicKey(t), 0o644))
	return &bundleRun{
		server:  newFixtureServer(t, signer),
		crane:   writeFakeCrane(t, dir),
		keyring: keyring,
		dist:    filepath.Join(dir, "dist"),
		lockDir: filepath.Join(dir, "lock"),
	}
}

// exec runs bundle.sh and returns its combined output.
func (b *bundleRun) exec(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{
		"--version", fixtureVersion,
		"--dist", b.dist,
		"--lock-dir", b.lockDir,
		"--crane", b.crane,
	}, args...)
	cmd := exec.Command(bundleScript, full...)
	cmd.Env = append(os.Environ(),
		"BUNDLE_DL_K8S_URL="+b.server.URL,
		"BUNDLE_GITHUB_URL="+b.server.URL,
		"BUNDLE_HELM_URL="+b.server.URL,
		"BUNDLE_RUNC_KEYRING="+b.keyring,
	)
	cmd.Env = append(cmd.Env, b.env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The lock is per architecture; these fixtures are amd64.
func (b *bundleRun) lockPath() string {
	return filepath.Join(b.lockDir, "amd64", fixtureVersion+".lock")
}

// writeLock puts a lock where bundle.sh will look for it, creating the
// architecture directory that os.WriteFile will not.
func (b *bundleRun) writeLock(t *testing.T, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(b.lockPath()), 0o755))
	require.NoError(t, os.WriteFile(b.lockPath(), []byte(body), 0o644))
}
func (b *bundleRun) archDir() string  { return filepath.Join(b.dist, fixtureVersion, "amd64") }
func (b *bundleRun) stageDir() string { return filepath.Join(b.dist, fixtureVersion, "stage") }

// wantLock is the lock text bundle.sh must produce from the fixtures.
func (b *bundleRun) wantLock() string {
	url := b.server.URL
	records := []string{
		fmt.Sprintf("artifact cni-plugins %s %s/containernetworking/plugins/releases/download/%s/cni-plugins-linux-amd64-%s.tgz",
			b.server.digestOf(fmt.Sprintf("/containernetworking/plugins/releases/download/%s/cni-plugins-linux-amd64-%s.tgz", fixtureCNI, fixtureCNI)), url, fixtureCNI, fixtureCNI),
		fmt.Sprintf("artifact containerd %s %s/containerd/containerd/releases/download/v%s/containerd-%s-linux-amd64.tar.gz",
			b.server.digestOf(fmt.Sprintf("/containerd/containerd/releases/download/v%s/containerd-%s-linux-amd64.tar.gz", fixtureContainerd, fixtureContainerd)), url, fixtureContainerd, fixtureContainerd),
		fmt.Sprintf("artifact crictl %s %s/kubernetes-sigs/cri-tools/releases/download/%s/crictl-%s-linux-amd64.tar.gz",
			b.server.digestOf(fmt.Sprintf("/kubernetes-sigs/cri-tools/releases/download/%s/crictl-%s-linux-amd64.tar.gz", fixtureCriTools, fixtureCriTools)), url, fixtureCriTools, fixtureCriTools),
		fmt.Sprintf("artifact helm %s %s/helm-%s-linux-amd64.tar.gz",
			b.server.digestOf(fmt.Sprintf("/helm-%s-linux-amd64.tar.gz", fixtureHelm)), url, fixtureHelm),
		fmt.Sprintf("artifact runc %s %s/opencontainers/runc/releases/download/%s/runc.amd64",
			b.server.digestOf(fmt.Sprintf("/opencontainers/runc/releases/download/%s/runc.amd64", fixtureRunc)), url, fixtureRunc),
	}
	for _, name := range []string{"kubeadm", "kubectl", "kubelet"} {
		path := fmt.Sprintf("/release/%s/bin/linux/amd64/%s", fixtureVersion, name)
		records = append(records, fmt.Sprintf("artifact %s %s %s%s", name, b.server.digestOf(path), url, path))
	}
	for _, img := range fixtureImages {
		records = append(records, fmt.Sprintf("image %s %s", img.ref, img.digest))
	}
	sort.Strings(records)
	header := fmt.Sprintf("# nke-kubeadm-distro lock for %s (amd64). Regenerate with `make lock`; review the diff before committing.\n"+
		"kubernetes %s\n", fixtureVersion, fixtureVersion)
	return header + strings.Join(records, "\n") + "\n"
}

// requireLinux skips the end-to-end cases on a host bundle.sh refuses to run
// on. The refusal itself is asserted by TestBundleRefusesNonLinux.
func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("bundle.sh refuses to run on %s", runtime.GOOS)
	}
	// The fixture server serves amd64 artifact paths, and bundle.sh runs the
	// downloaded kubeadm, so these cases need an amd64 host. The arm64 lock is
	// covered by building it, not by these fixtures.
	if runtime.GOARCH != "amd64" {
		t.Skipf("the fixtures are amd64; this host is %s", runtime.GOARCH)
	}
}

// TestBundleRejectsUnknownArch runs everywhere: the arch check comes before
// the Linux check, so it is the one refusal a developer laptop can exercise.
func TestBundleRejectsUnknownArch(t *testing.T) {
	requireBundleTools(t)
	b := newBundleRun(t, newGPGKey(t, "runc test <runc@example.invalid>"), newGPGKey(t, "runc test <runc@example.invalid>"))
	out, err := b.exec(t, "--arch", "riscv64")
	require.Error(t, err)
	assert.Contains(t, out, "expected amd64 or arm64")
}

// TestBundleRejectsForeignArch pins the refusal to cross-build: resolve_images
// runs the downloaded kubeadm, so the host must be the target architecture.
func TestBundleRejectsForeignArch(t *testing.T) {
	requireBundleTools(t)
	// Not requireLinux: this case is valid on an arm64 Linux host too, which
	// requireLinux skips because the fixtures are amd64.
	if runtime.GOOS != "linux" {
		t.Skipf("the Linux check fires first on %s", runtime.GOOS)
	}
	foreign := "arm64"
	if runtime.GOARCH == "arm64" {
		foreign = "amd64"
	}
	b := newBundleRun(t, newGPGKey(t, "runc test <runc@example.invalid>"), newGPGKey(t, "runc test <runc@example.invalid>"))
	out, err := b.exec(t, "--arch", foreign)
	require.Error(t, err)
	assert.Contains(t, out, "this host is")
}

// TestBundleRefusesNonLinux pins the early, clear failure on a host that
// cannot execute the downloaded kubeadm.
func TestBundleRefusesNonLinux(t *testing.T) {
	requireBundleTools(t)
	if runtime.GOOS == "linux" {
		t.Skip("this host is Linux")
	}
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	out, err := b.exec(t)
	require.Error(t, err)
	assert.Contains(t, out, "run on Linux")
}

// TestBundleWriteLock covers --write-lock: everything is resolved from
// upstream and the lock is written sorted, with no bundle staged.
func TestBundleWriteLock(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)

	out, err := b.exec(t, "--write-lock")
	require.NoErrorf(t, err, "bundle.sh --write-lock: %s", out)

	got, err := os.ReadFile(b.lockPath())
	require.NoError(t, err)
	assert.Equal(t, b.wantLock(), string(got))
	assert.Contains(t, out, "new lock file", "a first lock must be reported as new")
	assert.NoDirExists(t, b.stageDir(), "--write-lock must never stage a tree")
}

// TestBundleBuild covers the build path end to end against a matching lock.
func TestBundleBuild(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	b.writeLock(t, b.wantLock())

	out, err := b.exec(t)
	require.NoErrorf(t, err, "bundle.sh: %s", out)

	// The bare binaries stay where they landed; nfpm.yaml reads them there.
	for _, name := range []string{"kubeadm", "kubectl", "kubelet", "runc.amd64"} {
		info, err := os.Stat(filepath.Join(b.archDir(), name))
		require.NoError(t, err)
		assert.Equalf(t, os.FileMode(0o755), info.Mode().Perm(), "%s must be executable", name)
		assert.NoFileExists(t, filepath.Join(b.archDir(), name+".sha256"), "the bare binaries no longer carry sidecars")
	}

	stage := b.stageDir()
	for _, rel := range []string{
		"usr/local/bin/containerd", "usr/local/bin/containerd-shim-runc-v2", "usr/local/bin/ctr",
		"usr/bin/crictl", "usr/bin/helm", "opt/cni/bin/bridge", "opt/cni/bin/loopback",
		"etc/containerd/config.toml", "etc/crictl.yaml",
	} {
		assert.FileExists(t, filepath.Join(stage, rel))
	}

	config, err := os.ReadFile(filepath.Join(stage, "etc", "containerd", "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), "sandbox = 'registry.k8s.io/pause:3.10'")
	assert.Contains(t, string(config), "version = 3")
	assert.Contains(t, string(config), "imports = ['/etc/containerd/conf.d/*.toml']")

	crictl, err := os.ReadFile(filepath.Join(stage, "etc", "crictl.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "runtime-endpoint: unix:///run/containerd/containerd.sock\n"+
		"image-endpoint: unix:///run/containerd/containerd.sock\ntimeout: 10\n", string(crictl))

	// The image archive is a gzip of a plain docker-archive tar, and postinst
	// verifies it by running `sha256sum -c` on the sidecar in that directory.
	tgz := filepath.Join(b.archDir(), baseImagesTarball)
	require.FileExists(t, tgz)
	data, err := os.ReadFile(tgz)
	require.NoError(t, err)
	sidecar, err := os.ReadFile(tgz + ".sha256")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x  %s\n", sha256.Sum256(data), baseImagesTarball), string(sidecar))
	check := exec.Command("sha256sum", "-c", baseImagesTarball+".sha256")
	check.Dir = b.archDir()
	checkOut, err := check.CombinedOutput()
	require.NoErrorf(t, err, "sha256sum -c: %s", checkOut)

	// Every image has to import under the tag kubeadm asks for, since kubelet
	// looks the bundled images up by that tag and would otherwise go to the
	// registry.
	manifest := extractFromTgz(t, tgz, "manifest.json")
	for _, img := range fixtureImages {
		assert.Contains(t, manifest, fmt.Sprintf("%q", img.ref))
	}
	assert.NotContains(t, manifest, "i-was-a-digest")
}

// extractFromTgz returns one member of a gzipped tar as a string.
func extractFromTgz(t *testing.T, path, member string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		require.NoErrorf(t, err, "looking for %s in %s", member, path)
		if hdr.Name == member {
			body, err := io.ReadAll(tr)
			require.NoError(t, err)
			return string(body)
		}
	}
}

// TestBundleWarmCacheMakesNoRequests is what makes a second `make debs`
// offline: everything the lock pins is already on disk and byte-identical.
func TestBundleWarmCacheMakesNoRequests(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	b.writeLock(t, b.wantLock())

	out, err := b.exec(t)
	require.NoErrorf(t, err, "first build: %s", out)
	require.Positive(t, b.server.requests.Load(), "the first build must actually download")

	// The mode is not part of the digest, so a cache hit has to reassert it;
	// nfpm reads kubeadm straight from here and a 0644 one cannot run.
	kubeadm := filepath.Join(b.archDir(), "kubeadm")
	require.NoError(t, os.Chmod(kubeadm, 0o644))

	before := b.server.requests.Load()
	out, err = b.exec(t)
	require.NoErrorf(t, err, "second build: %s", out)
	assert.Equal(t, before, b.server.requests.Load(), "a warm-cache build must make no HTTP requests")

	info, err := os.Stat(kubeadm)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "a cache hit must reassert the executable bit")
}

// TestBundleRebuildsCorruptedImageArchive covers the third leg of the reuse
// gate: an archive whose bytes no longer match the sidecar has to be rebuilt.
func TestBundleRebuildsCorruptedImageArchive(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	b.writeLock(t, b.wantLock())

	out, err := b.exec(t)
	require.NoErrorf(t, err, "first build: %s", out)
	tgz := filepath.Join(b.archDir(), baseImagesTarball)
	good, err := os.ReadFile(tgz)
	require.NoError(t, err)

	// Same image list, same sidecar, different bytes: only re-hashing the
	// archive catches this.
	require.NoError(t, os.WriteFile(tgz, []byte("corrupted"), 0o644))
	out, err = b.exec(t)
	require.NoErrorf(t, err, "rebuild: %s", out)

	rebuilt, err := os.ReadFile(tgz)
	require.NoError(t, err)
	assert.Equal(t, good, rebuilt, "a corrupted archive must be rebuilt, not reused")
	check := exec.Command("sha256sum", "-c", baseImagesTarball+".sha256")
	check.Dir = b.archDir()
	checkOut, err := check.CombinedOutput()
	require.NoErrorf(t, err, "sha256sum -c: %s", checkOut)
}

// TestBundleRejectsArtifactDigestDrift covers a lock that no longer describes
// the bytes upstream serves: the download is refused and nothing is left in
// the artifact directory.
func TestBundleRejectsArtifactDigestDrift(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	lock := strings.Replace(b.wantLock(), b.server.digestOf(helmPath()), strings.Repeat("c", 64), 1)
	b.writeLock(t, lock)

	out, err := b.exec(t)
	require.Error(t, err, "a lock digest that does not match upstream must fail the build")
	assert.Contains(t, out, "upstream changed the bytes behind")
	assert.NoFileExists(t, filepath.Join(b.archDir(), fmt.Sprintf("helm-%s-linux-amd64.tar.gz", fixtureHelm)))
}

// TestBundleRejectsImageDrift covers kubeadm asking for an image the lock
// does not pin at that digest.
func TestBundleRejectsImageDrift(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	lock := strings.Replace(b.wantLock(), fixtureImages[0].digest, "sha256:"+strings.Repeat("d", 64), 1)
	b.writeLock(t, lock)

	out, err := b.exec(t)
	require.Error(t, err, "an image digest that differs from the lock must fail the build")
	assert.Contains(t, out, "run make lock and review the diff")
	assert.NoFileExists(t, filepath.Join(b.archDir(), baseImagesTarball))
}

// TestBundleRejectsUnknownRuncSigner is the point of shipping the keyring at
// all: a valid signature from a key that is not a runc release key must not
// verify.
func TestBundleRejectsUnknownRuncSigner(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "impostor <impostor@example.invalid>")
	release := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, release)

	out, err := b.exec(t, "--write-lock")
	require.Error(t, err, "a checksum list signed by an unknown key must be refused")
	assert.Contains(t, out, "runc checksum")
	assert.NoFileExists(t, b.lockPath())
}

// TestBundleWriteLockReportsWhatChanged covers the summary `make lock` prints
// for a reviewer. `diff | grep` cannot be tested by its exit status under
// pipefail, so a regression here reads "(unchanged)" over a real change.
func TestBundleWriteLockReportsWhatChanged(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)

	out, err := b.exec(t, "--write-lock")
	require.NoErrorf(t, err, "first --write-lock: %s", out)
	assert.Contains(t, out, "new lock file")

	// Nothing moved upstream, so the second run has nothing to report.
	out, err = b.exec(t, "--write-lock")
	require.NoErrorf(t, err, "unchanged --write-lock: %s", out)
	assert.Contains(t, out, "(unchanged)")

	// Now move helm's bytes and re-lock: the summary must name the change and
	// must not claim the lock is unchanged.
	before := b.server.digestOf(helmPath())
	b.server.replaceArtifact(helmPath(), tarGz(t, map[string]string{"linux-amd64/helm": "fake helm, rebuilt\n"}))
	after := b.server.digestOf(helmPath())
	require.NotEqual(t, before, after)

	out, err = b.exec(t, "--write-lock")
	require.NoErrorf(t, err, "changed --write-lock: %s", out)
	assert.NotContains(t, out, "(unchanged)", "a real change must not be reported as unchanged")
	assert.Contains(t, out, "< artifact helm "+before)
	assert.Contains(t, out, "> artifact helm "+after)

	got, err := os.ReadFile(b.lockPath())
	require.NoError(t, err)
	assert.Equal(t, b.wantLock(), string(got))
}

// TestBundleRepackIsOrderIndependent is what makes the .deb reproducible:
// crane writes the archive's members in Go map order, so two pulls of the
// same images have to come out byte-identical anyway.
func TestBundleRepackIsOrderIndependent(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	b.writeLock(t, b.wantLock())

	out, err := b.exec(t)
	require.NoErrorf(t, err, "first build: %s", out)
	first, err := os.ReadFile(filepath.Join(b.archDir(), baseImagesTarball))
	require.NoError(t, err)

	// A second, fresh dist, with crane writing its members the other way round
	// and under a different umask: the two things a repack that does not pin
	// member order and mode would let leak into the bytes.
	b.dist = filepath.Join(t.TempDir(), "dist2")
	b.env = []string{"FAKE_CRANE_REVERSE=1"}
	// Process-wide, and restored below. Nothing in this package calls
	// t.Parallel(), so no other case can observe the changed umask.
	previous := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(previous) })
	out, err = b.exec(t)
	require.NoErrorf(t, err, "second build: %s", out)
	second, err := os.ReadFile(filepath.Join(b.archDir(), baseImagesTarball))
	require.NoError(t, err)

	assert.Equal(t, first, second, "the repacked archive must not depend on crane's member order")
}

// TestBundleRejectsSurvivingPlaceholder covers the post-condition on the tag
// rewrite: crane's digest placeholder must not reach the node anywhere, not
// just in the manifest entries this script rewrites.
func TestBundleRejectsSurvivingPlaceholder(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	b.writeLock(t, b.wantLock())
	b.env = []string{"FAKE_CRANE_STRAY_PLACEHOLDER=1"}

	out, err := b.exec(t)
	require.Error(t, err, "a surviving placeholder must fail the build")
	assert.Contains(t, out, "placeholder survived")
	assert.NoFileExists(t, filepath.Join(b.archDir(), baseImagesTarball))
}

// TestBundleRejectsUnexpectedClearsignedChecksum guards the one component
// whose checksum list is signed: a signed document anywhere else is a change
// of shape, and parsing it as plain text would skip the signature entirely.
func TestBundleRejectsUnexpectedClearsignedChecksum(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	b.server.serve(helmPath()+".sha256", signer.clearsign(t, b.server.digestOf(helmPath())+"\n"))

	out, err := b.exec(t, "--write-lock")
	require.Error(t, err, "a clearsigned checksum for helm must be refused")
	assert.Contains(t, out, "unexpected clearsigned checksum for helm")
	assert.NoFileExists(t, b.lockPath())
}

// TestBundleRejectsUnexpectedClearsignedContainerd is the same guard on
// containerd's `.sha256sum` arm, which has its own checksum-document shape.
func TestBundleRejectsUnexpectedClearsignedContainerd(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	list := fmt.Sprintf("%s  %s\n", b.server.digestOf(containerdPath()), filepath.Base(containerdPath()))
	b.server.serve(containerdPath()+".sha256sum", signer.clearsign(t, list))

	out, err := b.exec(t, "--write-lock")
	require.Error(t, err, "a clearsigned checksum for containerd must be refused")
	assert.Contains(t, out, "unexpected clearsigned checksum for containerd")
	assert.NoFileExists(t, b.lockPath())
}

// TestBundleRejectsConflictingChecksumEntries covers a checksum document that
// names the artifact twice with different digests. `exit` from an awk main
// rule still runs END, so the refusal has to be carried into END as a flag.
func TestBundleRejectsConflictingChecksumEntries(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	name := filepath.Base(helmPath())
	b.server.serve(helmPath()+".sha256", []byte(fmt.Sprintf("%s  %s\n%s  %s\n",
		b.server.digestOf(helmPath()), name, strings.Repeat("f", 64), name)))

	out, err := b.exec(t, "--write-lock")
	require.Error(t, err, "two disagreeing entries for one basename must be refused")
	assert.Contains(t, out, "no unambiguous SHA-256 digest")
	assert.NoFileExists(t, b.lockPath())
}

// TestBundleRejectsNestedCNIMember covers the archive shape check: the CNI
// plugins land straight in /opt/cni/bin, so a nested path in that tarball
// would be extracted somewhere nothing looks.
func TestBundleRejectsNestedCNIMember(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	cni := fmt.Sprintf("/containernetworking/plugins/releases/download/%s/cni-plugins-linux-amd64-%s.tgz",
		fixtureCNI, fixtureCNI)
	b.server.replaceArtifact(cni, tarGz(t, map[string]string{"nested/dir/bridge": "fake bridge\n"}))
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	b.writeLock(t, b.wantLock())

	out, err := b.exec(t)
	require.Error(t, err, "a nested member in the CNI archive must be refused")
	assert.Contains(t, out, "is not a flat top-level file")
}

// TestBundleRejectsUnsignedRuncChecksums covers the downgrade runc's signed
// list exists to prevent.
func TestBundleRejectsUnsignedRuncChecksums(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	b.server.serve(runcChecksumPath(), []byte(b.server.digestOf(runcPath())+"  runc.amd64\n"))

	out, err := b.exec(t, "--write-lock")
	require.Error(t, err, "an unsigned runc checksum list must be refused")
	assert.Contains(t, out, "not OpenPGP clearsigned")
	assert.NoFileExists(t, b.lockPath())
}

// TestBundleIgnoresAmbientGNUPGHOME is the reason bundle.sh imports into a
// scratch homedir rather than passing --keyring: a key the build host happens
// to trust must not be able to sign runc's checksums.
func TestBundleIgnoresAmbientGNUPGHOME(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	impostor := newGPGKey(t, "impostor <impostor@example.invalid>")
	release := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, impostor, release)
	// The impostor's own keys, as the build host's default keyring.
	b.env = []string{"GNUPGHOME=" + impostor.home}

	out, err := b.exec(t, "--write-lock")
	require.Error(t, err, "a signer trusted only by the ambient GNUPGHOME must be refused")
	assert.Contains(t, out, "runc checksum")
	assert.NoFileExists(t, b.lockPath())
}

// TestBundleRejectsMismatchedLockVersion covers the `kubernetes` record: a
// lock copied or renamed to the wrong filename is not the file that was
// reviewed for this release.
func TestBundleRejectsMismatchedLockVersion(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	lock := strings.Replace(b.wantLock(), "kubernetes "+fixtureVersion, "kubernetes v1.35.3", 1)
	b.writeLock(t, lock)

	out, err := b.exec(t)
	require.Error(t, err, "a lock for another version must be refused")
	assert.Contains(t, out, "locks 'v1.35.3', not "+fixtureVersion)
	assert.NoDirExists(t, b.archDir(), "the check must run before anything is created on disk")
}

// TestBundleRejectsExtraLockArtifact covers the artifact comparison running
// both ways: a row for something this build never downloaded is not the file
// that was reviewed.
func TestBundleRejectsExtraLockArtifact(t *testing.T) {
	requireBundleTools(t)
	requireLinux(t)
	signer := newGPGKey(t, "runc test <runc@example.invalid>")
	b := newBundleRun(t, signer, signer)
	require.NoError(t, os.MkdirAll(b.lockDir, 0o755))
	lock := b.wantLock() + "artifact zzz-extra " + strings.Repeat("e", 64) + " https://example.invalid/zzz\n"
	b.writeLock(t, lock)

	out, err := b.exec(t)
	require.Error(t, err, "an artifact row nothing downloaded must fail the build")
	assert.Contains(t, out, "the artifacts do not match")
	assert.NoFileExists(t, filepath.Join(b.archDir(), baseImagesTarball))
}
