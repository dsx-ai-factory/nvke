// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NVIDIA/nke/cli/internal/clusterstate"
	"github.com/NVIDIA/nke/cli/internal/sitespec"
	api "github.com/NVIDIA/nke/controller/api"
)

// pollInterval is how often WaitProvisioned asks CWE where it has got to. A
// var, not a const, so tests can shrink it rather than paying real wall-clock
// time for a retry loop — the same shape cwedeploy's waitReadyPollInterval uses.
var pollInterval = 10 * time.Second

// ClusterAPI is the part of the CWE client this package needs.
//
// An interface rather than *clients.CWEClient so the create-and-wait flow can
// be tested against a fake that returns a scripted sequence of conditions,
// which is the only way to exercise the paths that matter here: a cluster that
// takes several polls to come up, one that starts deleting underneath us, and
// one that never finishes.
type ClusterAPI interface {
	CreateCluster(ctx context.Context, req api.CreateClusterJSONRequestBody) (*api.Cluster, error)
	GetCluster(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error)
}

// ErrClusterFailed is returned when CWE reports a state the cluster cannot come
// back from on its own. Callers tell it apart from a timeout so they can say
// which happened.
var ErrClusterFailed = errors.New("cluster provisioning failed")

// Create submits the cluster described by inv and returns what CWE recorded.
// Provisioning is asynchronous; see WaitProvisioned.
func Create(ctx context.Context, client ClusterAPI, inv *sitespec.SiteSpec, log *slog.Logger) (*api.Cluster, error) {
	req, err := ClusterRequest(inv)
	if err != nil {
		return nil, err
	}

	log.Info("creating the site cluster",
		"cluster", req.Name,
		"kubernetesVersion", req.ControlPlaneKubernetesVersion,
		"cni", req.CniPlugin,
		"hosts", len(inv.Cluster.Nodes))

	cluster, err := client.CreateCluster(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sitecluster: creating cluster %q: %w", req.Name, err)
	}
	log.Info("cluster created", "cluster", cluster.Name, "id", cluster.ID)
	return cluster, nil
}

// WaitProvisioned polls CWE until the cluster reports Provisioned, reports a
// state it cannot recover from, or timeout elapses.
//
// The state is logged on every change rather than every poll: a bootstrap takes
// minutes, and a line per poll would bury the two or three transitions that
// actually say what is happening.
func WaitProvisioned(
	ctx context.Context,
	client ClusterAPI,
	id openapi_types.UUID,
	timeout time.Duration,
	log *slog.Logger,
) (*api.Cluster, error) {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := ""
	lastErr := error(nil)
	for {
		cluster, err := client.GetCluster(ctx, id)
		if err != nil {
			// A read can fail while the cluster is perfectly healthy — CWE
			// restarting, for one. Keep polling and report the last error only
			// if the wait runs out.
			lastErr = err
		} else {
			status := clusterstate.Status(cluster)
			if status != lastStatus {
				log.Info("cluster state", "cluster", cluster.Name, "status", status)
				lastStatus = status
			}
			if clusterstate.IsProvisioned(cluster) {
				return cluster, nil
			}
			if clusterstate.IsTrue(&cluster.Conditions.Deleting) {
				return cluster, fmt.Errorf("%w: cluster %q started deleting while it was being provisioned",
					ErrClusterFailed, cluster.Name)
			}
			if clusterstate.IsTrue(cluster.Conditions.UpdateFailed) {
				return cluster, fmt.Errorf("%w: cluster %q reports updateFailed: %s",
					ErrClusterFailed, cluster.Name, cluster.Conditions.UpdateFailed.Message)
			}
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("sitecluster: cluster %s did not report provisioned within %s "+
				"(last status=%q, last error=%v)", id, timeout, lastStatus, lastErr)
		case <-time.After(pollInterval):
		}
	}
}
