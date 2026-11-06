// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validEntry = `
  - id: "1.2.11"
    title: Image pull policy
    scope:
      clusters: [site, tenant]
      roles: [control-plane]
    reason: Pre-loaded images must start without a registry.
    approver: ""
    reviewBy: "2027-05-04"
`

func TestLoadRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr []string
		check   func(t *testing.T, reg Register)
	}{
		{
			name: "a valid entry loads",
			yaml: "exceptions:" + validEntry,
			check: func(t *testing.T, reg Register) {
				t.Helper()
				require.Len(t, reg.Exceptions, 1)
				assert.Equal(t, Exception{
					ID:       "1.2.11",
					Title:    "Image pull policy",
					Scope:    Scope{Clusters: []Cluster{ClusterSite, ClusterTenant}, Roles: []Role{RoleControlPlane}},
					Reason:   "Pre-loaded images must start without a registry.",
					ReviewBy: "2027-05-04",
				}, reg.Exceptions[0])
			},
		},
		{
			name:    "an unknown field is an error",
			yaml:    "exceptions:" + validEntry + "    owner: someone\n",
			wantErr: []string{"owner"},
		},
		{
			name: "a missing title, reason or reviewBy is an error naming the id",
			yaml: `
exceptions:
  - id: "4.2.9"
    approver: ""
`,
			wantErr: []string{"4.2.9", "title", "reason", "reviewBy"},
		},
		{
			name: "an id that is not dotted numbers is an error",
			yaml: `
exceptions:
  - id: "1.2.x"
    title: t
    reason: r
    reviewBy: "2027-05-04"
`,
			wantErr: []string{`"1.2.x"`, "dotted"},
		},
		{
			name: "an unquoted id that YAML reads as a number is an error",
			yaml: `
exceptions:
  - id: 5.1
    title: t
    reason: r
    reviewBy: "2027-05-04"
`,
			wantErr: []string{"5.1", "quote"},
		},
		{
			name: "an unquoted reviewBy loads as the same date",
			yaml: `
exceptions:
  - id: "4.2.12"
    title: t
    reason: r
    reviewBy: 2027-05-01
`,
			check: func(t *testing.T, reg Register) {
				t.Helper()
				require.Len(t, reg.Exceptions, 1)
				assert.Equal(t, "2027-05-01", reg.Exceptions[0].ReviewBy)
			},
		},
		{
			name: "a reviewBy that is not a date is an error",
			yaml: `
exceptions:
  - id: "4.2.12"
    title: t
    reason: r
    reviewBy: next spring
`,
			wantErr: []string{"4.2.12", "reviewBy"},
		},
		{
			name: "an unknown cluster or role is an error",
			yaml: `
exceptions:
  - id: "4.2.12"
    title: t
    scope:
      clusters: [management]
      roles: [etcd]
    reason: r
    reviewBy: "2027-05-04"
`,
			wantErr: []string{"4.2.12", `"management"`, `"etcd"`},
		},
		{
			name: "two entries for one check with overlapping scope are an error",
			yaml: `
exceptions:
  - id: "5.1.1"
    title: t
    scope: {clusters: [site]}
    reason: r
    reviewBy: "2027-05-04"
  - id: "5.1.1"
    title: t
    reason: r
    reviewBy: "2027-05-04"
`,
			wantErr: []string{"5.1.1", "overlap"},
		},
		{
			name: "two entries for one check with disjoint scope load",
			yaml: `
exceptions:
  - id: "5.1.1"
    title: t
    scope: {clusters: [site]}
    reason: r
    reviewBy: "2027-05-04"
  - id: "5.1.1"
    title: t
    scope: {clusters: [tenant]}
    reason: r
    reviewBy: "2027-05-04"
`,
			check: func(t *testing.T, reg Register) {
				t.Helper()
				assert.Len(t, reg.Exceptions, 2)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, err := LoadRegister([]byte(tc.yaml))
			if len(tc.wantErr) > 0 {
				require.Error(t, err)
				for _, fragment := range tc.wantErr {
					assert.Contains(t, err.Error(), fragment)
				}
				return
			}
			require.NoError(t, err)
			tc.check(t, reg)
		})
	}
}

func TestScopeCovers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		scope   Scope
		cluster Cluster
		role    Role
		want    bool
	}{
		{name: "an empty scope covers everything", scope: Scope{}, cluster: ClusterSite, role: RoleWorker, want: true},
		{name: "a listed cluster with any role", scope: Scope{Clusters: []Cluster{ClusterTenant}}, cluster: ClusterTenant, role: RoleWorker, want: true},
		{name: "an unlisted cluster", scope: Scope{Clusters: []Cluster{ClusterTenant}}, cluster: ClusterSite, role: RoleControlPlane, want: false},
		{name: "a listed role on any cluster", scope: Scope{Roles: []Role{RoleControlPlane}}, cluster: ClusterSite, role: RoleControlPlane, want: true},
		{name: "an unlisted role", scope: Scope{Roles: []Role{RoleControlPlane}}, cluster: ClusterSite, role: RoleWorker, want: false},
		{
			name:  "both listed",
			scope: Scope{Clusters: []Cluster{ClusterSite}, Roles: []Role{RoleWorker}}, cluster: ClusterSite, role: RoleWorker, want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.scope.Covers(tc.cluster, tc.role))
		})
	}
}

// TestDefaultRegister makes every edit to exceptions.yaml pass the same checks
// as a hand-written register, under make test.
func TestDefaultRegister(t *testing.T) {
	t.Parallel()

	reg, err := DefaultRegister()
	require.NoError(t, err)
	assert.NotEmpty(t, reg.Exceptions)
}

// YAML reads an unquoted 5.10 as the number 5.1, so the error cannot echo the
// ID as written and must not suggest a different check.
func TestLoadRegisterUnquotedIDDoesNotSuggestAnotherCheck(t *testing.T) {
	t.Parallel()

	_, err := LoadRegister([]byte(`
exceptions:
  - id: 5.10
    title: t
    reason: r
    reviewBy: "2027-05-04"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceptions[0]")
	assert.Contains(t, err.Error(), "number")
	assert.Contains(t, err.Error(), "quote")
	assert.NotContains(t, err.Error(), `id: "5.1"`)
}
