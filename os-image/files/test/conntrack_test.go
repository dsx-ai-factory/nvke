// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conntrackScript is the script under test, relative to this package.
var conntrackScript = func() string {
	abs, err := filepath.Abs(filepath.Join("..", "configure-conntrack.sh"))
	if err != nil {
		panic(err)
	}
	return abs
}()

// dropIn is the file the script writes into its sysctl.d directory.
const dropIn = "10-nke-conntrack.conf"

// node is the fake /proc a test runs the script against.
type node struct {
	memTotalKiB int  // MemTotal in meminfo; 0 leaves the line out
	max         int  // the kernel's current nf_conntrack_max
	buckets     int  // the kernel's current nf_conntrack_buckets
	noModule    bool // nf_conntrack is not loaded, so its keys are missing
}

// runConntrack runs configure-conntrack.sh against n, writing into sysctlDir.
// stdout and stderr are kept apart, so a warning from the shell itself cannot
// break an exact match on what the script prints.
func runConntrack(t *testing.T, n node, sysctlDir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := t.TempDir()

	meminfo := filepath.Join(root, "meminfo")
	content := "MemFree:         1024 kB\n"
	if n.memTotalKiB > 0 {
		content = fmt.Sprintf("MemTotal:       %d kB\n", n.memTotalKiB) + content
	}
	require.NoError(t, os.WriteFile(meminfo, []byte(content), 0o644))

	netfilter := filepath.Join(root, "netfilter")
	require.NoError(t, os.MkdirAll(netfilter, 0o755))
	if !n.noModule {
		writeKey(t, netfilter, "nf_conntrack_max", n.max)
		writeKey(t, netfilter, "nf_conntrack_buckets", n.buckets)
	}

	cmd := exec.CommandContext(t.Context(), "bash", append([]string{conntrackScript}, args...)...)
	cmd.Env = append(os.Environ(),
		"CONNTRACK_MEMINFO="+meminfo,
		"CONNTRACK_NETFILTER_DIR="+netfilter,
		"CONNTRACK_SYSCTL_DIR="+sysctlDir,
	)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

func writeKey(t *testing.T, dir, key string, value int) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, key), fmt.Appendf(nil, "%d\n", value), 0o644))
}

// settings returns the assignments in a sysctl.d file, without its comments.
func settings(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func TestConntrackTarget(t *testing.T) {
	tests := []struct {
		name        string
		memTotalKiB int
		want        string
	}{
		{name: "a tiny VM is held at the 1024 floor", memTotalKiB: 8000, want: "1024"},
		{name: "a small VM gets 64 entries per MiB", memTotalKiB: 2015700, want: "125981"},
		{name: "just under 9375 MiB stays below the cap", memTotalKiB: 9599999, want: "599999"},
		{name: "9375 MiB reaches the cap", memTotalKiB: 9600000, want: "600000"},
		{name: "a 1 TiB node is held at the cap", memTotalKiB: 1056472020, want: "600000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --print-target needs no kernel values and writes nothing.
			sysctlDir := filepath.Join(t.TempDir(), "sysctl.d")
			stdout, stderr, err := runConntrack(t, node{memTotalKiB: tt.memTotalKiB, noModule: true}, sysctlDir, "--print-target")
			require.NoError(t, err, stderr)
			assert.Equal(t, tt.want+"\n", stdout)
			assert.NoDirExists(t, sysctlDir)
		})
	}
}

func TestConntrackDropIn(t *testing.T) {
	tests := []struct {
		name string
		node node
		// target is the value the drop-in's header records, and want its
		// assignments; a nil want means no file is written.
		target int
		want   []string
	}{
		{
			name:   "a large node raises both keys from the kernel default",
			node:   node{memTotalKiB: 1056472020, max: 262144, buckets: 262144},
			target: 600000,
			want: []string{
				"net.netfilter.nf_conntrack_max = 600000",
				"net.netfilter.nf_conntrack_buckets = 600000",
			},
		},
		{
			name:   "a small VM raises both keys to its RAM-sized target",
			node:   node{memTotalKiB: 2015700, max: 65536, buckets: 65536},
			target: 125981,
			want: []string{
				"net.netfilter.nf_conntrack_max = 125981",
				"net.netfilter.nf_conntrack_buckets = 125981",
			},
		},
		{
			name: "values above the target are never lowered",
			node: node{memTotalKiB: 1056472020, max: 4194304, buckets: 1048576},
		},
		{
			name: "values already at the target write no file",
			node: node{memTotalKiB: 1056472020, max: 600000, buckets: 600064},
		},
		{
			name:   "only buckets is written when max is already above the target",
			node:   node{memTotalKiB: 1056472020, max: 1000000, buckets: 262144},
			target: 600000,
			want:   []string{"net.netfilter.nf_conntrack_buckets = 600000"},
		},
		{
			name:   "only max is written when buckets is already above the target",
			node:   node{memTotalKiB: 1056472020, max: 262144, buckets: 1048576},
			target: 600000,
			want:   []string{"net.netfilter.nf_conntrack_max = 600000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A directory that does not exist yet, as /run/sysctl.d at boot.
			sysctlDir := filepath.Join(t.TempDir(), "sysctl.d")
			stdout, stderr, err := runConntrack(t, tt.node, sysctlDir)
			require.NoError(t, err, stderr)

			path := filepath.Join(sysctlDir, dropIn)
			if tt.want == nil {
				assert.Contains(t, stdout, "nothing to raise")
				assert.NoFileExists(t, path)
				return
			}

			content, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, settings(string(content)))
			assert.Contains(t, string(content), fmt.Sprintf("# MemTotal %d kB -> target %d.", tt.node.memTotalKiB, tt.target))

			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
		})
	}
}

func TestConntrackFailsWithoutMemTotal(t *testing.T) {
	sysctlDir := t.TempDir()
	_, stderr, err := runConntrack(t, node{max: 262144, buckets: 262144}, sysctlDir)
	require.Error(t, err)
	assert.Contains(t, stderr, "cannot read MemTotal")
	assert.NoFileExists(t, filepath.Join(sysctlDir, dropIn))
}

func TestConntrackFailsWithoutTheModule(t *testing.T) {
	sysctlDir := t.TempDir()
	_, stderr, err := runConntrack(t, node{memTotalKiB: 1056472020, noModule: true}, sysctlDir)
	require.Error(t, err)
	assert.Contains(t, stderr, "is nf_conntrack loaded?")
	assert.NoFileExists(t, filepath.Join(sysctlDir, dropIn))
}

func TestConntrackRewritesOnlyOnChange(t *testing.T) {
	sysctlDir := t.TempDir()
	path := filepath.Join(sysctlDir, dropIn)
	large := node{memTotalKiB: 1056472020, max: 262144, buckets: 262144}

	_, stderr, err := runConntrack(t, large, sysctlDir)
	require.NoError(t, err, stderr)
	first, err := os.Stat(path)
	require.NoError(t, err)

	// The same inputs leave the file in place rather than replacing it with
	// an identical copy.
	stdout, stderr, err := runConntrack(t, large, sysctlDir)
	require.NoError(t, err, stderr)
	assert.Contains(t, stdout, "unchanged")
	second, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(first, second), "an unchanged drop-in was replaced")

	// New inputs replace it, and no temporary file is left behind.
	_, stderr, err = runConntrack(t, node{memTotalKiB: 2015700, max: 65536, buckets: 65536}, sysctlDir)
	require.NoError(t, err, stderr)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, settings(string(content)), "net.netfilter.nf_conntrack_max = 125981")

	entries, err := os.ReadDir(sysctlDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the drop-in should remain in %s", sysctlDir)
	assert.Equal(t, dropIn, entries[0].Name())
}

func TestConntrackTimeoutsFile(t *testing.T) {
	// The static file sets TIME_WAIT only; the UDP stream timeout stays at the
	// kernel's own 120s default.
	content, err := os.ReadFile(filepath.Join("..", "10-nke-conntrack-timeouts.conf"))
	require.NoError(t, err)
	assert.Equal(t, []string{"net.netfilter.nf_conntrack_tcp_timeout_time_wait = 60"}, settings(string(content)))
}
