//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"os"
	"strings"
	"testing"
)

// fullVars returns a Vars with every required field populated — used as the
// baseline for table-driven tests that blank out individual fields.
func fullVars() Vars {
	return Vars{
		Hostname:     "test-vm",
		PasswordHash: "$6$rounds=4096$salt$hash",
		K8sProvision: AptProvision(
			"https://example.invalid/nke-apt-noble",
			"https://example.invalid/nke-apt-public.key",
			"nke-noble", "main", "nke-kubeadm-distro-1.34.2", "5"),
	}
}

// fullVarsJammy returns a Vars populated with Ubuntu 22.04 (Jammy) apt values.
func fullVarsJammy() Vars {
	return Vars{
		Hostname:     "test-vm",
		PasswordHash: "$6$rounds=4096$salt$hash",
		K8sProvision: AptProvision(
			"https://example.invalid/nke-apt-jammy",
			"https://example.invalid/nke-apt-public.key",
			"nke-jammy", "main", "nke-kubeadm-distro-1.34.2", "5"),
	}
}

func TestValidate_missing(t *testing.T) {
	fields := []struct {
		name  string
		blank func(*Vars)
	}{
		{"Hostname", func(v *Vars) { v.Hostname = "" }},
		{"PasswordHash", func(v *Vars) { v.PasswordHash = "" }},
		{"K8sProvision", func(v *Vars) { v.K8sProvision = "" }},
	}
	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			v := fullVars()
			f.blank(&v)
			if err := v.Validate(); err == nil {
				t.Fatalf("Validate() returned nil; want error for missing %s", f.name)
			}
		})
	}
}

func TestRender_substitutes(t *testing.T) {
	tmpl := t.TempDir() + "/user-data.yml.tmpl"
	content := "__HOSTNAME__ __PASSWORD_HASH__\n__K8S_PROVISION__"
	if err := writeFile(tmpl, content); err != nil {
		t.Fatal(err)
	}
	v := fullVars()
	out, err := Render(tmpl, v)
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if strings.Contains(out, "__") {
		t.Errorf("output still contains placeholders: %q", out)
	}
	for _, want := range []string{v.Hostname, v.PasswordHash, v.K8sProvision} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing value %q", want)
		}
	}
}

func TestRender_unresolved(t *testing.T) {
	tmpl := t.TempDir() + "/user-data.yml.tmpl"
	if err := writeFile(tmpl, "hostname: __HOSTNAME__\nunknown: __UNKNOWN_KEY__"); err != nil {
		t.Fatal(err)
	}
	_, err := Render(tmpl, fullVars())
	if err == nil {
		t.Fatal("Render() returned nil; want error for unresolved __UNKNOWN_KEY__")
	}
	if !strings.Contains(err.Error(), "__UNKNOWN_KEY__") {
		t.Errorf("error %q does not name the unresolved placeholder", err)
	}
}

// TestRender_realTemplate renders the actual cloud-init template shipped with
// this module and asserts no __KEY__ placeholders remain. This catches the case
// where someone adds a new placeholder to the .tmpl without adding the
// corresponding field to Vars (and its substitution in Render).
func TestRender_realTemplate(t *testing.T) {
	out, err := Render("../../testdata/user-data.yml.tmpl", fullVars())
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if strings.Contains(out, "__") {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "__") {
				t.Errorf("unresolved placeholder in rendered template: %q", line)
			}
		}
	}
}

func TestRender_realTemplate_jammy(t *testing.T) {
	out, err := Render("../../testdata/user-data.yml.tmpl", fullVarsJammy())
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	if strings.Contains(out, "__") {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "__") {
				t.Errorf("unresolved placeholder in rendered template: %q", line)
			}
		}
	}
	if !strings.Contains(out, "nke-apt-jammy") {
		t.Errorf("output missing nke-apt-jammy in rendered template")
	}
	if !strings.Contains(out, "nke-jammy") {
		t.Errorf("output missing nke-jammy in rendered template")
	}
}

// writeFile is a test helper that creates a file with the given content.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
