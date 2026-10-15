// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package agentupgrade contains the durable contract shared by nvke-agent and
// the out-of-process node-agent updater.
package agentupgrade

import "time"

// DefaultRoot is the durable directory shared by the agent and its updater.
const DefaultRoot = "/var/lib/nvke/agent-upgrades"

// Method selects how the updater obtains and installs the target artifact.
type Method string

const (
	// MethodAPT installs an exact nvke-agent Debian package version.
	MethodAPT Method = "apt"
	// MethodBundle installs an exact agent bundle resolved by nvke-agent.
	MethodBundle Method = "bundle"
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

// Operation is persisted below /var/lib/nvke/agent-upgrades and is intentionally
// independent of the nvke-agent state file so it survives service replacement.
type Operation struct {
	TargetVersion          string    `json:"targetVersion"`
	PreviousVersion        string    `json:"previousVersion"`
	PreviousPackageName    string    `json:"previousPackageName,omitempty"`
	PreviousPackageVersion string    `json:"previousPackageVersion,omitempty"`
	Method                 Method    `json:"method"`
	ArtifactURL            string    `json:"artifactUrl,omitempty"`
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

// Request identifies an installation by its exact target version and method.
type Request struct {
	TargetVersion string `json:"targetVersion"`
	Method        Method `json:"method"`
}

// Key returns the filesystem and systemd name derived from the request pair.
func (r Request) Key() string { return r.TargetVersion + "_" + string(r.Method) }

// Request returns the installation pair for this operation.
func (o Operation) Request() Request {
	return Request{TargetVersion: o.TargetVersion, Method: o.Method}
}

// Key returns the filesystem and systemd name derived from the request pair.
func (o Operation) Key() string { return o.Request().Key() }
