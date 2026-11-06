// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"strings"
	"testing"
)

func TestScopeMatches(t *testing.T) {
	stream := StreamDefinition{
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

// TestScopeExcludes checks that a scope leaves out the streams carrying a
// capability it excludes, with a selector or without one.
func TestScopeExcludes(t *testing.T) {
	forge := StreamDefinition{Name: "forge", Capabilities: map[string]string{"provider": "forge"}}
	perf := StreamDefinition{Name: "perf", Capabilities: map[string]string{"provider": "forge", "profile": "perf"}}

	scope := Scope{Selector: map[string]string{"provider": "forge"}, Exclude: map[string]string{"profile": "perf"}}
	if !scope.Matches(forge) {
		t.Error("a stream without the excluded capability should be matched")
	}
	if scope.Matches(perf) {
		t.Error("a stream with the excluded capability should be left out")
	}
	if (Scope{Exclude: map[string]string{"profile": "perf"}}).Matches(perf) {
		t.Error("a scope with no selector still leaves out what it excludes")
	}
	if !(Scope{Exclude: map[string]string{"profile": "other"}}).Matches(perf) {
		t.Error("a different value of the excluded capability should not leave the stream out")
	}
}

const validPolicy = `
apiVersion: nvke.nvidia.com/v1alpha1
kind: QualificationPolicy
spec:
  scopes:
    full: {}
  streams:
    - name: a
      capabilities: {kube: "1.34"}
`

// TestLoadPolicyRejects checks that a policy with a structural, selector,
// exclude or parameter fault is refused with an error that names it.
func TestLoadPolicyRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"wrong apiVersion", strings.Replace(validPolicy, "v1alpha1", "v1", 1), "apiVersion"},
		{"wrong kind", strings.Replace(validPolicy, "QualificationPolicy", "Widget", 1), "kind"},
		{"unknown field", strings.Replace(validPolicy, "  scopes:", "  scopez:", 1), "unknown field"},
		{
			"components no longer belong in the policy",
			strings.Replace(validPolicy, "  scopes:", "  components: [controller]\n  scopes:", 1),
			"unknown field",
		},
		{
			"selector on a capability no stream declares",
			strings.Replace(validPolicy, "full: {}", `full: {selector: {gpu: gb200}}`, 1),
			"which no stream declares",
		},
		{
			"duplicate stream name",
			validPolicy + "    - name: a\n      capabilities: {kube: \"1.35\"}\n",
			"two streams are named",
		},
		{
			"stream with no capabilities",
			strings.Replace(validPolicy, `      capabilities: {kube: "1.34"}`, "      capabilities: {}", 1),
			"no capabilities",
		},
		{
			"no streams",
			strings.Replace(validPolicy, "  streams:\n    - name: a\n      capabilities: {kube: \"1.34\"}\n", "  streams: []\n", 1),
			"spec.streams is empty",
		},
		{
			"exclude on a capability no stream declares",
			strings.Replace(validPolicy, "full: {}", `full: {exclude: {gpu: gb200}}`, 1),
			"excludes capability",
		},
		{
			"exclude with no value",
			strings.Replace(validPolicy, "full: {}", `full: {exclude: {kube: ""}}`, 1),
			"with no value",
		},
		{
			"a parameter nothing reads",
			strings.Replace(validPolicy, `      capabilities: {kube: "1.34"}`, "      capabilities: {kube: \"1.34\"}\n      parameters: {tenantWorker: 3}", 1),
			"unknown field",
		},
		{
			"a negative tenant worker count",
			strings.Replace(validPolicy, `      capabilities: {kube: "1.34"}`, "      capabilities: {kube: \"1.34\"}\n      parameters: {tenantWorkers: -1}", 1),
			"tenantWorkers is -1",
		},
		{
			"a negative suite timeout",
			strings.Replace(validPolicy, `      capabilities: {kube: "1.34"}`, "      capabilities: {kube: \"1.34\"}\n      parameters: {suiteTimeoutMinutes: -5}", 1),
			"suiteTimeoutMinutes is -5",
		},
		{
			"an environment that is a path",
			strings.Replace(validPolicy, `      capabilities: {kube: "1.34"}`, "      capabilities: {kube: \"1.34\"}\n      parameters: {environment: ../forge}", 1),
			"not an environment file's name",
		},
		{"no scopes", strings.Replace(validPolicy, "    full: {}\n", "", 1), "spec.scopes is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPolicy(write(t, "policy.yaml", tc.body))
			wantError(t, err, tc.want)
		})
	}
}

func TestLoadPolicyAccepts(t *testing.T) {
	p, err := LoadPolicy(write(t, "policy.yaml", `
apiVersion: nvke.nvidia.com/v1alpha1
kind: QualificationPolicy
metadata: {name: nvke}
spec:
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

// TestLoadPolicyAcceptsParametersAndExcludes checks that a policy whose scopes
// exclude and whose streams carry parameters loads with both intact.
func TestLoadPolicyAcceptsParametersAndExcludes(t *testing.T) {
	p, err := LoadPolicy(write(t, "policy.yaml", `
apiVersion: nvke.nvidia.com/v1alpha1
kind: QualificationPolicy
spec:
  scopes:
    full: {selector: {provider: forge}, exclude: {profile: perf}}
    perf: {selector: {profile: perf}}
  streams:
    - name: a
      capabilities: {provider: forge, kube: "1.34"}
    - name: b
      capabilities: {provider: forge, profile: perf, kube: "1.34"}
      parameters: {environment: forge-perf, tenantWorkers: 3, suiteTimeoutMinutes: 215}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Spec.Streams[0].Parameters; got != (Parameters{}) {
		t.Errorf("a stream naming no parameters has %+v", got)
	}
	want := Parameters{Environment: "forge-perf", TenantWorkers: 3, SuiteTimeoutMinutes: 215}
	if got := p.Spec.Streams[1].Parameters; got != want {
		t.Errorf("parameters = %+v, want %+v", got, want)
	}
}

// The perf stream holds the NVKE Scale site for hours, so only the scope that
// names it may select it. A scope added later with no selector would select
// it too, and this is what catches that.
func TestShippedPolicyKeepsThePerfStreamToItsScope(t *testing.T) {
	p, err := LoadPolicy("../../policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const perf = "forge-cpu-amd64-ubuntu-24.04-kube-1.36-perf"
	for name, scope := range p.Spec.Scopes {
		var selected []string
		for _, s := range p.Spec.Streams {
			if scope.Matches(s) {
				selected = append(selected, s.Name)
			}
		}
		if name == "perf" {
			if len(selected) != 1 || selected[0] != perf {
				t.Errorf("scope perf selects %v, want only %s", selected, perf)
			}
			continue
		}
		for _, n := range selected {
			if n == perf {
				t.Errorf("scope %s selects the perf stream", name)
			}
		}
	}
	if _, ok := p.Spec.Scopes["perf"]; !ok {
		t.Error("the policy has no perf scope")
	}
}
