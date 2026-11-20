// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeagentreadiness

import (
	"errors"
	"regexp"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// noAddressMessage is the nodeAgentReady message for a node that has no IP
	// address to dial.
	noAddressMessage = "Node has no IP address"
)

// tcpOpLocalAddr matches the local address net.OpError reports for operations
// on an established connection, as "read tcp <local>-><remote>" or
// "write tcp6 <local>-><remote>". The local port is ephemeral and differs on
// every probe, so keeping it in the condition message would make each probe
// look like a new failure: the write it triggers requeues the node and the
// agent is probed again about a second later. Only the remote side is kept.
var tcpOpLocalAddr = regexp.MustCompile(`\b(read|write|dial) (tcp[46]?) \S+->`)

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
		if errors.Is(agentProbe.err, agent.ErrNoAddress) {
			return updatedCondition(existing, db.ConditionStatusFalse, noAddressMessage)
		}
		return updatedCondition(existing, db.ConditionStatusFalse, normalizeProbeErrorMessage(agentProbe.err.Error()))
	}

	return updatedCondition(existing, db.ConditionStatusTrue, "Node agent is reachable")
}

func updatedCondition(existing db.Condition, status db.ConditionStatus, message string) db.Condition {
	db.SetCondition(&existing, db.Condition{Status: status, Message: message})
	return existing
}

// normalizeProbeErrorMessage drops the per-connection local address from a
// probe error so that consecutive failures with the same cause produce the
// same condition message.
func normalizeProbeErrorMessage(message string) string {
	return tcpOpLocalAddr.ReplaceAllString(message, "$1 $2 ->")
}
