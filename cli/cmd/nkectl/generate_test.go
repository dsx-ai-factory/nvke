// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

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
	stdout, _, err := runGenerate(t, "generate", "sitespec", "my-tier0-site", "--nodes", writeCSV(t, threeHostCSV))
	if err != nil {
		t.Fatalf("generate sitespec: %v", err)
	}

	for _, want := range []string{
		`clusterName: "my-tier0-site"`,
		`name: "cp-01"`,
		`ip: "10.10.50.4"`,
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
	stdout, _, err := runGenerate(t, "generate", "sitespec", "my-tier0-site", "--nodes", writeCSV(t, threeHostCSV))
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
		"--set", "cluster.cniName=calico",
		"--set", "cluster.podCidr=10.100.0.0/16")
	if err != nil {
		t.Fatalf("generate sitespec: %v", err)
	}
	for _, want := range []string{`cniName: "calico"`, `podCidr: "10.100.0.0/16"`} {
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
	root.SetArgs([]string{"generate", "sitespec", "my-tier0-site", "--nodes", "-"})
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
