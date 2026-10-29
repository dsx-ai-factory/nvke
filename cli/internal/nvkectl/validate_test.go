// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

// generatedSiteSpec writes what `generate sitespec` produces, so the tests
// start from the file an operator holds.
func generatedSiteSpec(t *testing.T) string {
	t.Helper()
	stdout, _, err := runGenerate(t, "generate", "sitespec", "my-tier0-site", "--nodes", writeCSV(t, threeHostCSV), "--set", threeNodeVIPSet)
	if err != nil {
		t.Fatalf("generate sitespec: %v", err)
	}
	path := filepath.Join(t.TempDir(), "site", "sitespec.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// siteSpecWithTrustDomain is the generated site spec with spire.trustDomain
// set, the way a bootstrapped site's spec reads.
func siteSpecWithTrustDomain(t *testing.T) string {
	t.Helper()
	path := generatedSiteSpec(t)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body) + "spire:\n  trustDomain: my-tier0-site.nke.local\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateSiteSpec_GeneratedFile(t *testing.T) {
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", generatedSiteSpec(t))
	if err != nil {
		t.Fatalf("a generated site spec did not validate: %v\n%s", err, stdout)
	}
	// generate sitespec leaves the spire block commented out; the default
	// trust domain is reported, not required.
	if !strings.Contains(stdout, "spire.trustDomain: not set; a bootstrap with the default cluster name and the controller stack install both use nvkectl-bootstrap.nke.local") {
		t.Errorf("default trust domain not reported:\n%s", stdout)
	}
	if !strings.HasSuffix(stdout, "valid\n") {
		t.Errorf("the output does not end with valid:\n%s", stdout)
	}
}

func TestValidateSiteSpec_WithTrustDomain(t *testing.T) {
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", siteSpecWithTrustDomain(t))
	if err != nil || stdout != "valid\n" {
		t.Errorf("site spec with a trust domain: %q %v", stdout, err)
	}
}

// The SPIRE CA from the environment is the one reference the spec has, and
// validate reads it the way bootstrap site does.
func TestValidateSiteSpec_SpireCA(t *testing.T) {
	path := siteSpecWithTrustDomain(t)
	body, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(string(body)+"  upstreamCA: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", path)
	if err == nil || !strings.Contains(err.Error(), sitespec.EnvSpireCACertFile) || strings.Contains(stdout, "valid") {
		t.Errorf("upstreamCA without the variables: %q %v", stdout, err)
	}
	dir := t.TempDir()
	for _, name := range []string{"ca.crt", "ca.key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(sitespec.EnvSpireCACertFile, filepath.Join(dir, "ca.crt"))
	t.Setenv(sitespec.EnvSpireCAKeyFile, filepath.Join(dir, "ca.key"))
	if stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", path); err != nil || stdout != "valid\n" {
		t.Errorf("upstreamCA with the variables: %q %v", stdout, err)
	}
}

func TestValidateSiteSpec_Problems(t *testing.T) {
	path := siteSpecWithTrustDomain(t)
	body, _ := os.ReadFile(path)
	text := strings.Replace(string(body), `k8sVersion: "`, `k8sVersion: "v`, 1)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runGenerate(t, "validate", "sitespec", "-f", path)
	if err == nil || !strings.Contains(err.Error(), "k8sVersion") || strings.Contains(stdout, "valid") {
		t.Errorf("k8sVersion error: %q %v", stdout, err)
	}
	// An unknown field is refused as one, whatever its name. The loader
	// reports the key lowercased.
	text = string(body) + "pullSecret: x\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runGenerate(t, "validate", "sitespec", "-f", path)
	if err == nil || !strings.Contains(err.Error(), "invalid keys: pullsecret") || strings.Contains(stdout, "valid") {
		t.Errorf("unknown field: %q %v", stdout, err)
	}
	if _, _, err := runGenerate(t, "validate", "sitespec"); err == nil {
		t.Error("missing -f accepted")
	}
}

// TestValidateSiteSpec_NicoNeedsTheSpireAddressUnlessMTLSIsOff: validate applies
// the check bootstrap site applies, so a file it passes is one that command
// accepts, and --disable-mtls lifts it in both.
func TestValidateSiteSpec_NicoNeedsTheSpireAddressUnlessMTLSIsOff(t *testing.T) {
	example, err := os.ReadFile("../../examples/sitespec-nico.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	text := strings.Replace(string(example), `  externalAddress: "192.0.2.10"`, "", 1)
	if text == string(example) {
		t.Fatal("the example no longer sets spire.externalAddress; update this test")
	}
	path := filepath.Join(dir, "sitespec.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nico.token"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("example-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NVKECTL_NICO_CREDENTIALS", filepath.Join(dir, "nico.token"))
	t.Setenv("NVKECTL_NODE_USER", "example-user")
	t.Setenv("NVKECTL_NODE_PASSWORD", "example-password")
	if err := os.Mkdir(filepath.Join(dir, "bootstrap-releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	const releases = "apiVersion: internal.nvke.nvidia.com/v1\nkind: MachineImage\nmetadata:\n  name: img\n---\napiVersion: internal.nvke.nvidia.com/v1\nkind: ClusterRelease\nmetadata:\n  name: example-ubuntu-24.04-amd64\nspec:\n  machineImageRef:\n    name: img\n"
	if err := os.WriteFile(filepath.Join(dir, "bootstrap-releases", "r.yaml"), []byte(releases), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err = runValidateSiteSpec(&out, &validateSiteSpecOptions{siteSpec: path, bootstrapOnly: true})
	if err == nil || !strings.Contains(err.Error(), "spire.externalAddress is required") || out.Len() != 0 {
		t.Fatalf("with mTLS: err = %v, stdout %q; want the address refused in the error and nothing on stdout", err, out.String())
	}
	out.Reset()
	if err := runValidateSiteSpec(&out, &validateSiteSpecOptions{siteSpec: path, bootstrapOnly: true, disableMTLS: true}); err != nil {
		t.Fatalf("with --disable-mtls: %v\n%s", err, out.String())
	}
}

// TestValidateSiteSpec_NicoOneNodeWithoutVIP: a nico site spec needs its
// control-plane VIP even with one node, so both checks refuse it.
func TestValidateSiteSpec_NicoOneNodeWithoutVIP(t *testing.T) {
	path := nicoStackSiteSpec(t, oneNicoNodeNoVIP(t))
	for _, bootstrapOnly := range []bool{false, true} {
		err := runValidateSiteSpec(io.Discard, &validateSiteSpecOptions{siteSpec: path, bootstrapOnly: bootstrapOnly})
		if err == nil || !strings.Contains(err.Error(), "cluster.controlPlaneVIP is required") {
			t.Errorf("bootstrapOnly=%v: err = %v, want cluster.controlPlaneVIP is required", bootstrapOnly, err)
		}
	}
}

// TestValidateSiteSpec_ReportsBootstrapProblemsTogether: a missing SPIRE address
// does not hide what else only the bootstrap host holds, so one run lists both,
// and --disable-mtls is offered for the address alone.
func TestValidateSiteSpec_ReportsBootstrapProblemsTogether(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "sitespec-nico.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(example), `  externalAddress: "192.0.2.10"`, "", 1)
	if text == string(example) {
		t.Fatal("the example no longer sets spire.externalAddress; update this test")
	}
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NVKECTL_NICO_CREDENTIALS", "")

	var out strings.Builder
	err = runValidateSiteSpec(&out, &validateSiteSpecOptions{siteSpec: path, bootstrapOnly: true})
	if err == nil {
		t.Fatalf("--bootstrap-only passed:\n%s", out.String())
	}
	if out.Len() != 0 {
		t.Errorf("a failing run wrote to stdout: %q", out.String())
	}
	for _, want := range []string{"SPIRE server (--disable-mtls lifts this)", "NVKECTL_NICO_CREDENTIALS must name", "bootstrap-releases"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

// TestValidateSiteSpec_NicoExampleWithTheDocumentedReleases: cli/docs/sitespec.md
// has the operator write the request documents in releases/ as full manifests
// into bootstrap-releases/, which the nico example names. Done to the example
// request documents, that passes the bootstrap check and leaves releases/ as it
// was; releases/ itself is refused before any Kind cluster exists.
func TestValidateSiteSpec_NicoExampleWithTheDocumentedReleases(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	example, err := os.ReadFile(filepath.Join(examples, "sitespec-nico.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sitespec.yaml")
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nico.token"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("example-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NVKECTL_NICO_CREDENTIALS", filepath.Join(dir, "nico.token"))
	t.Setenv("NVKECTL_NODE_USER", "example-user")
	t.Setenv("NVKECTL_NODE_PASSWORD", "example-password")

	// releases/ as docs/operate/build-node-images.md has the operator build it.
	for _, sub := range []string{"releases", "bootstrap-releases"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	requests := map[string]string{}
	for _, name := range []string{"machineimage.yaml", "clusterrelease.yaml", "nodepoolrelease.yaml"} {
		data, err := os.ReadFile(filepath.Join(examples, name))
		if err != nil {
			t.Fatal(err)
		}
		requests[name] = string(data)
		if err := os.WriteFile(filepath.Join(dir, "releases", name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The documented commands.
	for _, kind := range []string{"machineimage", "clusterrelease"} {
		stdout, stderr, err := runGenerate(t, "create", kind, "-f", filepath.Join(dir, "releases", kind+".yaml"), "--dry-run=client", "-o", "yaml")
		if err != nil {
			t.Fatalf("nvkectl create %s --dry-run=client: %v\n%s", kind, err, stderr)
		}
		if err := os.WriteFile(filepath.Join(dir, "bootstrap-releases", kind+".yaml"), []byte(stdout), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out strings.Builder
	if err := runValidateSiteSpec(&out, &validateSiteSpecOptions{siteSpec: path, bootstrapOnly: true}); err != nil {
		t.Fatalf("the documented releases failed the bootstrap check: %v\n%s", err, out.String())
	}
	for name, want := range requests {
		got, err := os.ReadFile(filepath.Join(dir, "releases", name))
		if err != nil || string(got) != want {
			t.Errorf("releases/%s changed (err %v)", name, err)
		}
	}

	// The request documents themselves cannot be applied, and are refused.
	text := strings.Replace(string(example), `releases: "bootstrap-releases"`, `releases: "releases"`, 1)
	if text == string(example) {
		t.Fatal("the example no longer names bootstrap-releases; update this test")
	}
	requestsSpec := filepath.Join(dir, "sitespec-requests.yaml")
	if err := os.WriteFile(requestsSpec, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runValidateSiteSpec(&out, &validateSiteSpecOptions{siteSpec: requestsSpec, bootstrapOnly: true}); err == nil ||
		!strings.Contains(err.Error(), "without apiVersion and kind") {
		t.Errorf("releases/ of request documents: err = %v, output %q; want them refused", err, out.String())
	}
}

// TestValidateSiteSpec_BootstrapFilesAreCheckedInTheBootstrapSection: the files
// the bootstrap reads are no longer checked when the spec loads, so validate
// checks them itself, and a nico spec without them is not ready to bootstrap.
func TestValidateSiteSpec_BootstrapFilesAreCheckedInTheBootstrapSection(t *testing.T) {
	example, err := os.ReadFile("../../examples/sitespec-nico.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NVKECTL_NICO_CREDENTIALS", "")
	t.Setenv("NVKECTL_NODE_USER", "")
	var out strings.Builder
	err = runValidateSiteSpec(&out, &validateSiteSpecOptions{siteSpec: path, bootstrapOnly: true})
	if err == nil || !strings.Contains(err.Error(), "NVKECTL_NICO_CREDENTIALS must name") || !strings.Contains(err.Error(), "NVKECTL_NODE_USER is not set") || out.Len() != 0 {
		t.Fatalf("err = %v, stdout %q; want the credentials variable and the missing files in the error", err, out.String())
	}
}

// TestValidateSiteSpec_NicoSpecIsValidWithoutBootstrapInputs: the controller
// stack install runs validate sitespec without NVKECTL_NICO_CREDENTIALS, the
// node login, the bootstrap's release manifests and the SPIRE address the
// bootstrap's nodes dial. The nico spec is still valid there, with those
// reported; --bootstrap-only is where they fail.
func TestValidateSiteSpec_NicoSpecIsValidWithoutBootstrapInputs(t *testing.T) {
	path := nicoStackSiteSpec(t, func(text string) string {
		out := strings.Replace(text, `  externalAddress: "192.0.2.10"`, "", 1)
		if out == text {
			t.Fatal("the example no longer sets spire.externalAddress; update this test")
		}
		return out
	})
	t.Setenv("NVKECTL_NICO_CREDENTIALS", "")
	t.Setenv("NVKECTL_NODE_USER", "")

	var out strings.Builder
	if err := runValidateSiteSpec(&out, &validateSiteSpecOptions{siteSpec: path}); err != nil {
		t.Fatalf("validate: %v\n%s", err, out.String())
	}
	if got := out.String(); !strings.HasSuffix(got, "valid\n") || !strings.Contains(got, "bootstrap: not ready for nvkectl bootstrap site on this host") {
		t.Errorf("output %q; want the bootstrap inputs reported and the file valid", got)
	}

	out.Reset()
	err := runValidateSiteSpec(&out, &validateSiteSpecOptions{siteSpec: path, bootstrapOnly: true})
	if err == nil || out.Len() != 0 {
		t.Fatalf("--bootstrap-only: err = %v, output %q; want it to fail without the inputs", err, out.String())
	}
}

func TestControlPlaneVIPOverrideCLIFlags(t *testing.T) {
	body := strings.Replace(threeNodeSiteSpec, "  controlPlaneVIP: 10.0.0.100\n", "", 1)
	path := writeSiteSpec(t, body)
	for _, command := range [][]string{
		{"validate", "sitespec", "-f", path},
		{"bootstrap", "site", "-f", path, "--dry-run"},
	} {
		if _, _, err := runGenerate(t, command...); err == nil {
			t.Fatalf("%v accepted multiple control planes without the flag", command)
		}
		args := append(append([]string{}, command...), "--allow-control-plane-without-vip")
		if _, _, err := runGenerate(t, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
