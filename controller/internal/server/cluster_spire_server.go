// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
)

const (
	// spireStatusConcurrency bounds the node agents GET spire-server dials at once.
	spireStatusConcurrency = 16
	// spireStatusTimeout bounds each node agent call, so one dead node cannot
	// stall the request.
	spireStatusTimeout = 10 * time.Second
)

var (
	errSpireServerDuringDeletion          = errors.New("cannot update the SPIRE server while cluster deletion is in progress")
	errSpireServerRequiresProvisioned     = errors.New("cannot update the SPIRE server until the cluster is provisioned")
	errSpireServerUpdateToDifferentServer = errors.New("a SPIRE server update to a different server is in progress")
)

// validateSpireServer rejects an address that is not a plain host name or IP
// address, and a port outside 1-65535. The address is written into the node's
// SPIRE agent config, so nothing else is accepted.
func validateSpireServer(server api.SpireServer) error {
	if server.Address == "" {
		return errors.New("address is required")
	}
	// The same check the node agent applies, so nothing it would refuse gets a 202.
	if net.ParseIP(server.Address) == nil && len(validation.IsDNS1123Subdomain(server.Address)) != 0 {
		return fmt.Errorf("address %q is not a host name or IP address", server.Address)
	}
	if server.Port < 1 || server.Port > 65535 {
		return fmt.Errorf("port %d is out of range 1-65535", server.Port)
	}
	return nil
}

// validateSpireServerUpdateCanStart applies the guards the other cluster-wide
// node operations apply: not while deleting, not before provisioning, and not
// while a legacy control-plane operation or a node agent upgrade runs.
func validateSpireServerUpdateCanStart(cluster db.Cluster) error {
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return errSpireServerDuringDeletion
	}
	if cluster.Conditions.Provisioned.Status != db.ConditionStatusTrue {
		return errSpireServerRequiresProvisioned
	}
	if cluster.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue {
		return &updateInProgressError{
			op:        db.OperationTypeK8sUpgrade,
			startedAt: cluster.Conditions.ControlPlaneUpgrading.LastTransitionTime,
			message:   cluster.Conditions.ControlPlaneUpgrading.Message,
		}
	}
	if cluster.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
		return &updateInProgressError{
			op:        db.OperationTypeCPNodesRecycling,
			startedAt: cluster.Conditions.CPNodesRecycling.LastTransitionTime,
			message:   cluster.Conditions.CPNodesRecycling.Message,
		}
	}
	if cluster.NodeAgentUpgrade != nil {
		return errAgentUpgradeInProgress
	}
	return nil
}

// UpdateClusterSpireServer records the requested SPIRE server and enqueues the
// UpdateSpireServer workflow, in one transaction, under the UpdateInProgress lock.
func (s *clusterService) UpdateClusterSpireServer(ctx context.Context, req api.UpdateClusterSpireServerRequestObject) (api.UpdateClusterSpireServerResponseObject, error) {
	if req.Body == nil {
		return api.UpdateClusterSpireServer400JSONResponse{Message: "request body is required"}, nil
	}
	if err := validateSpireServer(*req.Body); err != nil {
		return api.UpdateClusterSpireServer400JSONResponse{Message: err.Error()}, nil
	}

	err := s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, req.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return err
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		if uip := cluster.Conditions.UpdateInProgress; uip != nil {
			if uip.Type != db.OperationTypeSpireServerUpdate {
				return &updateInProgressError{op: uip.Type, startedAt: uip.StartedAt, message: uip.Message}
			}
			if cluster.SpireServer != nil && cluster.SpireServer.Address == req.Body.Address && cluster.SpireServer.Port == req.Body.Port {
				return nil // the same update is already running
			}
			return errSpireServerUpdateToDifferentServer
		}
		if err := validateSpireServerUpdateCanStart(cluster); err != nil {
			return err
		}

		now := time.Now().UTC()
		cluster.SpireServer = &db.ClusterSpireServer{Address: req.Body.Address, Port: req.Body.Port, RequestedAt: now}
		db.ClearUpdateFailed(&cluster)
		db.SetUpdateInProgress(&cluster, db.OperationTypeSpireServerUpdate, "pointing SPIRE agents at a new SPIRE server", now)
		if err := enqueueWorkflow(ctx, tx, otelx.WorkflowAttributes{
			WorkflowType: constants.UpdateSpireServerWorkflowType,
			WorkflowID:   constants.UpdateSpireServerWorkflowPrefix + cluster.ID.String(),
			ClusterID:    cluster.ID,
			ClusterName:  cluster.Name,
		}, map[string]any{
			clusterIDConst: cluster.ID,
			"address":      req.Body.Address,
			"port":         req.Body.Port,
		}); err != nil {
			return fmt.Errorf("failed to create workflow outbox entry for SPIRE server update: %w", err)
		}
		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to update cluster: %w", err)
		}
		return nil
	})
	if err != nil {
		var uipErr *updateInProgressError
		switch {
		case errors.Is(err, db.ErrNotFound):
			return api.UpdateClusterSpireServer404Response{}, nil
		case errors.As(err, &uipErr),
			errors.Is(err, errSpireServerUpdateToDifferentServer),
			errors.Is(err, errAgentUpgradeInProgress),
			errors.Is(err, errSpireServerDuringDeletion),
			errors.Is(err, errSpireServerRequiresProvisioned):
			return api.UpdateClusterSpireServer409JSONResponse{Message: err.Error()}, nil
		}
		return nil, fmt.Errorf("failed to update cluster SPIRE server: %w", err)
	}

	s.logger.Info("SPIRE server update accepted", clusterIDConst, req.ClusterID, "address", req.Body.Address, "port", req.Body.Port)
	return api.UpdateClusterSpireServer202Response{}, nil
}

// GetClusterSpireServer returns the requested SPIRE server and every node's
// SPIRE agent status, read live from the node agents.
func (s *clusterService) GetClusterSpireServer(ctx context.Context, req api.GetClusterSpireServerRequestObject) (api.GetClusterSpireServerResponseObject, error) {
	cluster, err := s.db.GetCluster(ctx, req.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetClusterSpireServer404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}
	nodes, err := s.db.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	resp := api.ClusterSpireServerStatus{Nodes: make([]api.NodeSpireAgentStatus, len(nodes))}
	if cluster.SpireServer != nil {
		resp.Requested = &api.SpireServer{Address: cluster.SpireServer.Address, Port: cluster.SpireServer.Port}
	}

	var g errgroup.Group
	g.SetLimit(spireStatusConcurrency)
	for i, node := range nodes {
		g.Go(func() error {
			resp.Nodes[i] = s.nodeSpireAgentStatus(ctx, node)
			return nil
		})
	}
	_ = g.Wait() // the goroutines never return an error

	return api.GetClusterSpireServer200JSONResponse(resp), nil
}

func (s *clusterService) nodeSpireAgentStatus(ctx context.Context, node db.Node) api.NodeSpireAgentStatus {
	status := api.NodeSpireAgentStatus{NodeID: node.ID, Name: node.Name}
	unreachable := func(msg string) api.NodeSpireAgentStatus {
		status.Message = &msg
		return status
	}
	if s.nodeAgentClient == nil {
		return unreachable("node agent client is not configured")
	}
	if node.IP == "" {
		return unreachable("node has no IP address")
	}

	ctx, cancel := context.WithTimeout(ctx, spireStatusTimeout)
	defer cancel()
	agentStatus, err := s.nodeAgentClient.GetSpireAgentStatus(ctx, node)
	if err != nil {
		s.logger.Warn("failed to get SPIRE agent status", "node", node.Name, "error", err)
		return unreachable(err.Error())
	}

	status.Reachable = true
	status.HasValidSVID = agentStatus.GetHasValidSvid()
	if addr := agentStatus.GetServerAddress(); addr != "" {
		status.ServerAddress = &addr
	}
	if port := int(agentStatus.GetServerPort()); port != 0 {
		status.ServerPort = &port
	}
	if exp := agentStatus.GetSvidExpiresAtUnixSeconds(); exp != 0 {
		t := time.Unix(exp, 0).UTC()
		status.SvidExpiresAt = &t
	}
	if msg := agentStatus.GetMessage(); msg != "" {
		status.Message = &msg
	}
	return status
}
