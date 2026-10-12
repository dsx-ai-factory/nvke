// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

var (
	// nodePoolCreatedRe matches the line `nvkectl create nodepool` prints on
	// success:
	//
	//	Node pool "gpu-workers" created (id: 0f8f...)
	nodePoolCreatedRe = regexp.MustCompile(`Node pool "([^"]+)" created \(id: ([^)]+)\)`)

	// errNodePoolNotFound reports that the CLI resolved no node pool of that name.
	errNodePoolNotFound = errors.New("node pool not found")
)

// NodePoolRef names a node pool the way nvkectl addresses one: by pool name
// within a cluster name. The CLI has no --nodepool flag and no lookup by id, so
// every call below is keyed on this pair.
type NodePoolRef struct {
	// Cluster is the value passed to --cluster.
	Cluster string
	// Pool is the positional node pool name.
	Pool string
}

// nodePoolNotFoundIn reports whether out carries the CLI's not-found error for
// this exact pool. `get`, `patch` and `delete` all phrase it identically
// (cli/internal/controller/nodepools/get.go notFoundError, and
// cli/internal/clients/nodepools.go FindNodePoolByName), and
// WaitForNodePoolDeleted reads not-found as success, so the whole sentence has
// to match rather than a bare "not found".
func nodePoolNotFoundIn(out string, ref NodePoolRef) bool {
	return strings.Contains(out, fmt.Sprintf("node pool %q not found in cluster %q", ref.Pool, ref.Cluster))
}

// CreateNodePool writes req to a file, creates it with
// `nvkectl create nodepool -f`, and returns the pool read back by name.
//
// The pool's identity is recorded before that read, so a pool the controller created but
// this could not then look up is still removable by CleanupCreatedNodePools.
func (r *Runner) CreateNodePool(ctx context.Context, clusterName string, req api.CreateNodePoolRequest) (*api.NodePool, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling the create request for node pool %q: %w", req.Name, err)
	}

	dir, err := os.MkdirTemp("", "nvkectl-e2e-nodepool-")
	if err != nil {
		return nil, fmt.Errorf("creating a spec directory: %w", err)
	}
	defer os.RemoveAll(dir)

	// JSON in a .yaml file: nvkectl parses -f as YAML, which is a superset.
	path := filepath.Join(dir, "nodepool.yaml")
	if writeErr := os.WriteFile(path, body, 0o600); writeErr != nil {
		return nil, fmt.Errorf("writing the spec for node pool %q: %w", req.Name, writeErr)
	}

	ref := NodePoolRef{Cluster: clusterName, Pool: req.Name}
	// Tracked before the command runs, for the same reason CreateCluster does:
	// a create the controller accepts but fails to report leaves a pool under this name
	// and no other trace of it.
	r.trackCreatedNodePool(ref)

	out, err := r.Run(ctx, "create", "nodepool", "-f", path)
	if err != nil {
		return nil, err
	}
	// Assert the success line, not just the exit status.
	match := nodePoolCreatedRe.FindStringSubmatch(out)
	if match == nil {
		return nil, fmt.Errorf("nvkectl create nodepool %q reported no id: %s", req.Name, strings.TrimSpace(out))
	}
	r.logf("nvkectl e2e: %s", strings.TrimSpace(out))

	ref.Pool = match[1]
	r.trackCreatedNodePool(ref) // a no-op unless the controller named it something else

	return r.GetNodePoolByName(ctx, ref)
}

// GetNodePoolByName returns the named node pool, or an error wrapping
// errNodePoolNotFound when the CLI resolves none.
func (r *Runner) GetNodePoolByName(ctx context.Context, ref NodePoolRef) (*api.NodePool, error) {
	out, err := r.Run(ctx, "get", "nodepools", ref.Pool, "--cluster", ref.Cluster, "-o", "json")
	if err != nil {
		if nodePoolNotFoundIn(out, ref) {
			return nil, fmt.Errorf("%q in cluster %q: %w", ref.Pool, ref.Cluster, errNodePoolNotFound)
		}
		return nil, err
	}

	var pools []api.NodePool
	if err := json.Unmarshal([]byte(out), &pools); err != nil {
		return nil, fmt.Errorf("parsing `get nodepools %s -o json`: %w: %s", ref.Pool, err, strings.TrimSpace(out))
	}
	if len(pools) != 1 {
		return nil, fmt.Errorf("`get nodepools %s -o json` returned %d node pools, want exactly 1", ref.Pool, len(pools))
	}
	if pools[0].Name != ref.Pool {
		return nil, fmt.Errorf("`get nodepools %s -o json` returned node pool %q", ref.Pool, pools[0].Name)
	}
	return &pools[0], nil
}

// ListNodePools returns every node pool of the named cluster.
func (r *Runner) ListNodePools(ctx context.Context, clusterName string) ([]api.NodePool, error) {
	out, err := r.Run(ctx, "get", "nodepools", "--cluster", clusterName, "-o", "json")
	if err != nil {
		return nil, err
	}
	var pools []api.NodePool
	if err := json.Unmarshal([]byte(out), &pools); err != nil {
		return nil, fmt.Errorf("parsing `get nodepools --cluster %s -o json`: %w: %s", clusterName, err, strings.TrimSpace(out))
	}
	return pools, nil
}

// ScaleNodePool sets the pool's desired node count through
// `nvkectl patch nodepool --patch`, and returns the pool as the CLI reports it
// afterwards. desiredNodeCount is the only field that command can set.
//
// Scaling up provisions nodes; scaling down does not remove running ones.
// controller/internal/controllers/nodeset/controller.go deletes only *pending*
// nodes on a surplus and deliberately skips auto-teardown of the rest, so a
// scale-down is observable in desiredNodeCount and nowhere else.
func (r *Runner) ScaleNodePool(ctx context.Context, ref NodePoolRef, desired int) (*api.NodePool, error) {
	out, err := r.Run(ctx, "patch", "nodepool", ref.Pool, "--cluster", ref.Cluster,
		"--patch", fmt.Sprintf("desiredNodeCount: %d", desired))
	if err != nil {
		if nodePoolNotFoundIn(out, ref) {
			return nil, fmt.Errorf("%q in cluster %q: %w", ref.Pool, ref.Cluster, errNodePoolNotFound)
		}
		return nil, err
	}
	if !strings.Contains(out, "patched (id: ") {
		return nil, fmt.Errorf("nvkectl patch nodepool %q did not report the patch: %s", ref.Pool, strings.TrimSpace(out))
	}
	r.logf("nvkectl e2e: %s", strings.TrimSpace(out))

	return r.GetNodePoolByName(ctx, ref)
}

// WaitForNodePoolProvisioned blocks until the pool's Provisioned condition is
// True, erroring if Provisioning flips to False, Timeouts.Provision passes, or
// ctx is done.
//
// The condition marks the pool record itself provisioned, not its nodes: the controller
// sets it once the node set exists (internal/temporal/workflows/provision_node_pool.go).
// Node readiness is asserted against the tenant cluster by the caller.
func (r *Runner) WaitForNodePoolProvisioned(ctx context.Context, ref NodePoolRef) (*api.NodePool, error) {
	deadline := time.After(r.timeouts.Provision)
	ticker := time.NewTicker(r.timeouts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for node pool %s to be provisioned", r.timeouts.Provision, ref.Pool)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for node pool %s to be provisioned: %w", ref.Pool, ctx.Err())
		case <-ticker.C:
			pool, err := r.GetNodePoolByName(ctx, ref)
			if err != nil {
				// A single failed poll is not fatal, for the reason
				// WaitForClusterReady gives: the API is reached through a
				// forward that reconnects, and the deadline above is the bound
				// that matters.
				r.logf("nvkectl e2e: polling node pool %s: %v", ref.Pool, err)
				continue
			}
			if pool.Conditions.Provisioned.Status == api.ConditionStatusTrue {
				return pool, nil
			}
			if pool.Conditions.Provisioning.Status == api.ConditionStatusFalse {
				return nil, fmt.Errorf("node pool %s provisioning failed: %s", ref.Pool, pool.Conditions.Provisioning.Message)
			}
		}
	}
}

// WaitForNodePoolDesiredCount blocks until the CLI reports the pool at desired,
// erroring if Timeouts.Provision passes or ctx is done. The patch is applied
// asynchronously, so the count read back immediately after one may still be the
// old value.
func (r *Runner) WaitForNodePoolDesiredCount(ctx context.Context, ref NodePoolRef, desired int) (*api.NodePool, error) {
	deadline := time.After(r.timeouts.Provision)
	ticker := time.NewTicker(r.timeouts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for node pool %s to report desiredNodeCount %d",
				r.timeouts.Provision, ref.Pool, desired)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for node pool %s to report desiredNodeCount %d: %w", ref.Pool, desired, ctx.Err())
		case <-ticker.C:
			pool, err := r.GetNodePoolByName(ctx, ref)
			if err != nil {
				r.logf("nvkectl e2e: polling node pool %s: %v", ref.Pool, err)
				continue
			}
			if pool.DesiredNodeCount == desired {
				return pool, nil
			}
		}
	}
}

// DeleteNodePool requests deletion of the named pool and requires the CLI to
// report it started.
func (r *Runner) DeleteNodePool(ctx context.Context, ref NodePoolRef) error {
	out, err := r.Run(ctx, "delete", "nodepool", ref.Pool, "--cluster", ref.Cluster)
	if err != nil {
		if nodePoolNotFoundIn(out, ref) {
			return fmt.Errorf("%q in cluster %q: %w", ref.Pool, ref.Cluster, errNodePoolNotFound)
		}
		return err
	}
	if !strings.Contains(out, "deletion started") {
		return fmt.Errorf("nvkectl delete nodepool %q did not report deletion: %s", ref.Pool, strings.TrimSpace(out))
	}
	r.logf("nvkectl e2e: %s", strings.TrimSpace(out))
	return nil
}

// WaitForNodePoolDeleted blocks until the pool no longer resolves by name,
// erroring if Timeouts.Delete passes or ctx is done.
func (r *Runner) WaitForNodePoolDeleted(ctx context.Context, ref NodePoolRef) error {
	deadline := time.After(r.timeouts.Delete)
	ticker := time.NewTicker(r.timeouts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for node pool %s to be deleted", r.timeouts.Delete, ref.Pool)
		case <-ctx.Done():
			return fmt.Errorf("waiting for node pool %s to be deleted: %w", ref.Pool, ctx.Err())
		case <-ticker.C:
			_, err := r.GetNodePoolByName(ctx, ref)
			if errors.Is(err, errNodePoolNotFound) {
				r.untrackCreatedNodePool(ref)
				return nil
			}
			if err != nil {
				r.logf("nvkectl e2e: polling for node pool %s to go away: %v", ref.Pool, err)
			}
		}
	}
}

// CleanupCreatedNodePools removes any node pool this Runner created that still
// exists. Call it before CleanupCreated, which takes their clusters away.
//
// Best-effort: it reports nothing, because it runs from test cleanup where a
// returned error would displace whatever really failed.
func (r *Runner) CleanupCreatedNodePools(ctx context.Context) {
	for _, ref := range r.createdNodePools {
		switch _, err := r.GetNodePoolByName(ctx, ref); {
		case errors.Is(err, errNodePoolNotFound):
			continue // the expected case: the delete spec already removed it
		case err != nil:
			// Not knowing is not the same as gone: say so, rather than leave a
			// pool running with nothing in the log to explain it.
			r.logf("nvkectl e2e: could not tell whether node pool %s is still there: %v", ref.Pool, err)
			continue
		}
		r.logf("nvkectl e2e: removing leftover node pool %s in cluster %s", ref.Pool, ref.Cluster)
		if err := r.DeleteNodePool(ctx, ref); err != nil {
			r.logf("nvkectl e2e: could not remove leftover node pool %s: %v", ref.Pool, err)
		}
	}
	r.createdNodePools = nil
}

// trackCreatedNodePool records a pool for CleanupCreatedNodePools to sweep,
// ignoring one already held.
func (r *Runner) trackCreatedNodePool(ref NodePoolRef) {
	if slices.Contains(r.createdNodePools, ref) {
		return
	}
	r.createdNodePools = append(r.createdNodePools, ref)
}

// untrackCreatedNodePool drops a pool the suite has already seen deleted.
func (r *Runner) untrackCreatedNodePool(ref NodePoolRef) {
	r.createdNodePools = slices.DeleteFunc(r.createdNodePools, func(held NodePoolRef) bool { return held == ref })
}
