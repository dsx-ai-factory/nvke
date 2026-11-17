// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --subject-spec names the subject's spec, so a subject kept apart from the
// tree --root names is looked for where it was pointed, not under the root.
func TestPlanReadsTheSubjectFromTheNamedFile(t *testing.T) {
	// The shipped registry reads the NGC organisation from the environment.
	t.Setenv("QUALIFY_E2E_NGC_ORG", "example-org")
	missing := filepath.Join(t.TempDir(), "elsewhere", "qualification.yaml")

	cmd := planCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--subject", "other",
		"--root", t.TempDir(),
		"--policy", "../../policy.yaml",
		"--registry", "../../registry.yaml",
		"--subject-spec", missing,
	})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want it to name %s", err, missing)
	}
}

// gitIn runs git in dir, failing the test if it fails. Signing is off so a
// developer's own git configuration cannot reach it.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

const (
	demoPolicy = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: QualificationPolicy
metadata:
  name: demo
spec:
  scopes:
    full:
      selector: {arch: amd64}
  streams:
    - name: amd64
      capabilities: {arch: amd64}
`
	demoRegistry = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: QualificationRegistry
metadata:
  name: demo
spec:
  components:
    - name: demo
      resolve:
        version: {strategy: gitTag, with: {tagPrefix: ""}}
    - name: widget
      resolve:
        version: {strategy: gitTag, with: {tagPrefix: "widget/"}}
`
	demoSubjectSpec = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: Qualification
metadata:
  name: demo
spec:
  subject: demo
  components:
    - {name: widget, selectors: [latest]}
`
)

// A subject spec kept outside the tree --root names plans end to end. The
// command assembles the resolvers, reads the spec from the file it is given,
// describes the components at the commit the subject's tag marks, and writes
// the plan.
func TestPlanWritesAPlanForASubjectSpecOutsideRoot(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(t, root, "tag", "v1.0.0")
	gitIn(t, root, "tag", "widget/v0.3.0")
	head := gitIn(t, root, "rev-parse", "HEAD")

	// Nothing the plan reads sits under root.
	files := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(files, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	var out bytes.Buffer
	cmd := planCmd()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--subject", "demo",
		"--root", root,
		"--policy", write("policy.yaml", demoPolicy),
		"--registry", write("registry.yaml", demoRegistry),
		"--subject-spec", write("qualification.yaml", demoSubjectSpec),
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var plan struct {
		Subject      string
		Version      string
		Commit       string
		Combinations []struct {
			Components []struct{ Name, Version string }
			Streams    []struct{ Name string }
		}
	}
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("the output is not a plan: %v\n%s", err, out.String())
	}
	if plan.Subject != "demo" || plan.Version != "v1.0.0" || plan.Commit != head {
		t.Errorf("plan is %s %s at %q, want demo v1.0.0 at %q", plan.Subject, plan.Version, plan.Commit, head)
	}
	if len(plan.Combinations) != 1 {
		t.Fatalf("plan has %d combinations, want 1", len(plan.Combinations))
	}
	c := plan.Combinations[0]
	if len(c.Components) != 1 || c.Components[0].Name != "widget" || c.Components[0].Version != "v0.3.0" {
		t.Errorf("components are %+v, want the widget at v0.3.0, the tag without its prefix", c.Components)
	}
	if len(c.Streams) != 1 || c.Streams[0].Name != "amd64" {
		t.Errorf("streams are %+v, want amd64", c.Streams)
	}
}
