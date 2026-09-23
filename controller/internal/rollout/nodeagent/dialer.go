// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodeagent is the rollout subsystem's agent transport
// contract: how to reach an agent (Dialer) and the kubeadm status
// values it reports. It depends only on the agent proto, so both the
// concrete preflights/operations and the CWE adapter can share it without
// coupling to the CWE data model.
package nodeagent

import (
	"context"
	"errors"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
)

// Dialer opens an agent gRPC client for node and returns a cleanup the
// caller must invoke. The node identity is part of the contract because CWE's
// SPIFFE authorization uses the node UUID; an IP address alone is not unique
// when clusters use overlapping private networks.
type Dialer func(ctx context.Context, node rollout.Node) (client nkeproto.NodeAgentClient, cleanup func(), err error)

// ErrMethodUnsupported reports that an agent cannot prove it implements a
// requested RPC — it is too old, or does not serve gRPC reflection.
// MethodSupportChecker implementations wrap it so callers can branch with
// IsMethodUnsupported (fail a validation, or skip an optional step) without
// coupling to the transport layer.
var ErrMethodUnsupported = errors.New("agent method unsupported")

// MethodSupportChecker reports whether node's agent implements
// fullMethod — a generated proto _FullMethodName such as
// nkeproto.NodeAgent_EnableAPIServerAuthenticationConfig_FullMethodName —
// typically via gRPC reflection. It returns nil when supported, an
// ErrMethodUnsupported-wrapped error when support cannot be proven, or a plain
// error on a transport failure. It is the reusable capability primitive a
// rollout operation's Validate gates on, in place of an agent version floor.
type MethodSupportChecker func(ctx context.Context, node rollout.Node, fullMethod string) error

// IsMethodUnsupported reports whether err indicates the agent does not
// implement the requested method (as opposed to a transport failure).
func IsMethodUnsupported(err error) bool { return errors.Is(err, ErrMethodUnsupported) }

// Kubeadm status values reported by the agent's GetKubeadmStatus.
// Anything other than KubeadmStatusReady means the agent is busy or broken.
const (
	KubeadmStatusUnknown = "unknown"
	KubeadmStatusPending = "pending"
	KubeadmStatusReady   = "ready"
	KubeadmStatusError   = "error"
)
