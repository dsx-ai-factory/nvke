// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"regexp"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

var localAddressPattern = regexp.MustCompile(`(read|write) tcp (\[[0-9a-fA-F:]+\]|[0-9]{1,3}([.][0-9]{1,3}){3}):[0-9]+->`)

type nodeAgentProbe struct {
	agentVersion string
	err          error
}

func applyNodeAgentReadiness(node *db.Node, agentProbe nodeAgentProbe) bool {
	previousAgentReady := node.Conditions.NodeAgentReady
	previousAgentVersion := node.AgentVersion

	node.Conditions.NodeAgentReady = nodeAgentCondition(node.Conditions.NodeAgentReady, agentProbe)
	if agentProbe.agentVersion != "" {
		node.AgentVersion = agentProbe.agentVersion
	}

	return node.Conditions.NodeAgentReady != previousAgentReady || node.AgentVersion != previousAgentVersion
}

func nodeAgentCondition(existing db.Condition, agentProbe nodeAgentProbe) db.Condition {
	if agentProbe.err != nil {
		return updatedCondition(existing, db.ConditionStatusFalse, normalizeProbeError(agentProbe.err.Error()))
	}

	return updatedCondition(existing, db.ConditionStatusTrue, "Node agent is reachable")
}

func normalizeProbeError(message string) string {
	return localAddressPattern.ReplaceAllString(message, "$1 tcp ->")
}

func updatedCondition(existing db.Condition, status db.ConditionStatus, message string) db.Condition {
	db.SetCondition(&existing, db.Condition{Status: status, Message: message})
	return existing
}
