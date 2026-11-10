// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// nodeHasNoAddressMessage is the condition message for a node that has no IP
// address yet. Nothing was dialed, so it is not a probe failure.
const nodeHasNoAddressMessage = "Node has no IP address"

type nodeAgentProbe struct {
	agentVersion string
	err          error
	// message is the condition message. It is empty for a probe outcome, whose
	// message is the error text.
	message string
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
	if agentProbe.message != "" {
		return updatedCondition(existing, db.ConditionStatusFalse, agentProbe.message)
	}
	if agentProbe.err != nil {
		return updatedCondition(existing, db.ConditionStatusFalse, agentProbe.err.Error())
	}

	return updatedCondition(existing, db.ConditionStatusTrue, "Node agent is reachable")
}

func updatedCondition(existing db.Condition, status db.ConditionStatus, message string) db.Condition {
	db.SetCondition(&existing, db.Condition{Status: status, Message: message})
	return existing
}
