// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/agent"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/nodeagent"
)

// ErrNodeNotFound reports that a rollout node is absent from CWE inventory.
var ErrNodeNotFound = errors.New("node not found")

// NodeResolver loads the current db.Node for a rollout node UUID. The dialer
// needs the database record to build its SPIFFE ID and read SpiffeEnabled.
type NodeResolver func(ctx context.Context, nodeID uuid.UUID) (db.Node, error)

// NewNodeResolver builds a resolver over a fixed node set for tests or other
// snapshot-scoped callers.
func NewNodeResolver(nodes []db.Node) NodeResolver {
	byID := make(map[uuid.UUID]db.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	return func(_ context.Context, nodeID uuid.UUID) (db.Node, error) {
		n, ok := byID[nodeID]
		if !ok {
			return db.Node{}, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
		}
		return n, nil
	}
}

// NewDatabaseNodeResolver builds a resolver over current CWE inventory. Rollout
// workers are long-lived and must handle clusters and nodes created after worker
// startup, so a fixed startup snapshot is not suitable for production wiring.
func NewDatabaseNodeResolver(database db.DB) (NodeResolver, error) {
	if database == nil || isNilDatabase(database) {
		return nil, errors.New("rollout cwe NewDatabaseNodeResolver: database must be non-nil")
	}
	return func(ctx context.Context, nodeID uuid.UUID) (db.Node, error) {
		node, err := database.GetNode(ctx, nodeID)
		if err != nil {
			return db.Node{}, fmt.Errorf("resolve rollout node %s: %w", nodeID, err)
		}
		return node, nil
	}, nil
}

func isNilDatabase(database db.DB) bool {
	value := reflect.ValueOf(database)
	kind := value.Kind()
	nilable := kind == reflect.Chan ||
		kind == reflect.Func ||
		kind == reflect.Interface ||
		kind == reflect.Map ||
		kind == reflect.Pointer ||
		kind == reflect.Slice
	return nilable && value.IsNil()
}

// NewSPIFFEDialer returns a nodeagent.Dialer that resolves the rollout node ID
// to its current db.Node and dials its agent over the transport the rest of the
// controller uses, so a rollout reaches a node exactly the way every other
// caller does. resolve and transport are both required. The returned client is
// the raw agent gRPC client; callers invoke its RPCs directly.
func NewSPIFFEDialer(transport *agent.Transport, resolve NodeResolver) (nodeagent.Dialer, error) {
	if transport == nil {
		return nil, errors.New("rollout cwe NewSPIFFEDialer: transport must be non-nil")
	}
	if resolve == nil {
		return nil, errors.New("rollout cwe NewSPIFFEDialer: resolve must be non-nil")
	}
	return func(ctx context.Context, rolloutNode rollout.Node) (nkeproto.NodeAgentClient, func(), error) {
		conn, cleanup, err := dial(ctx, transport, resolve, rolloutNode)
		if err != nil {
			return nil, nil, err
		}
		return nkeproto.NewNodeAgentClient(conn), cleanup, nil
	}, nil
}

// NewMethodSupportChecker returns a nodeagent.MethodSupportChecker that dials an
// agent over the same transport a rollout will use and verifies method support
// via gRPC server reflection. It translates an unsupported-method reflection
// result into nodeagent.ErrMethodUnsupported so rollout consumers branch on
// capability without importing the transport layer. resolve and transport are
// both required.
func NewMethodSupportChecker(transport *agent.Transport, resolve NodeResolver) (nodeagent.MethodSupportChecker, error) {
	if transport == nil {
		return nil, errors.New("rollout cwe NewMethodSupportChecker: transport must be non-nil")
	}
	if resolve == nil {
		return nil, errors.New("rollout cwe NewMethodSupportChecker: resolve must be non-nil")
	}
	return func(ctx context.Context, rolloutNode rollout.Node, fullMethod string) error {
		conn, cleanup, err := dial(ctx, transport, resolve, rolloutNode)
		if err != nil {
			return err
		}
		defer cleanup()

		if err := ensureGRPCMethodSupported(ctx, conn, fullMethod); err != nil {
			if errors.Is(err, errGRPCMethodUnsupported) {
				return fmt.Errorf("%w: %w", nodeagent.ErrMethodUnsupported, err)
			}
			return err
		}
		return nil
	}, nil
}

func dial(
	ctx context.Context,
	transport *agent.Transport,
	resolve NodeResolver,
	rolloutNode rollout.Node,
) (*grpc.ClientConn, func(), error) {
	if rolloutNode == nil {
		return nil, nil, errors.New("rollout cwe dialer: node must be non-nil")
	}
	node, err := resolve(ctx, rolloutNode.ID())
	if err != nil {
		return nil, nil, fmt.Errorf("resolve node %s: %w", rolloutNode.ID(), err)
	}
	conn, cleanup, err := transport.Dial(ctx, node)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent on node %s: %w", rolloutNode.Name(), err)
	}
	return conn, cleanup, nil
}
