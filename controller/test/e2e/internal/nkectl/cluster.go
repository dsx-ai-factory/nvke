// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nkectl

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

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

var (
	// createdRe matches the line `nkectl create cluster` prints on success:
	//
	//	Cluster "my-cluster" created (id: 0f8f...)
	createdRe = regexp.MustCompile(`Cluster "([^"]+)" created \(id: ([^)]+)\)`)

	// errClusterNotFound reports that the CLI resolved no cluster of that name.
	errClusterNotFound = errors.New("cluster not found")
)

// CreateCluster writes spec to a file, creates it with
// `nkectl create cluster -f`, and returns the cluster read back by name.
//
// The new cluster's identity is recorded before that read, so a cluster CWE
// created but this could not then look up is still removable by CleanupCreated.
func (r *Runner) CreateCluster(ctx context.Context, spec nke.ClusterSpec) (*api.Cluster, error) {
	req, err := nke.BuildCreateRequest(spec)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling the create request for %q: %w", req.Name, err)
	}

	dir, err := os.MkdirTemp("", "nkectl-e2e-spec-")
	if err != nil {
		return nil, fmt.Errorf("creating a spec directory: %w", err)
	}
	defer os.RemoveAll(dir)

	// JSON in a .yaml file: nkectl parses -f as YAML, which is a superset.
	path := filepath.Join(dir, "cluster.yaml")
	if writeErr := os.WriteFile(path, body, 0o600); writeErr != nil {
		return nil, fmt.Errorf("writing the spec for %q: %w", req.Name, writeErr)
	}

	// Tracked before the command runs: a create CWE accepts but fails to report
	// -- a dropped connection, a timeout -- leaves a cluster under this name
	// and no other trace of it.
	r.trackCreated(req.Name)

	out, err := r.Run(ctx, "create", "cluster", "-f", path)
	if err != nil {
		return nil, err
	}
	// Assert the success line, not just the exit status: a command that created
	// nothing and still exited 0 is the regression most worth catching.
	match := createdRe.FindStringSubmatch(out)
	if match == nil {
		return nil, fmt.Errorf("nkectl create cluster %q reported no id: %s", req.Name, strings.TrimSpace(out))
	}
	r.logf("nkectl e2e: %s", strings.TrimSpace(out))

	name := match[1]
	r.trackCreated(name) // a no-op unless CWE named it something else
	if id, parseErr := uuid.Parse(match[2]); parseErr == nil {
		r.clusterNames[id] = name
	} else {
		r.logf("nkectl e2e: cluster %s reported an unparsable id %q: %v", name, match[2], parseErr)
	}

	return r.GetClusterByName(ctx, name)
}

// clusterNotFoundIn reports whether out carries the CLI's not-found error for
// this exact cluster. The whole sentence has to match: the CLI also says "not
// found" for a missing site and for an absent kubeconfig Secret, and
// WaitForClusterDeleted reads not-found as success.
func clusterNotFoundIn(out, name string) bool {
	return strings.Contains(out, fmt.Sprintf("cluster %q not found", name))
}

// GetClusterByName returns the named cluster, or an error wrapping
// errClusterNotFound when the CLI resolves none.
func (r *Runner) GetClusterByName(ctx context.Context, name string) (*api.Cluster, error) {
	out, err := r.Run(ctx, "get", "clusters", name, "-o", "json")
	if err != nil {
		if clusterNotFoundIn(out, name) {
			return nil, fmt.Errorf("%q: %w", name, errClusterNotFound)
		}
		return nil, err
	}

	var clusters []api.Cluster
	if err := json.Unmarshal([]byte(out), &clusters); err != nil {
		return nil, fmt.Errorf("parsing `get clusters %s -o json`: %w: %s", name, err, strings.TrimSpace(out))
	}
	if len(clusters) != 1 {
		return nil, fmt.Errorf("`get clusters %s -o json` returned %d clusters, want exactly 1", name, len(clusters))
	}
	if clusters[0].Name != name {
		return nil, fmt.Errorf("`get clusters %s -o json` returned cluster %q", name, clusters[0].Name)
	}
	r.clusterNames[clusters[0].ID] = clusters[0].Name
	return &clusters[0], nil
}

// GetByName returns the named cluster. It is GetClusterByName under the name
// the suite's lifecycle seam expects.
func (r *Runner) GetByName(ctx context.Context, name string) (*api.Cluster, error) {
	return r.GetClusterByName(ctx, name)
}

// WaitForClusterReady blocks until the cluster's Provisioned condition is True,
// erroring if Provisioning flips to False, Timeouts.Provision passes, or ctx is
// done.
//
// It takes an id to match the REST client's signature but polls by name, which
// is the only lookup nkectl has.
func (r *Runner) WaitForClusterReady(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error) {
	name, err := r.nameForID(ctx, id)
	if err != nil {
		return nil, err
	}

	deadline := time.After(r.timeouts.Provision)
	ticker := time.NewTicker(r.timeouts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for cluster %s to be provisioned", r.timeouts.Provision, name)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for cluster %s to be provisioned: %w", name, ctx.Err())
		case <-ticker.C:
			cluster, err := r.GetClusterByName(ctx, name)
			if err != nil {
				// A single failed poll is not fatal: the API is reachable through
				// a forward that reconnects, and the deadline above is the bound
				// that matters.
				r.logf("nkectl e2e: polling %s: %v", name, err)
				continue
			}
			if cluster.Conditions.Provisioned.Status == api.ConditionStatusTrue {
				return cluster, nil
			}
			if cluster.Conditions.Provisioning.Status == api.ConditionStatusFalse {
				return nil, fmt.Errorf("cluster %s provisioning failed: %s", name, cluster.Conditions.Provisioning.Message)
			}
		}
	}
}

// GetKubeconfig returns the tenant admin kubeconfig `nkectl get kubeconfig`
// prints for the named cluster.
//
// The command reads the nke-system/<cluster>-admin-kubeconfig Secret through the
// site's own kubeconfig rather than the CWE API. That Secret is published only
// once the cluster has provisioned, so this fails against one that has not.
func (r *Runner) GetKubeconfig(ctx context.Context, name string) ([]byte, error) {
	out, err := r.Run(ctx, "get", "kubeconfig", name)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// PatchCluster applies patch to the named cluster, passing it as --patch-file,
// and returns the cluster as the CLI reports it afterwards.
func (r *Runner) PatchCluster(ctx context.Context, name string, patch []byte) (*api.Cluster, error) {
	dir, err := os.MkdirTemp("", "nkectl-e2e-patch-")
	if err != nil {
		return nil, fmt.Errorf("creating a patch directory: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "patch.yaml")
	if writeErr := os.WriteFile(path, patch, 0o600); writeErr != nil {
		return nil, fmt.Errorf("writing the patch for %q: %w", name, writeErr)
	}

	out, err := r.Run(ctx, "patch", "cluster", name, "--patch-file", path)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(out, "patched (id: ") {
		return nil, fmt.Errorf("nkectl patch cluster %q did not report the patch: %s", name, strings.TrimSpace(out))
	}
	r.logf("nkectl e2e: %s", strings.TrimSpace(out))

	return r.GetClusterByName(ctx, name)
}

// DeleteCluster requests deletion of the cluster and requires the CLI to report
// it started. It takes an id to match the REST client's signature.
func (r *Runner) DeleteCluster(ctx context.Context, id openapi_types.UUID) error {
	name, err := r.nameForID(ctx, id)
	if err != nil {
		return err
	}
	return r.deleteClusterByName(ctx, name)
}

// WaitForClusterDeleted blocks until the cluster no longer resolves by name,
// erroring if Timeouts.Delete passes or ctx is done. The cluster must already
// be known to this Runner: a listing would stop finding it partway through.
func (r *Runner) WaitForClusterDeleted(ctx context.Context, id openapi_types.UUID) error {
	name, ok := r.clusterNames[id]
	if !ok {
		return fmt.Errorf("no cached name for cluster %s, so its deletion cannot be awaited", id)
	}

	deadline := time.After(r.timeouts.Delete)
	ticker := time.NewTicker(r.timeouts.Poll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout after %v waiting for cluster %s to be deleted", r.timeouts.Delete, name)
		case <-ctx.Done():
			return fmt.Errorf("waiting for cluster %s to be deleted: %w", name, ctx.Err())
		case <-ticker.C:
			_, err := r.GetClusterByName(ctx, name)
			if errors.Is(err, errClusterNotFound) {
				delete(r.clusterNames, id)
				return nil
			}
			if err != nil {
				r.logf("nkectl e2e: polling for %s to go away: %v", name, err)
			}
		}
	}
}

// CleanupCreated removes any cluster this Runner created that still exists.
//
// Best-effort: it reports nothing, because it runs from test cleanup where a
// returned error would displace whatever really failed.
func (r *Runner) CleanupCreated(ctx context.Context) {
	for _, name := range r.createdClusters {
		switch _, err := r.GetClusterByName(ctx, name); {
		case errors.Is(err, errClusterNotFound):
			continue // the expected case: the delete spec already removed it
		case err != nil:
			// Not knowing is not the same as gone: say so, rather than leave a
			// cluster running with nothing in the log to explain it.
			r.logf("nkectl e2e: could not tell whether cluster %s is still there: %v", name, err)
			continue
		}
		r.logf("nkectl e2e: removing leftover cluster %s", name)
		if err := r.deleteClusterByName(ctx, name); err != nil {
			r.logf("nkectl e2e: could not remove leftover cluster %s: %v", name, err)
		}
	}
	r.createdClusters = nil
}

// trackCreated records a cluster name for CleanupCreated to sweep, ignoring a
// name already held.
func (r *Runner) trackCreated(name string) {
	if slices.Contains(r.createdClusters, name) {
		return
	}
	r.createdClusters = append(r.createdClusters, name)
}

// deleteClusterByName issues the delete and requires the CLI to report it.
func (r *Runner) deleteClusterByName(ctx context.Context, name string) error {
	out, err := r.Run(ctx, "delete", "cluster", name)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "deletion started") {
		return fmt.Errorf("nkectl delete cluster %q did not report deletion: %s", name, strings.TrimSpace(out))
	}
	return nil
}

// nameForID resolves an id to the name nkectl addresses clusters by, from
// clusterNames when it can and by listing when it cannot.
func (r *Runner) nameForID(ctx context.Context, id openapi_types.UUID) (string, error) {
	if name, ok := r.clusterNames[id]; ok {
		return name, nil
	}

	out, err := r.Run(ctx, "get", "clusters", "-o", "json")
	if err != nil {
		return "", err
	}
	var clusters []api.Cluster
	if err := json.Unmarshal([]byte(out), &clusters); err != nil {
		return "", fmt.Errorf("parsing `get clusters -o json`: %w: %s", err, strings.TrimSpace(out))
	}
	for _, cluster := range clusters {
		r.clusterNames[cluster.ID] = cluster.Name
		if cluster.ID == id {
			return cluster.Name, nil
		}
	}
	return "", fmt.Errorf("no cluster with id %s in `get clusters -o json`", id)
}
