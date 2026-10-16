// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
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
