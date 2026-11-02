// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitecluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/clients"
	"github.com/dsx-ai-factory/nvke/cli/internal/clusterstate"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// pollInterval is how often WaitProvisioned asks the controller where it has got to. A
// var, not a const, so tests can shrink it rather than paying real wall-clock
// time for a retry loop — the same shape controllerdeploy's waitReadyPollInterval uses.
var pollInterval = 10 * time.Second

// DefaultWaitTimeout bounds a wait whose caller supplied no budget of its own.
const DefaultWaitTimeout = 30 * time.Minute

// ClusterAPI is the part of the controller client this package needs.
//
// An interface rather than *clients.ControllerClient so the create-and-wait flow can
// be tested against a fake that returns a scripted sequence of conditions,
// which is the only way to exercise the paths that matter here: a cluster that
// takes several polls to come up, one that starts deleting underneath us, and
// one that never finishes.
type ClusterAPI interface {
	CreateCluster(ctx context.Context, req api.CreateClusterJSONRequestBody) (*api.Cluster, error)
	GetCluster(ctx context.Context, id openapi_types.UUID) (*api.Cluster, error)
	ListControlPlaneNodes(ctx context.Context, clusterID openapi_types.UUID) ([]api.Node, error)
}

// ErrClusterFailed is returned when the controller reports a state the cluster cannot come
// back from on its own. Callers tell it apart from a timeout so they can say
// which happened.
var ErrClusterFailed = errors.New("cluster provisioning failed")

// ErrCreateUnconfirmed marks a Create whose request the controller may have
// accepted: the request went out and no answer came back, or the answer was not
// a refusal (a 4xx). The
// controller commits the cluster and starts provisioning it before it answers,
// so the cluster may exist and be provisioning.
var ErrCreateUnconfirmed = errors.New("the controller may have created the cluster")

// Create submits the cluster described by inv and returns what the controller recorded.
// Provisioning is asynchronous; see WaitProvisioned. An error that wraps
// ErrCreateUnconfirmed leaves open whether the cluster was created.
func Create(ctx context.Context, client ClusterAPI, inv *sitespec.SiteSpec, log *slog.Logger) (*api.Cluster, error) {
	req, err := ClusterRequest(inv)
	if err != nil {
		return nil, err
	}

	log.Info("creating the site cluster",
		"cluster", req.Name,
		"kubernetesVersion", req.ControlPlaneKubernetesVersion,
		"cni", req.CniPlugin,
		"infraProvider", inv.Infra.Provider,
		"controlPlaneNodes", inv.ControlPlaneCount())

	cluster, err := client.CreateCluster(ctx, req)
	if err != nil {
		if notCreated(err) {
			return nil, fmt.Errorf("sitecluster: creating cluster %q: %w", req.Name, err)
		}
		return nil, fmt.Errorf("sitecluster: creating cluster %q: %w: %w", req.Name, ErrCreateUnconfirmed, err)
	}
	log.Info("cluster created", "cluster", cluster.Name, "id", cluster.ID)
	return cluster, nil
}

// notCreated reports whether a CreateCluster error proves the controller did
// not create the cluster: it refused the request with a 4xx, or the connection
// was never made. Anything else, a 5xx or a request that went out and got no
// answer, may follow a committed create.
func notCreated(err error) bool {
	var status *clients.StatusError
	if errors.As(err, &status) {
		return status.StatusCode >= 400 && status.StatusCode < 500
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// WaitProvisioned polls the controller until the cluster reports Provisioned, reports a
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
		timeout = DefaultWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := ""
	lastErr := error(nil)
	for {
		cluster, err := client.GetCluster(ctx, id)
		if err != nil {
			// A read can fail while the cluster is perfectly healthy — the controller
			// restarting, for one. Keep polling and report the last error only
			// if the wait runs out.
			lastErr = err
		} else {
			lastErr = nil
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
			if clusterstate.IsProvisioningFailed(cluster) {
				return cluster, fmt.Errorf("%w: cluster %q provisioning failed: %s",
					ErrClusterFailed, cluster.Name, cluster.Conditions.Provisioning.Message)
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

// WaitControlPlaneJoined polls the controller until want control-plane nodes report
// Provisioned, or timeout elapses.
//
// This wait exists because the cluster's own Provisioned condition does not
// mean what a caller tearing down orchestration needs it to mean. The controller sets it
// as soon as the one node it picked as the initial control-plane node has run
// kubeadm init and had its CNI installed — deliberately, because the remaining
// nodes cannot join until there is something to join, and the controller that
// starts those joins waits on exactly this condition. They then join through
// their own workflows, one at a time. Stopping at the cluster condition would
// leave every node that had not got its turn yet stranded with no workflow
// engine left to finish it.
//
// Provisioned rather than Joined is the bar: Joined flips when kubeadm join
// returns, while the node's workflow still has its after-join hooks and
// labelling left to run.
func WaitControlPlaneJoined(
	ctx context.Context,
	client ClusterAPI,
	id openapi_types.UUID,
	want int,
	timeout time.Duration,
	log *slog.Logger,
) error {
	if want <= 0 {
		return nil
	}
	// No DefaultWaitTimeout fallback here, unlike WaitProvisioned: this wait is
	// handed what is left of a deadline it shares with that one, so a
	// non-positive value means the budget is spent, not that the caller wants a
	// default. Starting a fresh 30 minutes on it would overrun --timeout.
	if timeout <= 0 {
		return fmt.Errorf("sitecluster: no time left to wait for the %d control plane nodes "+
			"of cluster %s to join", want, id)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Info("waiting for the control plane nodes to join", "cluster_id", id, "nodes", want)

	lastProvisioned := -1
	lastErr := error(nil)
	outstanding := "no nodes reported yet"
	for {
		nodes, err := client.ListControlPlaneNodes(ctx, id)
		if err != nil {
			// As in WaitProvisioned: a failed read is not a failed cluster.
			lastErr = err
		} else {
			// The read recovered, so it is no longer what went wrong. Left set,
			// it would be reported as the reason a node never joined and send
			// the operator after the controller instead of the node.
			lastErr = nil
			provisioned, pending := controlPlaneProgress(nodes, want)
			outstanding = pending
			if provisioned != lastProvisioned {
				log.Info("control plane nodes joined",
					"cluster_id", id, "provisioned", provisioned, "of", want)
				lastProvisioned = provisioned
			}
			if provisioned >= want {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("sitecluster: only %d of %d control plane nodes of cluster %s "+
				"finished provisioning within %s (outstanding: %s, last error=%v)",
				max(lastProvisioned, 0), want, id, timeout, outstanding, lastErr)
		case <-time.After(pollInterval):
		}
	}
}

// controlPlaneProgress counts the control-plane nodes that finished
// provisioning and describes the ones that have not.
//
// The description is what the operator gets when the wait runs out, so it
// carries each outstanding node's condition message — the difference between
// "a node is slow" and "a node is stuck on something" — and says how many
// nodes the controller has not created a record for at all.
func controlPlaneProgress(nodes []api.Node, want int) (int, string) {
	provisioned := 0
	pending := make([]string, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		if clusterstate.IsTrue(&node.Conditions.Provisioned) {
			provisioned++
			continue
		}
		pending = append(pending, fmt.Sprintf("%s (provisioned=%s: %s)",
			node.Name, node.Conditions.Provisioned.Status, node.Conditions.Provisioned.Message))
	}
	if missing := want - len(nodes); missing > 0 {
		pending = append(pending, fmt.Sprintf("%d node(s) not yet created", missing))
	}
	if len(pending) == 0 {
		return provisioned, "none"
	}
	return provisioned, strings.Join(pending, ", ")
}
