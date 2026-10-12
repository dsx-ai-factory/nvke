// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

func TestUnit_ExpectedControlPlaneFailureDomainCounts(t *testing.T) {
	tests := []struct {
		name    string
		domains []nvke.CAPDFailureDomain
		desired int
		want    map[string]int
	}{
		{name: "no eligible domains", domains: []nvke.CAPDFailureDomain{{Name: "worker"}}, desired: 1, want: map[string]int{}},
		{name: "zero machines", domains: []nvke.CAPDFailureDomain{{Name: "zone", ControlPlane: true}}, want: map[string]int{}},
		{name: "lexicographic first placement ignores spec order", domains: []nvke.CAPDFailureDomain{{Name: "zone-z", ControlPlane: true}, {Name: "zone-a", ControlPlane: true}}, desired: 1, want: map[string]int{"zone-a": 1}},
		{name: "three arbitrary domain names exclude workers", domains: []nvke.CAPDFailureDomain{{Name: "zone-z", ControlPlane: true}, {Name: "worker"}, {Name: "zone-m", ControlPlane: true}, {Name: "zone-a", ControlPlane: true}}, desired: 5, want: map[string]int{"zone-a": 2, "zone-m": 2, "zone-z": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, expectedControlPlaneFailureDomainCounts(nvke.ClusterSpec{CAPDFailureDomains: tt.domains}, tt.desired))
		})
	}
}

func TestUnit_CAPDFailureDomainReuse(t *testing.T) {
	tests := []struct {
		name       string
		backend    nvke.Backend
		controller bool
		reuse      string
		wantErr    bool
	}{
		{name: "new controller fixture", backend: nvke.BackendCAPD, controller: true},
		{name: "reject reused controller fixture", backend: nvke.BackendCAPD, controller: true, reuse: "existing", wantErr: true},
		{name: "legacy reuse remains supported", backend: nvke.BackendCAPD, reuse: "existing"},
		{name: "other backend reuse remains supported", backend: nvke.BackendNicoContainers, controller: true, reuse: "existing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCAPDFailureDomainReuse(nvke.ClusterSpec{Backend: tt.backend}, tt.controller, tt.reuse)
			if tt.wantErr {
				require.ErrorContains(t, err, "lifecycle.reuseCluster is unsupported")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestUnit_CAPDMachineDomain(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		observed  string
		actual    string
		eligible  bool
		missing   bool
		wantErr   string
	}{
		{name: "requested observed and container agree", requested: "zone", observed: "zone", actual: "zone", eligible: true},
		{name: "older provider omits observed domain", requested: "zone", actual: "zone", eligible: true},
		{name: "no eligible domains permits empty placement"},
		{name: "eligible domains require placement", eligible: true, wantErr: "requested ineligible domain"},
		{name: "worker-only domain is rejected", requested: "worker", observed: "worker", actual: "worker", eligible: true, wantErr: "requested ineligible domain"},
		{name: "container mismatch is rejected", requested: "zone", actual: "worker", eligible: true, wantErr: "but its Docker container has domain"},
		{name: "observed mismatch is rejected", requested: "zone", observed: "worker", actual: "zone", eligible: true, wantErr: "reports observed domain"},
		{name: "empty request still rejects observed placement", observed: "worker", wantErr: "reports observed domain"},
		{name: "missing running container is rejected", requested: "zone", eligible: true, missing: true, wantErr: "no running Docker container"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"name": "node", "labels": map[string]any{"cluster.x-k8s.io/control-plane": ""}},
				"spec":     map[string]any{"failureDomain": tt.requested},
				"status":   map[string]any{"failureDomain": tt.observed},
			}}
			containers := map[string]string{"node": tt.actual}
			if tt.missing {
				delete(containers, "node")
			}
			domain, err := capdMachineDomainAt(machine, map[string]bool{"zone": tt.eligible, "worker": false}, containers)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.actual, domain)
			}
		})
	}
}

func TestUnit_CAPDContainerDomainSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   map[string]string
		err    bool
	}{
		{name: "all containers fetched at once", output: "node-a\tzone-a\nnode-b\tzone-b\nunrelated\t\n", want: map[string]string{"node-a": "zone-a", "node-b": "zone-b", "unrelated": ""}},
		{name: "empty Docker inventory", want: map[string]string{}},
		{name: "malformed row is rejected", output: "missing-tab\n", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			calls := filepath.Join(root, "calls")
			require.NoError(t, os.WriteFile(filepath.Join(root, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CAPD_TEST_CALLS\"\nprintf '%s' \"$CAPD_TEST_OUTPUT\"\n"), 0o700))
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CAPD_TEST_CALLS", calls)
			t.Setenv("CAPD_TEST_OUTPUT", tt.output)
			domains, err := capdContainerFailureDomains(t.Context())
			if tt.err {
				require.ErrorContains(t, err, "malformed Docker")
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, domains)
			}
			raw, err := os.ReadFile(calls)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(raw), "\n"), "one Docker call per snapshot")
			require.Contains(t, string(raw), "ps --format")
			require.Contains(t, string(raw), `io.x-k8s.cluster.failureDomain`)
		})
	}
}
