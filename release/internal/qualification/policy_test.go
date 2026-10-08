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

const validPolicy = `
apiVersion: nke.nvidia.com/v1alpha1
kind: QualificationPolicy
spec:
  scopes:
    full: {}
  streams:
    - name: a
      capabilities: {kube: "1.34"}
`

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
apiVersion: nke.nvidia.com/v1alpha1
kind: QualificationPolicy
metadata: {name: nke}
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
