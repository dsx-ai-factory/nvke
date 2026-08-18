// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package preflights

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseReadyzVerbose(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     string
		want   []string
		wantOk bool
	}{
		{
			name: "all green",
			in: `[+]ping ok
[+]log ok
[+]etcd ok
[+]informer-sync ok
readyz check passed
`,
			want:   nil,
			wantOk: true,
		},
		{
			name: "one failing",
			in: `[+]ping ok
[-]etcd failed: reason
[+]log ok
readyz check failed
`,
			want:   []string{"etcd"},
			wantOk: true,
		},
		{
			name: "multiple failing",
			in: `[-]etcd failed: reason
[+]ping ok
[-]informer-sync failed: not started
`,
			want:   []string{"etcd", "informer-sync"},
			wantOk: true,
		},
		{
			name:   "empty input",
			in:     "",
			want:   nil,
			wantOk: false,
		},
		{
			name:   "whitespace tolerated",
			in:     "  [-]etcd failed\n   [+]ping ok\n",
			want:   []string{"etcd"},
			wantOk: true,
		},
		{
			name:   "no markers fails closed",
			in:     "some other text without markers\nreadyz check passed\n",
			want:   nil,
			wantOk: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseReadyzVerbose(tc.in)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantOk, ok)
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	t.Parallel()

	cases := []struct {
		actual, minimum string
		want            bool
	}{
		// equal
		{"v0.1.17", "v0.1.17", true},
		{"0.1.17", "v0.1.17", true},
		{"v0.1.17", "0.1.17", true},

		// dev tags ignored on either side
		{"v0.1.17-3-gf5bc014", "v0.1.17", true},
		{"v0.1.17", "v0.1.17-rc.1", true},
		{"v0.1.21-dirty", "v0.1.21", true},
		{"v0.1.17+meta", "v0.1.17", true},

		// patch / minor / major bumps
		{"v0.1.18", "v0.1.17", true},
		{"v0.2.0", "v0.1.99", true},
		{"v1.0.0", "v0.99.99", true},

		// below minimum
		{"v0.1.17", "v0.1.21", false},
		{"v0.1.17", "v0.2.0", false},
		{"v0.0.99", "v0.1.0", false},

		// fail-closed on malformed input
		{"", "v0.1.17", false},
		{"not-a-version", "v0.1.17", false},
		{"v0.1", "v0.1.17", false},      // missing patch
		{"v0.1.17", "v0.1", false},      // missing patch on minimum
		{"v0.1.17.1", "v0.1.17", false}, // too many core segments
		{"v-1.0.0", "v0.0.0", false},    // negative
	}
	for _, tc := range cases {
		t.Run(tc.actual+"_vs_"+tc.minimum, func(t *testing.T) {
			t.Parallel()
			got := versionAtLeast(tc.actual, tc.minimum)
			assert.Equal(t, tc.want, got,
				"versionAtLeast(%q, %q)", tc.actual, tc.minimum)
		})
	}
}

func TestKubectlRawLocal(t *testing.T) {
	t.Parallel()

	got := kubectlRawLocal("/readyz?verbose")
	assert.True(t, strings.HasPrefix(got, "#!/bin/sh\n"),
		"script must carry its own shebang so the agent direct-exec path runs it instead of ENOEXEC-ing to a silent exit 0")
	assert.Contains(t, got, "--server https://localhost:6443",
		"must pin to local apiserver to avoid healthy-peer masking local breakage")
	assert.Contains(t, got, "--kubeconfig /etc/kubernetes/admin.conf")
	assert.Contains(t, got, "get --raw '/readyz?verbose'",
		"the --raw path must be shell-quoted so '?' isn't glob-expanded")
	assert.Contains(t, got, "--insecure-skip-tls-verify=true")
}

func TestHealthyRequired(t *testing.T) {
	t.Parallel()

	cases := []struct{ n, want int }{
		{0, 0}, // pathological — caught by caller, but defined behavior
		{1, 1}, // single-CP — all healthy required
		{2, 2}, // 2-CP — both healthy (no fault tolerance possible)
		{3, 3}, // 3-CP — all three (quorum 2 + 1 headroom)
		{4, 4}, // rare; capped at N
		{5, 4}, // 5-CP — quorum 3 + 1 headroom = 4
		{6, 5}, // 6-CP — quorum 4 + 1 headroom = 5
		{7, 5}, // 7-CP — quorum 4 + 1 headroom = 5
	}
	for _, tc := range cases {
		got := healthyRequired(tc.n)
		assert.Equal(t, tc.want, got, "healthyRequired(%d)", tc.n)
	}
}
