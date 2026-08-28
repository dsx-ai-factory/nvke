// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package agentupgrade contains the durable contract shared by nke-agent and
// the out-of-process node-agent updater.
package agentupgrade

import "time"

// Method selects how the updater obtains and installs the target artifact.
type Method string

const (
	// MethodAPT installs an exact nke-agent Debian package version.
	MethodAPT Method = "apt"
	// MethodBundle installs an exact agent bundle resolved by nke-agent.
	MethodBundle Method = "bundle"
)

// Action distinguishes an upgrade from a restore of a prior operation.
type Action string

const (
	// ActionUpgrade applies the requested target artifact.
	ActionUpgrade Action = "upgrade"
	// ActionRollback restores the artifact retained by a prior upgrade.
	ActionRollback Action = "rollback"
)

// Phase is the durable lifecycle state of an updater operation.
type Phase string

const (
	// PhasePending means the request is persisted but the updater has not started.
	PhasePending Phase = "Pending"
	// PhaseRunning means the independent updater is executing the request.
	PhaseRunning Phase = "Running"
	// PhaseSucceeded means the requested version was installed and verified.
	PhaseSucceeded Phase = "Succeeded"
	// PhaseFailedRestored means the request failed and the previous version is running.
	PhaseFailedRestored Phase = "FailedRestored"
	// PhaseFailedNotRestored means neither the request nor restoration completed safely.
	PhaseFailedNotRestored Phase = "FailedNotRestored"
)

// Operation is persisted below /var/lib/nke/agent-upgrades and is intentionally
// independent of the nke-agent state file so it survives service replacement.
type Operation struct {
	ID                     string    `json:"id"`
	Action                 Action    `json:"action"`
	TargetVersion          string    `json:"targetVersion"`
	PreviousVersion        string    `json:"previousVersion"`
	PreviousPackageVersion string    `json:"previousPackageVersion,omitempty"`
	Method                 Method    `json:"method"`
	ArtifactURL            string    `json:"artifactUrl,omitempty"`
	RestoreFromOperationID string    `json:"restoreFromOperationId,omitempty"`
	Phase                  Phase     `json:"phase"`
	Step                   string    `json:"step"`
	ErrorMessage           string    `json:"errorMessage,omitempty"`
	CreatedAt              time.Time `json:"createdAt"`
	UpdatedAt              time.Time `json:"updatedAt"`
}

// Terminal reports whether no further updater work is expected for the operation.
func (o Operation) Terminal() bool {
	switch o.Phase {
	case PhaseSucceeded, PhaseFailedRestored, PhaseFailedNotRestored:
		return true
	case PhasePending, PhaseRunning:
		return false
	default:
		return false
	}
}
