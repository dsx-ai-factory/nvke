// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package infra is the contract for creating the bare machine a site
// controller is built on, before any CWE exists to ask. It names capabilities
// rather than infrastructure, so nothing here is provider vocabulary and it
// carries no external dependencies.
package infra

import (
	"context"
)

// NodeRequest is what a run asks for.
type NodeRequest struct {
	// Name identifies the run's machine to the provider and to anyone reading
	// the provider's console. It becomes the node's hostname and its
	// Kubernetes node name.
	Name string

	// Capabilities select a machine by what it is. Keys a provider cannot
	// answer, such as a Kubernetes version, are ignored rather than matched.
	Capabilities map[string]string

	// NodeVersion is the Node component under qualification, which decides
	// the OS image written to the machine.
	NodeVersion string

	// Labels are attached to the machine for provenance: run, commit,
	// candidate.
	Labels map[string]string
}

// Node is what bootstrap needs to know about a machine. It is the triple
// `nkectl generate sitespec --nodes` consumes, so the provider produces it
// rather than leaving the caller to rediscover it over the session.
type Node struct {
	Name string
	IP   string

	// TPMEkThumbprint is optional, and gates TPM attestation: an empty one
	// deploys no TPM attestor for this node.
	TPMEkThumbprint string
}

// Session is a command, copy and forward channel to the node. It is what
// bootstrap runs nkectl through, how the test reaches the cluster the node
// ends up hosting, and what the caller tears down when finished.
type Session interface {
	// Run executes cmd on the node and fails on a non-zero exit, returning
	// the command's combined output either way so a failure can be reported
	// with what the node said.
	Run(ctx context.Context, cmd string) (string, error)

	// Put writes data to path on the node with the given mode, as root:
	// what bootstrap stages goes into system paths.
	Put(ctx context.Context, path string, data []byte, mode uint32) error

	// Fetch reads path from the node as root, since the kubeconfig bootstrap
	// writes is readable by nobody else.
	Fetch(ctx context.Context, path string) ([]byte, error)

	// Forward forwards remotePort on the node to a local listener and returns
	// its address. The forward lives until Teardown.
	Forward(ctx context.Context, remotePort int) (string, error)

	// Teardown closes every forward, drops the connection, and releases the
	// machine. It must be safe to call more than once and on a session whose
	// machine never became reachable.
	Teardown(ctx context.Context) error
}

// Provider creates the bare machine a site controller is built on. Forge is
// the first implementation. A second provider is a second implementation plus
// an environment file in its own vocabulary, with nothing above this
// interface changing.
type Provider interface {
	ProvisionNode(ctx context.Context, req NodeRequest) (Node, Session, error)
}
