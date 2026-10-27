// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spirehandover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/clients"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// repointNodes asks the seed controller to point every node at the site
// server and waits until each node's agent holds an SVID from it.
func (h *handover) repointNodes(ctx context.Context) error {
	target := api.SpireServer{Address: h.cfg.SiteSpireAddress, Port: h.cfg.SiteSpirePort}
	if err := h.cfg.Controller.UpdateClusterSpireServer(ctx, h.cfg.ClusterID, target); err != nil {
		var status *clients.StatusError
		if !errors.As(err, &status) || status.StatusCode != http.StatusConflict {
			return err
		}
		same, checkErr := h.sameUpdateInProgress(ctx, target)
		if checkErr != nil {
			return errors.Join(err, checkErr)
		}
		if !same {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, h.cfg.PollTimeout)
	defer cancel()
	ticker := time.NewTicker(h.cfg.PollInterval)
	defer ticker.Stop()
	var pending []string
	var lastErr error
	for {
		status, err := h.cfg.Controller.GetClusterSpireServer(ctx, h.cfg.ClusterID)
		if err != nil {
			lastErr = err
			h.log.Warn("reading node SPIRE status", "err", err)
		} else {
			pending = pendingNodes(status.Nodes, target)
			if len(pending) == 0 {
				h.log.Info("every node holds an SVID from the site SPIRE server", "nodes", len(status.Nodes))
				return nil
			}
			if err := h.updateEnded(ctx, pending); err != nil {
				return err
			}
			h.log.Info("waiting for nodes to move to the site SPIRE server", "pending", len(pending), "of", len(status.Nodes))
		}
		select {
		case <-ctx.Done():
			if pending == nil && lastErr != nil {
				return fmt.Errorf("timed out reading node SPIRE status: %w", lastErr)
			}
			return fmt.Errorf("timed out waiting for nodes to hold an SVID from %s:%d: %s",
				target.Address, target.Port, strings.Join(pending, "; "))
		case <-ticker.C:
		}
	}
}

// sameUpdateInProgress reports whether a 409 came from a SPIRE server update
// to target that is still running, which a repeat of the request joins.
func (h *handover) sameUpdateInProgress(ctx context.Context, target api.SpireServer) (bool, error) {
	cluster, err := h.cfg.Controller.GetCluster(ctx, h.cfg.ClusterID)
	if err != nil {
		return false, err
	}
	op := cluster.Conditions.UpdateInProgress
	if op == nil || op.Type != api.UpdateInProgressTypeSpireServerUpdate {
		return false, nil
	}
	status, err := h.cfg.Controller.GetClusterSpireServer(ctx, h.cfg.ClusterID)
	if err != nil {
		return false, err
	}
	return status.Requested != nil && *status.Requested == target, nil
}

// updateEnded returns an error when the SPIRE server update has released the
// cluster's lock while nodes are still pending, since no further node will move.
func (h *handover) updateEnded(ctx context.Context, pending []string) error {
	cluster, err := h.cfg.Controller.GetCluster(ctx, h.cfg.ClusterID)
	if err != nil {
		h.log.Warn("reading the cluster's update state", "err", err)
		return nil
	}
	if op := cluster.Conditions.UpdateInProgress; op != nil && op.Type == api.UpdateInProgressTypeSpireServerUpdate {
		return nil
	}
	msg := strings.Join(pending, "; ")
	if f := cluster.Conditions.UpdateFailed; f != nil && f.Status == api.ConditionStatusTrue && f.Message != "" {
		msg = f.Message
	}
	return fmt.Errorf("SPIRE server update ended before every node moved: %s", msg)
}

// pendingNodes describes each node whose agent is not yet on target with a valid SVID.
func pendingNodes(nodes []api.NodeSpireAgentStatus, target api.SpireServer) []string {
	pending := []string{}
	for _, n := range nodes {
		if n.Reachable && n.HasValidSVID && n.ServerAddress != nil && *n.ServerAddress == target.Address &&
			n.ServerPort != nil && *n.ServerPort == target.Port {
			continue
		}
		msg := "not yet on the site server"
		if n.Message != nil && *n.Message != "" {
			msg = *n.Message
		}
		pending = append(pending, n.Name+": "+msg)
	}
	return pending
}
