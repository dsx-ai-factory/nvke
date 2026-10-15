// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const threeHostCSV = `name,ip,tpm_ek_thumbprint
cp-01,10.10.50.2,0000000000000000000000000000000000000000000000000000000000000000
cp-02,10.10.50.3,
cp-03,10.10.50.4,
`

const threeNodeVIPSet = "cluster.controlPlaneVIP=10.10.50.10"

// runGenerate drives the real command tree, so the flags, argument count and
// output plumbing are exercised rather than just the package beneath them.
func runGenerate(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRootCmd("test", "none", "clean", "now")
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func writeCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write hosts.csv: %v", err)
	}
	return path
}

func TestGenerateSiteSpecWritesToStdout(t *testing.T) {
	stdout, _, err := runGenerate(t, "generate", "sitespec", "my-tier0-site", "--nodes", writeCSV(t, threeHostCSV), "--set", threeNodeVIPSet)
	if err != nil {
		t.Fatalf("generate sitespec: %v", err)
	}

	for _, want := range []string{
		`clusterName: "my-tier0-site"`,
		`name: "cp-01"`,
		`ip: "10.10.50.4"`,
		`controlPlaneVIP: "10.10.50.10"`,
		"SPDX-License-Identifier: Apache-2.0",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("generated site spec is missing %q:\n%s", want, stdout)
		}
	}
}

// TestGenerateSiteSpecThenBootstrapDryRun is the end-to-end claim: what
// generate writes, bootstrap site accepts, with no editing in between. Every
// other test proves a part of that; this one proves the whole of it.
func TestGenerateSiteSpecThenBootstrapDryRun(t *testing.T) {
	stdout, _, err := runGenerate(t, "generate", "sitespec", "my-tier0-site", "--nodes", writeCSV(t, threeHostCSV), "--set", threeNodeVIPSet)
	if err != nil {
		t.Fatalf("generate sitespec: %v", err)
	}

	path := filepath.Join(t.TempDir(), "sitespec.yaml")
	if err := os.WriteFile(path, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runGenerate(t, "bootstrap", "site", "-f", path, "--dry-run"); err != nil {
		t.Fatalf("bootstrap site rejected the generated site spec: %v\n%s", err, stdout)
	}
}

func TestGenerateSiteSpecSetReachesTheFile(t *testing.T) {
	stdout, _, err := runGenerate(t, "generate", "sitespec", "my-tier0-site",
		"--nodes", writeCSV(t, threeHostCSV),
		"--set", threeNodeVIPSet,
		"--set", "cluster.cniName=ovn-kubernetes",
		"--set", "cluster.podCidr=10.244.0.0/16")
	if err != nil {
		t.Fatalf("generate sitespec: %v", err)
	}
	for _, want := range []string{`cniName: "ovn-kubernetes"`, `podCidr: "10.244.0.0/16"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--set did not reach the file; %q is absent:\n%s", want, stdout)
		}
	}
}

func TestGenerateSiteSpecReadsStdin(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd("test", "none", "clean", "now")
	root.SetOut(&out)
	root.SetIn(strings.NewReader(threeHostCSV))
	root.SetArgs([]string{"generate", "sitespec", "my-tier0-site", "--nodes", "-", "--set", threeNodeVIPSet})
	if err := root.Execute(); err != nil {
		t.Fatalf("generate sitespec --nodes -: %v", err)
	}
	if !strings.Contains(out.String(), `name: "cp-02"`) {
		t.Errorf("the CSV piped on standard input did not reach the file:\n%s", out.String())
	}
}

// TestGenerateSiteSpecRejects covers the ways an operator gets it wrong, and
// asserts nothing is written when they do — a half-written file redirected over
// a good one is worse than no file.
func TestGenerateSiteSpecRejects(t *testing.T) {
	valid := writeCSV(t, threeHostCSV)

	for name, tc := range map[string]struct {
		args    []string
		wantSub string
	}{
		"no cluster name": {
			[]string{"generate", "sitespec", "--nodes", valid}, "arg",
		},
		"no --nodes": {
			[]string{"generate", "sitespec", "my-tier0-site"}, "nodes",
		},
		"missing CSV": {
			[]string{"generate", "sitespec", "my-tier0-site", "--nodes", "/nope/hosts.csv"}, "open node CSV",
		},
		"cluster name is not a label": {
			[]string{"generate", "sitespec", "Not_A_Label", "--nodes", valid}, "clusterName",
		},
		"unknown --set path": {
			[]string{"generate", "sitespec", "s", "--nodes", valid, "--set", "cluster.nope=1"}, "unknown field",
		},
		"--set the registry credential": {
			[]string{"generate", "sitespec", "s", "--nodes", valid, "--set", "registry.apiKey=x"}, "committed",
		},
		"unsupported cni": {
			[]string{"generate", "sitespec", "s", "--nodes", valid, "--set", "cluster.cniName=cilium"}, "cniName",
		},
		"even node count": {
			[]string{
				"generate", "sitespec", "s", "--nodes",
				writeCSV(t, "name,ip\ncp-01,10.0.0.1\ncp-02,10.0.0.2\n"),
			}, "odd",
		},
		"malformed node ip": {
			[]string{
				"generate", "sitespec", "s", "--nodes",
				writeCSV(t, "name,ip\ncp-01,999.1.1.1\n"),
			}, "ip",
		},
		"unknown CSV column": {
			[]string{
				"generate", "sitespec", "s", "--nodes",
				writeCSV(t, "name,ip,colour\ncp-01,10.0.0.1,red\n"),
			}, "unknown column",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := runGenerate(t, tc.args...)
			if err == nil {
				t.Fatalf("generate sitespec accepted invalid input; want an error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
			if stdout != "" {
				t.Errorf("a failed generate wrote to stdout, which would truncate a redirected file:\n%s", stdout)
			}
		})
	}
}

func writeAdminKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.kubeconfig")
	const body = `apiVersion: v1
kind: Config
clusters:
- name: site
  cluster:
    server: https://10.10.50.2:6443
    certificate-authority-data: Q0EgQ0VSVA==
contexts:
- name: kubernetes-admin@site
  context:
    cluster: site
    user: kubernetes-admin
current-context: kubernetes-admin@site
users:
- name: kubernetes-admin
  user: {}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write admin kubeconfig: %v", err)
	}
	return path
}

func TestGenerateOIDCKubeconfigWritesToStdout(t *testing.T) {
	stdout, _, err := runGenerate(t,
		"generate", "oidc-kubeconfig",
		"--kubeconfig", writeAdminKubeconfig(t),
		"--oidc-issuer-url", "https://idp.example/realms/nke",
		"--oidc-client-id", "nvke-cli",
	)
	if err != nil {
		t.Fatalf("generate oidc-kubeconfig: %v", err)
	}
	for _, want := range []string{
		"https://10.10.50.2:6443",
		"oidc-login",
		"--oidc-issuer-url=https://idp.example/realms/nke",
		"--oidc-client-id=nvke-cli",
		"oidc@site",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("generated kubeconfig is missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "kubernetes-admin") {
		t.Errorf("admin identity leaked into generated kubeconfig:\n%s", stdout)
	}
	if strings.Contains(stdout, "device-code") || strings.Contains(stdout, "skip-open-browser") {
		t.Errorf("default kubeconfig stamped headless args:\n%s", stdout)
	}
}

func TestGenerateOIDCKubeconfigHeadless(t *testing.T) {
	stdout, _, err := runGenerate(t,
		"generate", "oidc-kubeconfig",
		"--kubeconfig", writeAdminKubeconfig(t),
		"--oidc-issuer-url", "https://idp.example/realms/nke",
		"--oidc-client-id", "nvke-cli",
		"--headless",
	)
	if err != nil {
		t.Fatalf("generate oidc-kubeconfig: %v", err)
	}
	for _, want := range []string{
		"--grant-type=device-code",
		"--skip-open-browser",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("generated kubeconfig is missing %q:\n%s", want, stdout)
		}
	}
}

func TestGenerateOIDCKubeconfigWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oidc.kubeconfig")
	stdout, _, err := runGenerate(t,
		"generate", "oidc-kubeconfig",
		"--kubeconfig", writeAdminKubeconfig(t),
		"--oidc-issuer-url", "https://idp.example/realms/nke",
		"--oidc-client-id", "nvke-cli",
		"--output-file", path,
	)
	if err != nil {
		t.Fatalf("generate oidc-kubeconfig: %v", err)
	}
	if !strings.Contains(stdout, path) {
		t.Errorf("stdout = %q, want the output path", stdout)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestGenerateOIDCKubeconfigRejects(t *testing.T) {
	valid := writeAdminKubeconfig(t)
	for name, tc := range map[string]struct {
		args    []string
		wantSub string
	}{
		"no --kubeconfig": {
			[]string{
				"generate", "oidc-kubeconfig",
				"--oidc-issuer-url", "https://idp.example",
				"--oidc-client-id", "nvke-cli",
			},
			"kubeconfig",
		},
		"no --oidc-issuer-url": {
			[]string{
				"generate", "oidc-kubeconfig",
				"--kubeconfig", valid,
				"--oidc-client-id", "nvke-cli",
			},
			"oidc-issuer-url",
		},
		"no --oidc-client-id": {
			[]string{
				"generate", "oidc-kubeconfig",
				"--kubeconfig", valid,
				"--oidc-issuer-url", "https://idp.example",
			},
			"oidc-client-id",
		},
		"empty --kubeconfig": {
			[]string{
				"generate", "oidc-kubeconfig",
				"--kubeconfig", "",
				"--oidc-issuer-url", "https://idp.example",
				"--oidc-client-id", "nvke-cli",
			},
			"kubeconfig",
		},
		"empty --oidc-issuer-url": {
			[]string{
				"generate", "oidc-kubeconfig",
				"--kubeconfig", valid,
				"--oidc-issuer-url", "",
				"--oidc-client-id", "nvke-cli",
			},
			"oidc-issuer-url",
		},
		"empty --oidc-client-id": {
			[]string{
				"generate", "oidc-kubeconfig",
				"--kubeconfig", valid,
				"--oidc-issuer-url", "https://idp.example",
				"--oidc-client-id", "",
			},
			"oidc-client-id",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := runGenerate(t, tc.args...)
			if err == nil {
				t.Fatalf("accepted invalid input; want an error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
			if stdout != "" {
				t.Errorf("a failed generate wrote to stdout:\n%s", stdout)
			}
		})
	}
}
