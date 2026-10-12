// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package infra is the contract for creating the bare machine a site
// controller is built on, before any controller exists to ask. It names capabilities
// rather than infrastructure, so nothing here is provider vocabulary and it
// carries no external dependencies.
package infra

import (
	"context"
)

// The capability keys a machine can answer, and the architecture a run that
// named none gets.
//
// Here rather than with a provider because they are the vocabulary a run and
// a provider share: a request states them and every provider has to answer
// the same words. A second provider reads them from the contract rather than
// from the first implementation.
//
// A run states more than these -- kube names a software version, not a
// property of any machine -- so a provider matches on the keys it knows and
// ignores the rest, which is what stops a Kubernetes version from excluding
// every machine.
const (
	CapabilityMachineType = "machineType"
	CapabilityArch        = "arch"
	CapabilityGPU         = "gpu"

	// The two values CapabilityMachineType takes. A machine with a GPU
	// provides both an architecture and a model, so matching on architecture
	// alone can land on one, and the caller that wanted a plain machine has
	// to say so.
	MachineTypeCPU = "cpu"
	MachineTypeGPU = "gpu"

	// DefaultArch is what a run that named no architecture gets. Every site
	// onboarded so far has an amd64 shape, so this keeps a run without
	// -e2e.capabilities resolving as it did before architectures were told
	// apart.
	DefaultArch = "amd64"
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
// `nvkectl generate sitespec --nodes` consumes, so the provider produces it
// rather than leaving the caller to rediscover it over the session.
type Node struct {
	Name string
	IP   string

	// Arch is the machine's architecture, as the provider's own catalog
	// names it. The provider knows it when it picks a machine to satisfy the
	// request, and a caller that had to ask the provider's types for it would
	// be holding a provider-specific value to learn a property of the node.
	Arch string

	// TPMEkThumbprint is optional, and gates TPM attestation: an empty one
	// deploys no TPM attestor for this node.
	TPMEkThumbprint string
}

// Session is a command, copy and forward channel to the node. It is what
// bootstrap runs nvkectl through, how the test reaches the cluster the node
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

	// Forward forwards remote, a host:port the node can reach, to a local
	// listener and returns its address. The forward lives until Teardown.
	//
	// The host is named rather than assumed to be the node's loopback: a
	// tenant cluster's API server has its own VIP, reachable from the node
	// and not from here.
	Forward(ctx context.Context, remote string) (string, error)

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
