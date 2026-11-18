// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"regexp"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// A new connection can pick a different local port for the same probe failure.
// Match only the local side so the condition message retains the remote endpoint.
var probeTCPLocalAddress = regexp.MustCompile(`\b(read|write) tcp (?:[0-9]+(?:\.[0-9]+){3}|\[[^\s\[\]]+\]):[0-9]+->`)

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
		message := probeTCPLocalAddress.ReplaceAllString(agentProbe.err.Error(), "$1 tcp ->")
		return updatedCondition(existing, db.ConditionStatusFalse, message)
	}

	return updatedCondition(existing, db.ConditionStatusTrue, "Node agent is reachable")
}

func updatedCondition(existing db.Condition, status db.ConditionStatus, message string) db.Condition {
	db.SetCondition(&existing, db.Condition{Status: status, Message: message})
	return existing
}
