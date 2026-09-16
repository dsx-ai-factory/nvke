// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodepools

import (
	"strings"
	"testing"
)

func TestDeleteNodePool(t *testing.T) {
	fake, clientFn := newFakeCWE(t, seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3})

	out, err := run(t, NewDeleteCmd(clientFn), "gpu-workers", "--cluster", "alpha")
	if err != nil {
		t.Fatalf("delete nodepool: %v", err)
	}

	if len(fake.stored()) != 0 {
		t.Errorf("stored pools = %v, want the pool removed", fake.stored())
	}
	for _, want := range []string{`"gpu-workers"`, "deletion started", "async", "get nodepools --cluster alpha"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	seen := fake.seen()
	if len(seen) != 2 {
		t.Fatalf("requests = %v, want a list then a delete", seen)
	}
	if !strings.Contains(seen[0], "clusterName=alpha") || !strings.Contains(seen[0], "name=gpu-workers") {
		t.Errorf("list request = %q, want a GET filtered by cluster name and pool name", seen[0])
	}
	if !strings.HasPrefix(seen[1], "DELETE /api/nodePools/") {
		t.Errorf("delete request = %q, want a DELETE of the resolved pool", seen[1])
	}
}

func TestDeleteNodePoolResolvesWithinTheNamedCluster(t *testing.T) {
	fake, clientFn := newFakeCWE(t,
		seed{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3},
		seed{clusterName: "beta", name: "gpu-workers", desiredNodeCount: 5},
	)

	if _, err := run(t, NewDeleteCmd(clientFn), "gpu-workers", "--cluster", "beta"); err != nil {
		t.Fatalf("delete nodepool: %v", err)
	}

	stored := fake.stored()
	if len(stored) != 1 {
		t.Fatalf("stored pools = %v, want alpha's pool left alone", stored)
	}
	if stored[0].Name != "gpu-workers" || stored[0].DesiredNodeCount != 3 {
		t.Errorf("surviving pool = %+v, want alpha's untouched pool", stored[0])
	}
}

func TestDeleteNodePoolErrors(t *testing.T) {
	tests := []struct {
		name     string
		seeds    []seed
		args     []string
		clientFn func(*testing.T) CWEClientFn
		wantErr  string
	}{
		{
			name:    "node pool not found",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"cpu-workers", "--cluster", "alpha"},
			wantErr: `node pool "cpu-workers" not found in cluster "alpha"`,
		},
		{
			name:    "node pool is in another cluster",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers", "--cluster", "beta"},
			wantErr: `node pool "gpu-workers" not found in cluster "beta"`,
		},
		{
			name: "node pool name is ambiguous within the cluster",
			seeds: []seed{
				{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3},
				{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 5},
			},
			args:    []string{"gpu-workers", "--cluster", "alpha"},
			wantErr: "is ambiguous",
		},
		{
			name:    "cluster flag missing",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"gpu-workers"},
			wantErr: `required flag(s) "cluster" not set`,
		},
		{
			name:    "node pool name missing",
			seeds:   []seed{{clusterName: "alpha", name: "gpu-workers", desiredNodeCount: 3}},
			args:    []string{"--cluster", "alpha"},
			wantErr: "accepts 1 arg(s)",
		},
		{
			// CWE's own rejection for the last joined control-plane pool,
			// surfaced verbatim rather than pre-checked client-side.
			name: "server rejects deleting the last control-plane pool",
			args: []string{"control-plane", "-c", "alpha"},
			clientFn: func(t *testing.T) CWEClientFn {
				t.Helper()
				fake, fn := newFakeCWE(t, seed{
					clusterName: "alpha", name: "control-plane", desiredNodeCount: 1, controlPlane: true,
				})
				fake.failDeleteWith(400,
					`{"message":"cannot delete the last joined control-plane node pool; delete the cluster instead"}`)
				return fn
			},
			wantErr: "deleting node pool: 400 Bad Request: cannot delete the last joined control-plane node pool",
		},
		{
			name: "server rejects the lookup",
			args: []string{"gpu-workers", "-c", "alpha"},
			clientFn: func(t *testing.T) CWEClientFn {
				t.Helper()
				return newBrokenCWE(t, 503, `{"message":"upstream unavailable"}`)
			},
			wantErr: "finding node pool: listing node pools: 503 Service Unavailable: upstream unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientFn := tt.clientFn
			if clientFn == nil {
				clientFn = func(t *testing.T) CWEClientFn {
					t.Helper()
					_, fn := newFakeCWE(t, tt.seeds...)
					return fn
				}
			}
			out, err := run(t, NewDeleteCmd(clientFn(t)), tt.args...)
			if err == nil {
				t.Fatalf("want an error, got output:\n%s", out)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
