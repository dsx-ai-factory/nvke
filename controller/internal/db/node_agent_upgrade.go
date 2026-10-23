// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"time"

	"github.com/google/uuid"
)

const (
	// NodeAgentUpgradeSourceLifecycle identifies upgrades required before preflight.
	NodeAgentUpgradeSourceLifecycle = "lifecycle"
)

// NodeAgentUpgrade records the selected node and accepted target for a cluster.
type NodeAgentUpgrade struct {
	// Source selects the provisioning workflow or background controller that resumes this request.
	Source string `json:"source"`
	// NodeID keeps the same node selected after a controller restart.
	NodeID uuid.UUID `json:"nodeId"`
	// TargetVersion preserves the accepted version until the current installation finishes.
	TargetVersion string `json:"targetVersion"`
	// Method preserves the accepted installation method across controller configuration changes.
	Method string `json:"method"`
	// Phase is Pending before the first request and Running during installation or retries.
	Phase string `json:"phase"`
	// ErrorMessage exposes the last failure even when the agent is unreachable.
	ErrorMessage string `json:"errorMessage"`
	// RetryAt keeps the retry delay across controller restarts.
	RetryAt time.Time `json:"retryAt"`
}

// NodeAgentLifecycle keeps the provisioning target and method through join.
type NodeAgentLifecycle struct {
	// Version pins the provisioning target across activity and workflow retries.
	Version string `json:"version"`
	// Method pins the installation method if controller configuration changes during provisioning.
	Method string `json:"method"`
}

// NodeAgentCertificateRenewal identifies actual renewal work, not a queued workflow.
// Its identity lets activity retries resume accepted work after a connection failure.
type NodeAgentCertificateRenewal struct {
	NodeID      uuid.UUID `json:"nodeId"`
	OperationID string    `json:"operationId"`
	// Fenced marks a V2 reservation, owned by a cycle under the cluster mutation
	// lock, so the reconciler may clear it once the agent has no such operation.
	Fenced bool `json:"fenced,omitempty"`
}
