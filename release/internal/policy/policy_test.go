// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is where the real policy files live, relative to this package.
const repoRoot = "../../.."

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qualification.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRepositoryPolicyFilesAreValid is the check that earns this package its
// place in the merge gate. The policy files are configuration that nothing
// compiles, so without this a typo in a capability name reaches CI as a
// scope that silently selects nothing.
func TestRepositoryPolicyFilesAreValid(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(repoRoot, "release", "qualification.yaml"))
	if err != nil {
		t.Fatalf("release/qualification.yaml: %v", err)
	}

	for _, component := range p.Spec.Components {
		path := ComponentPath(repoRoot, component)
		spec, err := LoadComponent(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}

		// Every dependency must itself be a component the policy covers.
		// One that is not could never be qualified, so holding against it
		// would be holding against something nothing tests.
		for _, d := range spec.Spec.Dependencies {
			if !contains(p.Spec.Components, d.Name) {
				t.Errorf("%s depends on %q, which the policy does not list as a component",
					component, d.Name)
			}
		}
	}
}

func TestScopeMatches(t *testing.T) {
	stream := Stream{
		Name:         "nico-amd64-kube-1.34",
		Capabilities: map[string]string{"provider": "nico", "arch": "amd64", "kube": "1.34"},
	}

	for _, tc := range []struct {
		name     string
		selector map[string]string
		want     bool
	}{
		{"no selector matches everything", nil, true},
		{"one capability", map[string]string{"kube": "1.34"}, true},
		{"every capability", map[string]string{"provider": "nico", "arch": "amd64", "kube": "1.34"}, true},
		{"wrong value", map[string]string{"kube": "1.35"}, false},
		{"one of two wrong", map[string]string{"kube": "1.34", "arch": "arm64"}, false},
		{"capability the stream lacks", map[string]string{"gpu": "gb200"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Scope{Selector: tc.selector}).Matches(stream); got != tc.want {
				t.Errorf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadPolicyRejects(t *testing.T) {
	const valid = `
apiVersion: nke.nvidia.com/v1alpha1
kind: QualificationPolicy
spec:
  components: [controller]
  scopes:
    full: {}
  streams:
    - name: a
      capabilities: {kube: "1.34"}
`

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"wrong apiVersion", strings.Replace(valid, "v1alpha1", "v1", 1), "apiVersion"},
		{"wrong kind", strings.Replace(valid, "QualificationPolicy", "Widget", 1), "kind"},
		{
			"unknown field",
			strings.Replace(valid, "  components:", "  componets:", 1),
			"unknown field",
		},
		{
			"selector on a capability no stream declares",
			strings.Replace(valid, "full: {}", `full: {selector: {gpu: gb200}}`, 1),
			"which no stream declares",
		},
		{
			"duplicate stream name",
			valid + "    - name: a\n      capabilities: {kube: \"1.35\"}\n",
			"two streams are named",
		},
		{
			"stream with no capabilities",
			strings.Replace(valid, `      capabilities: {kube: "1.34"}`, "      capabilities: {}", 1),
			"no capabilities",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPolicy(write(t, tc.body))
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadPolicyAccepts(t *testing.T) {
	p, err := LoadPolicy(write(t, `
apiVersion: nke.nvidia.com/v1alpha1
kind: QualificationPolicy
spec:
  components: [controller, cli]
  scopes:
    active: {selector: {kube: "1.34"}}
    full: {}
  streams:
    - name: a
      capabilities: {kube: "1.34", arch: amd64}
    - name: b
      capabilities: {kube: "1.35", arch: amd64}
`))
	if err != nil {
		t.Fatal(err)
	}

	if got := len(p.Spec.Streams); got != 2 {
		t.Errorf("got %d streams, want 2", got)
	}
	if p.Spec.Scopes["full"].Selector != nil {
		t.Error("the full scope should carry no selector")
	}
	if got := p.Spec.Scopes["active"].Selector["kube"]; got != "1.34" {
		t.Errorf("active selects kube %q, want 1.34", got)
	}
}

func TestLoadComponentSpecRejects(t *testing.T) {
	const valid = `
apiVersion: nke.nvidia.com/v1alpha1
kind: ComponentQualification
metadata:
  name: controller
spec:
  component: controller
  dependencies:
    - name: cli
      selectors: [latest]
`

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"metadata disagrees with spec",
			strings.Replace(valid, "  name: controller", "  name: cli", 1),
			"but spec.component is",
		},
		{
			"depends on itself",
			strings.Replace(valid, "    - name: cli", "    - name: controller", 1),
			"depends on itself",
		},
		{
			"dependency listed twice",
			valid + "    - name: cli\n      selectors: [latest]\n",
			"listed twice",
		},
		{
			"dependency with no selectors",
			strings.Replace(valid, "      selectors: [latest]", "      selectors: []", 1),
			"no selectors",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadComponent(write(t, tc.body))
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
