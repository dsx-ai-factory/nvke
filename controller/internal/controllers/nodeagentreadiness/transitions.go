// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// probeCause returns the cause of a failed probe, or "" for a successful one.
// A node without an IP is NoAddress whatever the dial returned.
func probeCause(node db.Node, err error) agent.ProbeCause {
	if err == nil {
		return ""
	}
	if node.IP == "" {
		return agent.ProbeCauseNoAddress
	}
	return agent.CauseOf(err)
}

// causeTracker holds the last probe cause of each node. The condition stores
// only the status and message, so the previous cause lives here. After a
// controller restart every cause is unknown, and a failing node logs its cause
// once.
type causeTracker struct {
	mu     sync.Mutex
	causes map[uuid.UUID]agent.ProbeCause
}

// swap stores the node's new cause and returns the previous one.
func (t *causeTracker) swap(nodeID uuid.UUID, cause agent.ProbeCause) agent.ProbeCause {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.causes == nil {
		t.causes = make(map[uuid.UUID]agent.ProbeCause)
	}
	previous := t.causes[nodeID]
	if cause == "" {
		delete(t.causes, nodeID)
	} else {
		t.causes[nodeID] = cause
	}
	return previous
}

func (t *causeTracker) forget(nodeID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.causes, nodeID)
}

// logTransition logs a change of the node's nodeAgentReady status or cause at
// Info, and a change of the message alone at Debug. node holds the condition
// after the probe was applied, previous the condition before it.
func (c *AgentReadinessController) logTransition(node db.Node, previous db.Condition, probe nodeAgentProbe) {
	previousCause := c.causes.swap(node.ID, probe.cause)
	current := node.Conditions.NodeAgentReady
	statusChanged := previous.Status != current.Status
	causeChanged := previousCause != probe.cause

	attrs := []any{
		"nodeID", node.ID,
		"nodeName", node.Name,
		"clusterID", node.ClusterID,
		"nodeIP", node.IP,
		"profile", agent.TransportProfile(node),
		"previousStatus", previous.Status,
		"status", current.Status,
		"previousCause", previousCause,
		"cause", probe.cause,
	}
	if !previous.LastTransitionTime.IsZero() && statusChanged {
		attrs = append(attrs, "previousStatusDuration", time.Since(previous.LastTransitionTime).Round(time.Second))
	}
	if probe.err != nil {
		attrs = append(attrs, "error", probe.err)
	}
	attrs = append(attrs,
		"kubernetesReady", node.Conditions.KubernetesReady.Status,
		"rebooting", node.Conditions.Rebooting.Status,
		"provisioned", node.Conditions.Provisioned.Status,
	)

	switch {
	case statusChanged || causeChanged:
		message := "Node agent readiness changed"
		if probe.cause == agent.ProbeCauseTransportSetup {
			message = "Node agent readiness changed: the controller could not build the transport credentials, " +
				"so every node with this profile fails the same way"
		}
		c.logger.Info(message, attrs...)
	case previous.Message != current.Message:
		c.logger.Debug("Node agent readiness message changed", attrs...)
	}
}
