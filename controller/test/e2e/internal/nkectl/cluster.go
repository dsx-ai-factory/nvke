// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nkectl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/test/e2e/internal/nke"
)

// CreateCluster writes the spec to a file and creates it with
// `nkectl create cluster -f`, then reads the result back by name.
//
// The body is the same api.CreateClusterRequest the REST client posts, built by
// the same nke.BuildCreateRequest, so the two paths cannot drift apart in what
// they ask for. It is marshaled as JSON, which is valid YAML and keeps a YAML
// dependency out of this package.
//
// The created object is fetched rather than parsed out of the success line: the
// line carries only a name and an id, and every caller wants the cluster.
func (r *Runner) CreateCluster(ctx context.Context, spec nke.ClusterSpec) (*api.Cluster, error) {
	req, err := nke.BuildCreateRequest(spec)
	if err != nil {
		return nil, err
	}

	body, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling the create request for %q: %w", req.Name, err)
	}

	dir, err := os.MkdirTemp("", "nkectl-e2e-spec-")
	if err != nil {
		return nil, fmt.Errorf("creating a spec directory: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "cluster.yaml")
	if writeErr := os.WriteFile(path, body, 0o600); writeErr != nil {
		return nil, fmt.Errorf("writing the spec for %q: %w", req.Name, writeErr)
	}

	out, err := r.Run(ctx, "create", "cluster", "-f", path)
	if err != nil {
		return nil, err
	}
	// Assert the success line, not just the exit status: a command that created
	// nothing and still exited 0 is the regression most worth catching.
	if !strings.Contains(out, "created (id: ") {
		return nil, fmt.Errorf("nkectl create cluster %q reported no id: %s", req.Name, strings.TrimSpace(out))
	}
	r.logf("nkectl e2e: %s", strings.TrimSpace(out))

	return r.GetClusterByName(ctx, req.Name)
}

// GetClusterByName reads one cluster with `nkectl get clusters -c <name> -o json`.
// That form filters server-side through CWE's ?name= lookup and returns an
// array, which this requires to hold exactly the cluster asked for.
func (r *Runner) GetClusterByName(ctx context.Context, name string) (*api.Cluster, error) {
	out, err := r.Run(ctx, "get", "clusters", "-c", name, "-o", "json")
	if err != nil {
		return nil, err
	}

	var clusters []api.Cluster
	if err := json.Unmarshal([]byte(out), &clusters); err != nil {
		return nil, fmt.Errorf("parsing `get clusters -c %s -o json`: %w: %s", name, err, strings.TrimSpace(out))
	}
	if len(clusters) != 1 {
		return nil, fmt.Errorf("`get clusters -c %s -o json` returned %d clusters, want exactly 1", name, len(clusters))
	}
	if clusters[0].Name != name {
		return nil, fmt.Errorf("`get clusters -c %s -o json` returned cluster %q", name, clusters[0].Name)
	}
	return &clusters[0], nil
}

// GetByName satisfies the suite's cluster-creator seam. CWE resolves names, so
// the id is not needed to look one up.
func (r *Runner) GetByName(ctx context.Context, name string) (*api.Cluster, error) {
	return r.GetClusterByName(ctx, name)
}

// WaitForClusterReady polls through the CLI until the cluster's Provisioned
// condition is True, erroring if Provisioning flips to False, the deadline
// passes, or ctx is done.
//
// It takes an id to match the REST client's signature, but resolves the name
// first and polls by name: nkectl has no get-by-id form, and using the one it
// does have is the point of driving it here.
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
// prints.
//
// This is the one tenant command that never touches the CWE API: it reads the
// nke-system/<cluster>-admin-kubeconfig Secret the tenantaccess controller
// publishes, through the site's own kubeconfig. That Secret exists only once
// the cluster has provisioned, which is why the smoke lane -- whose cluster is
// deliberately configured never to provision -- can cover only its not-found
// path.
func (r *Runner) GetKubeconfig(ctx context.Context, name string) ([]byte, error) {
	out, err := r.Run(ctx, "get", "kubeconfig", name)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// DeleteCluster deletes by name and requires the CLI to say so.
func (r *Runner) DeleteCluster(ctx context.Context, name string) error {
	out, err := r.Run(ctx, "delete", "cluster", name)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "deletion started") {
		return fmt.Errorf("nkectl delete cluster %q did not report deletion: %s", name, strings.TrimSpace(out))
	}
	return nil
}

// nameForID finds the cluster with the given id by listing. The suite's seam
// hands ids around because the REST client is addressed that way; the CLI is
// not, so this is where the two meet.
func (r *Runner) nameForID(ctx context.Context, id openapi_types.UUID) (string, error) {
	out, err := r.Run(ctx, "get", "clusters", "-o", "json")
	if err != nil {
		return "", err
	}
	var clusters []api.Cluster
	if err := json.Unmarshal([]byte(out), &clusters); err != nil {
		return "", fmt.Errorf("parsing `get clusters -o json`: %w: %s", err, strings.TrimSpace(out))
	}
	for _, c := range clusters {
		if c.ID == id {
			return c.Name, nil
		}
	}
	return "", fmt.Errorf("no cluster with id %s in `get clusters -o json`", id)
}
