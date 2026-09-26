// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra"
)

const nodeKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: site
  cluster:
    server: https://10.0.0.5:6443
    certificate-authority-data: Zm9v
contexts:
- name: admin@site
  context:
    cluster: site
    user: admin
current-context: admin@site
users:
- name: admin
  user:
    token: t
`

type put struct {
	data []byte
	mode uint32
}

type fakeSession struct {
	mu       sync.Mutex
	runs     []string
	puts     map[string]put
	fetch    map[string][]byte
	forwards []string
	fail     func(cmd string) (string, error)
}

func newFakeSession() *fakeSession {
	return &fakeSession{
		puts:  map[string]put{},
		fetch: map[string][]byte{kubeconfigPath: []byte(nodeKubeconfig)},
	}
}

func (f *fakeSession) Run(_ context.Context, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, cmd)
	if f.fail != nil {
		return f.fail(cmd)
	}
	return "", nil
}

func (f *fakeSession) Put(_ context.Context, p string, data []byte, mode uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts[p] = put{data: data, mode: mode}
	return nil
}

func (f *fakeSession) Fetch(_ context.Context, p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.fetch[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (f *fakeSession) Forward(_ context.Context, remote string) (string, error) {
	f.forwards = append(f.forwards, remote)
	return "127.0.0.1:40000", nil
}

func (f *fakeSession) Teardown(context.Context) error { return nil }

func TestCollectDiagnostics(t *testing.T) {
	s := newFakeSession()
	s.fetch[diagnosticsArchive] = []byte("tarball")
	dest := filepath.Join(t.TempDir(), "site-diagnostics.tar.gz")

	if err := CollectDiagnostics(context.Background(), s, dest); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "tarball" {
		t.Errorf("wrote %q, %v", got, err)
	}
	// Archived from the parent so the tarball is not inside what it archives.
	if i := s.ran("tar -czf " + diagnosticsArchive + " -C " + workDir + " diagnostics"); i < 0 {
		t.Errorf("did not archive the diagnostics: %v", s.runs)
	}
}

// The archive is uploaded as a CI artifact, so what the run put on the node
// must not be in the file that lands on disk.
func TestCollectDiagnosticsRedacts(t *testing.T) {
	s := newFakeSession()
	s.fetch[diagnosticsArchive] = tarGz(t, map[string]string{
		"diagnostics/journal.log": "authenticating with " + testForgeToken + "\n",
	})
	dest := filepath.Join(t.TempDir(), "site-diagnostics.tar.gz")

	if err := CollectDiagnostics(context.Background(), s, dest, testForgeToken); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	// Against the file, not the tar member: a secret that survived anywhere
	// in the bytes on disk is a secret that shipped.
	if bytes.Contains(got, []byte(testForgeToken)) {
		t.Error("the written archive still carries the token")
	}
	if body := readTarGz(t, got)["diagnostics/journal.log"]; !strings.Contains(body, redactedMarker) {
		t.Errorf("the journal was not redacted: %q", body)
	}
}

// A run that failed before nkectl wrote anything has no diagnostics
// directory, and that must be reported rather than leaving an empty file
// where the real ones would be.
func TestCollectDiagnosticsWithoutADirectory(t *testing.T) {
	s := newFakeSession()
	// fail is the Run hook; a nil error here is the point. This is what the
	// remote `if test -d ...; else echo ...; fi` prints when nkectl wrote
	// nothing, which is what a deploy that worked leaves behind.
	s.fail = func(string) (string, error) { return noDiagnosticsMarker + "\n", nil }
	dest := filepath.Join(t.TempDir(), "site-diagnostics.tar.gz")

	if err := CollectDiagnostics(context.Background(), s, dest); !errors.Is(err, ErrNoDiagnostics) {
		t.Fatalf("an absent directory should be ErrNoDiagnostics, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("wrote a file anyway: %v", err)
	}
}

// A directory that exists but cannot be archived is a different finding: the
// diagnostics are there and are being lost, which is worth an error.
func TestCollectDiagnosticsWhenArchivingFails(t *testing.T) {
	s := newFakeSession()
	s.fail = func(string) (string, error) { return "tar: permission denied", errors.New("exit status 2") }
	dest := filepath.Join(t.TempDir(), "site-diagnostics.tar.gz")

	err := CollectDiagnostics(context.Background(), s, dest)
	if err == nil || errors.Is(err, ErrNoDiagnostics) {
		t.Fatalf("a failed archive should be an error, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("wrote a file anyway: %v", err)
	}
}

// unquoted strips the shell quoting from a command, so a test can match the
// words it runs.
var unquoted = strings.NewReplacer(`'\''`, "", "'", "").Replace

// ran returns the index of the first command containing substr, ignoring
// quoting.
func (f *fakeSession) ran(substr string) int {
	for i, cmd := range f.runs {
		if strings.Contains(unquoted(cmd), substr) {
			return i
		}
	}
	return -1
}

// cmd is the first command containing substr, without its quoting.
func (f *fakeSession) cmd(t *testing.T, substr string) string {
	t.Helper()
	i := f.ran(substr)
	if i < 0 {
		t.Fatalf("no command contains %q:\n%s", substr, strings.Join(f.runs, "\n"))
	}
	return unquoted(f.runs[i])
}

type fakeDeployer struct {
	called bool
	target DeployTarget
	err    error
}

func (d *fakeDeployer) Deploy(_ context.Context, _ infra.Session, t DeployTarget) error {
	d.called = true
	d.target = t
	return d.err
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testCandidate(t *testing.T, d Deployer) Candidate {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "nkectl"), "binary")
	writeFile(t, filepath.Join(dir, "cwe.tar"), "image")
	checkout := filepath.Join(dir, "checkout")
	writeFile(t, filepath.Join(checkout, "build", "use-nke-sbom.sh"), "#!/bin/bash")
	writeFile(t, filepath.Join(checkout, "controller", "deploy", "nke-sbom", "release.yaml"), "items: []")
	return Candidate{
		SiteName:          "e2e-site",
		Nkectl:            filepath.Join(dir, "nkectl"),
		Assets:            checkout,
		AssetPaths:        []string{"build", "controller/deploy"},
		Images:            []string{filepath.Join(dir, "cwe.tar")},
		KubernetesVersion: "1.32.0",
		CNI:               "calico",
		DisableMTLS:       true,
		BootstrapEnv:      map[string]string{"NKECTL_CWE_SPIRE_CHART": "{assets}/controller/deploy/charts/spire"},
		ControllerVersion: "v1.2.3",
		Deploy:            d,
	}
}

var testNode = infra.Node{Name: "e2e-site-0", IP: "10.0.0.5", TPMEkThumbprint: "abc123"}

func testOptions(t *testing.T, probe Prober) Options {
	t.Helper()
	return Options{Probe: probe, OutputDir: t.TempDir(), ReadyTimeout: time.Second, PollInterval: time.Millisecond}
}

func versionProbe(v string) Prober {
	return func(context.Context, string) (string, error) { return v, nil }
}

func TestBootstrap(t *testing.T) {
	s := newFakeSession()
	d := &fakeDeployer{}
	res, err := Bootstrap(context.Background(), testNode, s, testCandidate(t, d), testOptions(t, versionProbe("v1.2.3")))
	if err != nil {
		t.Fatal(err)
	}

	if got := s.puts[nkectlPath]; string(got.data) != "binary" || got.mode != 0o755 {
		t.Errorf("nkectl staged as %q mode %o", got.data, got.mode)
	}
	if got := string(s.puts[hostsCSV].data); got != "name,ip,tpm_ek_thumbprint\ne2e-site-0,10.0.0.5,abc123\n" {
		t.Errorf("hosts.csv = %q", got)
	}
	if _, ok := s.puts[filepath.Join(imagesDir, "cwe.tar")]; !ok {
		t.Error("image archive not staged")
	}

	order := []string{
		"nkectl version",
		"tar -C",
		"ctr -n k8s.io images import",
		"generate sitespec e2e-site",
		"bootstrap site",
	}
	last := -1
	for _, want := range order {
		i := s.ran(want)
		if i <= last {
			t.Fatalf("command %q ran at %d, want after %d:\n%s", want, i, last, strings.Join(s.runs, "\n"))
		}
		last = i
	}

	gen := s.cmd(t, "generate sitespec")
	for _, want := range []string{"cluster.k8sVersion=1.32.0", "cluster.cniName=calico", "spire.trustDomain=e2e-site.nke.local"} {
		if !strings.Contains(gen, want) {
			t.Errorf("generate sitespec lacks %s: %s", want, gen)
		}
	}
	boot := s.cmd(t, "bootstrap site")
	for _, want := range []string{"sudo -n -i sh -c ", "--disable-mtls", "--kubeconfig-out", "NKECTL_CWE_SPIRE_CHART=", "/var/lib/nke-e2e/assets/controller/deploy/charts/spire"} {
		if !strings.Contains(boot, want) {
			t.Errorf("bootstrap site lacks %s: %s", want, boot)
		}
	}
	if !strings.Contains(s.cmd(t, "ctr -n k8s.io"), "ctr -n default images import") {
		t.Error("image not imported into the default namespace")
	}

	if !d.called || d.target.Kubeconfig != kubeconfigPath || d.target.Assets != assetsDir ||
		d.target.TrustDomain != "e2e-site.nke.local" || d.target.Node != testNode {
		t.Errorf("deployer target = %+v", d.target)
	}

	// Loopback, because the site's own API server runs on the node this
	// session is connected to. A tenant's does not; see sessionRelay.
	if len(s.forwards) != 1 || s.forwards[0] != "127.0.0.1:6443" {
		t.Errorf("forwards = %v, want [127.0.0.1:6443]", s.forwards)
	}
	cfg, err := clientcmd.LoadFromFile(res.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cl := cfg.Clusters["site"]
	if cl.Server != "https://127.0.0.1:40000" || cl.TLSServerName != "10.0.0.5" {
		t.Errorf("rewritten cluster = %s (tls-server-name %q)", cl.Server, cl.TLSServerName)
	}
	if info, err := os.Stat(res.Kubeconfig); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig mode = %v, %v", info.Mode(), err)
	}
	if res.Version != "v1.2.3" {
		t.Errorf("version = %q", res.Version)
	}
}

func TestBootstrapVersionMismatch(t *testing.T) {
	_, err := Bootstrap(context.Background(), testNode, newFakeSession(), testCandidate(t, &fakeDeployer{}),
		testOptions(t, versionProbe("v9.9.9")))
	if err == nil || !strings.Contains(err.Error(), `"v9.9.9", want "v1.2.3"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestBootstrapStepFailureStopsAndReports(t *testing.T) {
	s := newFakeSession()
	s.fail = func(cmd string) (string, error) {
		if strings.Contains(unquoted(cmd), "bootstrap site") {
			return "kubeadm init failed: port 6443 in use", errors.New("exit status 1")
		}
		return "", nil
	}
	d := &fakeDeployer{}
	_, err := Bootstrap(context.Background(), testNode, s, testCandidate(t, d), testOptions(t, versionProbe("v1.2.3")))
	if err == nil || !strings.Contains(err.Error(), "bootstrap site") || !strings.Contains(err.Error(), "port 6443 in use") {
		t.Fatalf("err = %v", err)
	}
	if d.called {
		t.Error("deployer ran after a failed bootstrap")
	}
}

func TestBootstrapDeployFailure(t *testing.T) {
	d := &fakeDeployer{err: errors.New("helm: timed out")}
	_, err := Bootstrap(context.Background(), testNode, newFakeSession(), testCandidate(t, d), testOptions(t, versionProbe("v1.2.3")))
	if err == nil || !strings.Contains(err.Error(), "deploy: helm: timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestBootstrapWaitsForCWE(t *testing.T) {
	calls := 0
	probe := func(context.Context, string) (string, error) {
		calls++
		if calls < 3 {
			return "", errUnhealthy
		}
		return "v1.2.3", nil
	}
	if _, err := Bootstrap(context.Background(), testNode, newFakeSession(), testCandidate(t, &fakeDeployer{}),
		testOptions(t, probe)); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("probed %d times, want 3", calls)
	}
}

func TestBootstrapCWENeverHealthy(t *testing.T) {
	opts := testOptions(t, func(context.Context, string) (string, error) { return "", errUnhealthy })
	opts.ReadyTimeout = 20 * time.Millisecond
	_, err := Bootstrap(context.Background(), testNode, newFakeSession(), testCandidate(t, &fakeDeployer{}), opts)
	if !errors.Is(err, errUnhealthy) || !strings.Contains(err.Error(), "did not become healthy") {
		t.Fatalf("err = %v", err)
	}
}

func TestCandidateValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Candidate){
		"site name":   func(c *Candidate) { c.SiteName = "Bad_Name" },
		"v prefix":    func(c *Candidate) { c.KubernetesVersion = "v1.32.0" },
		"no deployer": func(c *Candidate) { c.Deploy = nil },
		"no nkectl":   func(c *Candidate) { c.Nkectl = "/does/not/exist" },
		"escape":      func(c *Candidate) { c.AssetPaths = []string{"../outside"} },
		"no CNI":      func(c *Candidate) { c.CNI = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := testCandidate(t, &fakeDeployer{})
			mutate(&c)
			s := newFakeSession()
			if _, err := Bootstrap(context.Background(), testNode, s, c, testOptions(t, versionProbe(""))); err == nil {
				t.Fatal("expected a validation error")
			}
			if len(s.runs) != 0 || len(s.puts) != 0 {
				t.Error("touched the node before validating")
			}
		})
	}
}

func TestArchiveAssets(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "build", "use-nke-sbom.sh"), "script")
	writeFile(t, filepath.Join(root, "build", "._use-nke-sbom.sh"), "appledouble")
	writeFile(t, filepath.Join(root, "controller", "deploy", "nke-sbom", "release.yaml"), "items")
	writeFile(t, filepath.Join(root, "controller", "deploy", ".git", "HEAD"), "ref")
	writeFile(t, filepath.Join(root, "controller", "deploy", ".DS_Store"), "junk")
	writeFile(t, filepath.Join(root, "controller", "main.go"), "not staged")

	data, err := archiveAssets(root, []string{"build", "controller/deploy"})
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var files []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			files = append(files, hdr.Name)
		}
		if hdr.Uid != 0 || hdr.Uname != "" {
			t.Errorf("%s keeps the runner's owner", hdr.Name)
		}
	}
	sort.Strings(files)
	want := []string{"build/use-nke-sbom.sh", "controller/deploy/nke-sbom/release.yaml"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Errorf("archived %v, want %v", files, want)
	}
}

func TestRewriteServerKeepsExplicitServerName(t *testing.T) {
	raw := strings.Replace(nodeKubeconfig, "    certificate-authority-data: Zm9v\n",
		"    certificate-authority-data: Zm9v\n    tls-server-name: kubernetes\n", 1)
	out, err := rewriteServer([]byte(raw), "127.0.0.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if cl := cfg.Clusters["site"]; cl.TLSServerName != "kubernetes" || cl.Server != "https://127.0.0.1:1234" {
		t.Errorf("cluster = %+v", cl)
	}
}
