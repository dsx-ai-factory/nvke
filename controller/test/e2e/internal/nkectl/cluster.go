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
	//
	// Parsed rather than ignored so a create whose follow-up lookup fails still
	// yields an identity for cleanup to act on.
	createdRe = regexp.MustCompile(`Cluster "([^"]+)" created \(id: ([^)]+)\)`)

	// errClusterNotFound reports that the CLI resolved no cluster of that name.
	errClusterNotFound = errors.New("cluster not found")
)

// CreateCluster writes the spec to a file and creates it with
// `nkectl create cluster -f`, then reads the result back by name.
//
// The body is the same api.CreateClusterRequest the REST client posts, built by
// the same nke.BuildCreateRequest, so the two paths cannot drift apart in what
// they ask for. It is marshaled as JSON, which is valid YAML and keeps a YAML
// dependency out of this package.
//
// The identity is recorded from the success line before the lookup runs. A
// cluster that CWE created but whose lookup then failed is still a cluster that
// exists, and CleanupCreated is what removes it.
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
	if id, perr := uuid.Parse(match[2]); perr == nil {
		r.names[id] = name
		r.created = append(r.created, createdCluster{name: name, id: id})
	}

	return r.GetClusterByName(ctx, name)
}

// GetClusterByName reads one cluster with `nkectl get clusters -c <name> -o json`.
// That form filters server-side through CWE's ?name= lookup and returns an
// array, which this requires to hold exactly the cluster asked for.
func (r *Runner) GetClusterByName(ctx context.Context, name string) (*api.Cluster, error) {
	out, err := r.Run(ctx, "get", "clusters", "-c", name, "-o", "json")
	if err != nil {
		if strings.Contains(strings.ToLower(out), "not found") {
			return nil, fmt.Errorf("%q: %w", name, errClusterNotFound)
		}
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
	r.names[clusters[0].ID] = clusters[0].Name
	return &clusters[0], nil
}

// GetByName satisfies the suite's lifecycle seam. CWE resolves names, so the id
// is not needed to look one up.
func (r *Runner) GetByName(ctx context.Context, name string) (*api.Cluster, error) {
	return r.GetClusterByName(ctx, name)
}

// WaitForClusterReady polls through the CLI until the cluster's Provisioned
// condition is True, erroring if Provisioning flips to False, the deadline
// passes, or ctx is done.
//
// It takes an id to match the REST client's signature, but polls by name:
// nkectl has no get-by-id form, and exercising the one it does have is the
// point of driving it here.
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

// DeleteCluster deletes by name and requires the CLI to say so. It takes an id
// to match the REST client's signature.
func (r *Runner) DeleteCluster(ctx context.Context, id openapi_types.UUID) error {
	name, err := r.nameForID(ctx, id)
	if err != nil {
		return err
	}
	return r.deleteClusterByName(ctx, name)
}

// WaitForClusterDeleted polls until the cluster no longer resolves by name.
//
// The name is read from the cache rather than by listing: the cluster is on its
// way out, so a lookup would start failing partway through the wait.
func (r *Runner) WaitForClusterDeleted(ctx context.Context, id openapi_types.UUID) error {
	name, ok := r.names[id]
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
				delete(r.names, id)
				return nil
			}
			if err != nil {
				r.logf("nkectl e2e: polling for %s to go away: %v", name, err)
			}
		}
	}
}

// CleanupCreated removes any cluster this Runner created that is still there.
//
// The normal path never needs it: the suite's delete spec removes the cluster
// and this finds nothing. It exists for the case the suite cannot see -- a
// create that succeeded but whose follow-up lookup failed, leaving the suite
// with no cluster recorded and nothing to clean up.
//
// Best-effort by design. It is called from cleanup, where returning an error
// would replace a real failure with this one.
func (r *Runner) CleanupCreated(ctx context.Context) {
	for _, c := range r.created {
		if _, err := r.GetClusterByName(ctx, c.name); err != nil {
			continue // already gone, or unreachable; nothing useful to do either way
		}
		r.logf("nkectl e2e: removing leftover cluster %s", c.name)
		if err := r.deleteClusterByName(ctx, c.name); err != nil {
			r.logf("nkectl e2e: could not remove leftover cluster %s: %v", c.name, err)
		}
	}
	r.created = nil
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

// nameForID resolves an id to the name the CLI addresses clusters by, from the
// cache when it can and by listing when it cannot. The suite's seam passes ids
// because the REST client is addressed that way; nkectl is not, so this is
// where the two meet.
func (r *Runner) nameForID(ctx context.Context, id openapi_types.UUID) (string, error) {
	if name, ok := r.names[id]; ok {
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
	for _, c := range clusters {
		r.names[c.ID] = c.Name
		if c.ID == id {
			return c.Name, nil
		}
	}
	return "", fmt.Errorf("no cluster with id %s in `get clusters -o json`", id)
}
