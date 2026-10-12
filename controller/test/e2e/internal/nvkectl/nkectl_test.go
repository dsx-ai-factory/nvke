// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// requireClosed fails unless ch closes promptly. done closing is what tells
// ensureForward the forward is gone, so a path that leaves it open is the bug
// these tests exist to catch.
func requireClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("done was never closed")
	}
}

func TestAwaitForwardPort(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		wantPort string
		wantErr  bool
	}{
		{
			name:     "reports the bound port",
			stdout:   "Forwarding from 127.0.0.1:54321 -> 8080\n",
			wantPort: "54321",
		},
		{
			// kubectl keeps logging after the line this waits for, so the
			// goroutine has to go on reading or the pipe fills and blocks the
			// forward it is meant to be serving.
			name:     "drains the output that follows",
			stdout:   "Forwarding from 127.0.0.1:1234 -> 8080\nHandling connection for 1234\nHandling connection for 1234\n",
			wantPort: "1234",
		},
		{
			name:    "errors when the port is never announced",
			stdout:  "error: unable to listen on any of the requested ports\n",
			wantErr: true,
		},
		{
			name:    "errors on empty output",
			stdout:  "",
			wantErr: true,
		},
		{
			// An IPv6 or non-loopback bind is not the local listener this
			// expects, so it must not be read as one.
			name:    "ignores a forward on another address",
			stdout:  "Forwarding from [::1]:54321 -> 8080\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan struct{})
			port, err := awaitForwardPort(io.NopCloser(strings.NewReader(tt.stdout)), done)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.wantPort, port)
			}
			requireClosed(t, done)
		})
	}
}

// A reader that blocks forever stands in for kubectl starting but never
// printing the line. The timeout has to fire, and done must still close once
// the pipe is shut, which is what startForward waits on before reaping.
func TestAwaitForwardPortClosesDoneAfterTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	errCh := make(chan error, 1)

	// The error comes back over a channel rather than being asserted in here:
	// require calls FailNow, which is only defined on the test's own goroutine.
	go func() { _, err := awaitForwardPort(reader, done); errCh <- err }()

	// The forward is never announced, so nothing should arrive on done while
	// the pipe is open.
	select {
	case <-done:
		t.Fatal("done closed while the pipe was still open")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, writer.Close())
	requireClosed(t, done)
	require.Error(t, <-errCh)
}

// WaitForClusterDeleted reads not-found as the cluster having gone, so anything
// this matches by mistake reads as a completed deletion.
func TestClusterNotFoundIn(t *testing.T) {
	const name = "capd-cluster-20260914"

	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "the cluster is absent",
			out:  `Error: cluster "capd-cluster-20260914" not found`,
			want: true,
		},
		{
			// The CLI says "not found" for a site missing from its config too.
			// Reading that as a deletion would pass the wait against a Runner
			// whose site registration had broken.
			name: "the site is missing from the config",
			out:  `Error: site "e2e-nvkectl" not found in config`,
			want: false,
		},
		{
			// And for the Secret behind `get kubeconfig`.
			name: "the admin kubeconfig secret is absent",
			out:  `Error: admin kubeconfig secret nke-system/capd-cluster-20260914-admin-kubeconfig not found -- check the cluster name`,
			want: false,
		},
		{
			name: "a different cluster is absent",
			out:  `Error: cluster "some-other-cluster" not found`,
			want: false,
		},
		{
			name: "an unrelated failure",
			out:  `Error: Get "http://127.0.0.1:1234/api/clusters": dial tcp: connection refused`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, clusterNotFoundIn(tt.out, name))
		})
	}
}

// CleanupCreated sweeps what trackCreated recorded, so a name missing from it
// is a cluster nothing will remove.
func TestTrackCreated(t *testing.T) {
	r := &Runner{}

	r.trackCreated("capd-one")
	r.trackCreated("capd-two")
	require.Equal(t, []string{"capd-one", "capd-two"}, r.createdClusters)

	// CreateCluster records the name it asked for and again the name the controller
	// reported, which are normally the same. The second must not duplicate.
	r.trackCreated("capd-one")
	require.Equal(t, []string{"capd-one", "capd-two"}, r.createdClusters)
}

func TestResolveBinary(t *testing.T) {
	t.Run("rejects a path that does not exist", func(t *testing.T) {
		_, err := resolveBinary(t.TempDir() + "/absent-nvkectl")
		require.Error(t, err)
	})

	t.Run("reports a missing binary as ErrBinaryNotFound", func(t *testing.T) {
		// An empty PATH and no override leaves nothing to find. Callers branch
		// on this to skip rather than fail, so the sentinel has to survive the
		// wrapping resolveBinary does.
		t.Setenv("PATH", t.TempDir())
		t.Setenv(BinaryEnv, "")

		_, err := resolveBinary("")
		require.Error(t, err)
		require.ErrorIs(t, err, ErrBinaryNotFound)
	})
}

// WaitForNodePoolDeleted reads not-found as the pool having gone, so anything
// this matches by mistake reads as a completed deletion.
func TestNodePoolNotFoundIn(t *testing.T) {
	ref := NodePoolRef{Cluster: "capd-cluster-20260914", Pool: "nvkectl-workers"}

	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "the node pool is absent",
			out:  `Error: node pool "nvkectl-workers" not found in cluster "capd-cluster-20260914"`,
			want: true,
		},
		{
			// `get nodepools --cluster` resolves the cluster first, so a missing
			// cluster says "not found" too. Reading that as a deletion would
			// pass the wait against a cluster that had gone instead.
			name: "the cluster is absent",
			out:  `Error: cluster "capd-cluster-20260914" not found`,
			want: false,
		},
		{
			name: "the same pool name is absent from another cluster",
			out:  `Error: node pool "nvkectl-workers" not found in cluster "some-other-cluster"`,
			want: false,
		},
		{
			name: "a different pool is absent from this cluster",
			out:  `Error: node pool "gpu-workers" not found in cluster "capd-cluster-20260914"`,
			want: false,
		},
		{
			name: "an unrelated failure",
			out:  `Error: Get "http://127.0.0.1:1234/api/nodepools": dial tcp: connection refused`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, nodePoolNotFoundIn(tt.out, ref))
		})
	}
}

// CleanupCreatedNodePools sweeps what trackCreatedNodePool recorded, so a pool
// missing from it is one nothing will remove.
func TestTrackCreatedNodePool(t *testing.T) {
	r := &Runner{}
	first := NodePoolRef{Cluster: "capd-one", Pool: "nvkectl-workers"}
	// The same pool name in another cluster is another pool.
	second := NodePoolRef{Cluster: "capd-two", Pool: "nvkectl-workers"}

	r.trackCreatedNodePool(first)
	r.trackCreatedNodePool(second)
	require.Equal(t, []NodePoolRef{first, second}, r.createdNodePools)

	// CreateNodePool records the name it asked for and again the name the controller
	// reported, which are normally the same. The second must not duplicate.
	r.trackCreatedNodePool(first)
	require.Equal(t, []NodePoolRef{first, second}, r.createdNodePools)

	r.untrackCreatedNodePool(first)
	require.Equal(t, []NodePoolRef{second}, r.createdNodePools)
}

func TestReadOnly(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"get", "clusters", "x", "-o", "json"}, true},
		{[]string{"describe", "nodepool", "x"}, true},
		{[]string{"create", "cluster", "-f", "x.json"}, false},
		{[]string{"delete", "cluster", "x"}, false},
		{[]string{"scale", "nodepool", "x", "--count", "2"}, false},
	}
	for _, tc := range cases {
		if got := readOnly(tc.args); got != tc.want {
			t.Errorf("readOnly(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestForwardBroken(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{`Error: listing clusters: Get "http://127.0.0.1:40543/api/clusters": read tcp 127.0.0.1:1->127.0.0.1:40543: read: connection reset by peer`, true},
		{`Error: listing clusters: Get "http://127.0.0.1:40543/api/clusters": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, true},
		{`Error: getting cluster x: unexpected status 500 Internal Server Error`, false},
		{`Error: cluster "x" not found`, false},
	}
	for _, tc := range cases {
		if got := forwardBroken(tc.out); got != tc.want {
			t.Errorf("forwardBroken(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// TestRunReplacesAWedgedForward: a fake kubectl idles after reporting a port; the fake nvkectl's first `get` fails like a wedged forward, its second succeeds.
func TestRunReplacesAWedgedForward(t *testing.T) {
	dir := t.TempDir()
	kubectlStarts := filepath.Join(dir, "kubectl-starts")
	nvkectlGets := filepath.Join(dir, "nvkectl-gets")
	writeScript(t, filepath.Join(dir, "kubectl"), `#!/bin/sh
echo start >> "`+kubectlStarts+`"
port=$((40000 + $(wc -l < "`+kubectlStarts+`")))
echo "Forwarding from 127.0.0.1:$port -> 8080"
exec sleep 300
`)
	nvkectl := filepath.Join(dir, "nvkectl")
	writeScript(t, nvkectl, `#!/bin/sh
case "$*" in
  *set-site*) exit 0 ;;
esac
echo get >> "`+nvkectlGets+`"
if [ "$(wc -l < "`+nvkectlGets+`")" -eq 1 ]; then
  echo 'Error: listing clusters: Get "http://127.0.0.1:40001/api/clusters": read tcp 127.0.0.1:1->127.0.0.1:40001: read: connection reset by peer' >&2
  exit 1
fi
echo '[]'
`)
	t.Setenv("PATH", dir+":/usr/bin:/bin") // the fakes first; wc and sleep from the system
	r := &Runner{binary: nvkectl, kubeconfig: filepath.Join(dir, "kubeconfig"), configHome: filepath.Join(dir, "config"), logf: t.Logf}
	t.Cleanup(func() { _ = r.Close() })

	out, err := r.Run(t.Context(), "get", "clusters", "-o", "json")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("Run output = %q, want the second call's []", out)
	}
	starts, err := os.ReadFile(kubectlStarts)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(starts), "\n"); n != 2 {
		t.Errorf("kubectl port-forward started %d time(s), want 2 (replaced once)", n)
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
