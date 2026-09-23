// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
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
// to its current db.Node and dials its agent over SPIFFE x509 mTLS, mirroring
// internal/agent.NodeAgentClient. Nodes with SpiffeEnabled=false fall back to an
// insecure connection (tier-0 self-bootstrap). resolve is required; trustDomain
// is required only for SPIFFE-enabled nodes. The returned client is the raw
// agent gRPC client; callers invoke its RPCs directly.
func NewSPIFFEDialer(cfg config.Config, trustDomain string, resolve NodeResolver) (nodeagent.Dialer, error) {
	if resolve == nil {
		return nil, errors.New("rollout cwe NewSPIFFEDialer: resolve must be non-nil")
	}
	return func(ctx context.Context, rolloutNode rollout.Node) (nkeproto.NodeAgentClient, func(), error) {
		if rolloutNode == nil {
			return nil, nil, errors.New("rollout cwe dialer: node must be non-nil")
		}
		node, err := resolve(ctx, rolloutNode.ID())
		if err != nil {
			return nil, nil, fmt.Errorf("resolve node %s: %w", rolloutNode.ID(), err)
		}
		conn, cleanup, err := dialNodeAgent(ctx, cfg, trustDomain, node)
		if err != nil {
			return nil, nil, fmt.Errorf("dial agent on node %s: %w", rolloutNode.Name(), err)
		}
		return nkeproto.NewNodeAgentClient(conn), cleanup, nil
	}, nil
}

// NewMethodSupportChecker returns a nodeagent.MethodSupportChecker that dials a
// agent (over the same transport a rollout will use) and verifies method
// support via gRPC server reflection. It translates an unsupported-method
// reflection result into nodeagent.ErrMethodUnsupported so rollout consumers
// branch on capability without importing the transport layer. resolve is
// required; trustDomain is required only for SPIFFE-enabled nodes.
func NewMethodSupportChecker(cfg config.Config, trustDomain string, resolve NodeResolver) (nodeagent.MethodSupportChecker, error) {
	if resolve == nil {
		return nil, errors.New("rollout cwe NewMethodSupportChecker: resolve must be non-nil")
	}
	return func(ctx context.Context, rolloutNode rollout.Node, fullMethod string) error {
		if rolloutNode == nil {
			return errors.New("rollout cwe method-support checker: node must be non-nil")
		}
		node, err := resolve(ctx, rolloutNode.ID())
		if err != nil {
			return fmt.Errorf("resolve node %s: %w", rolloutNode.ID(), err)
		}
		conn, cleanup, err := dialNodeAgent(ctx, cfg, trustDomain, node)
		if err != nil {
			return fmt.Errorf("dial agent on node %s: %w", rolloutNode.Name(), err)
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

// dialNodeAgent replicates internal/agent.NodeAgentClient.getGRPCConnection:
// an insecure dial for non-SPIFFE nodes, otherwise SPIFFE x509 mTLS with the
// node's SPIFFE ID authorized. It returns the raw connection plus a cleanup the
// caller must invoke.
func dialNodeAgent(ctx context.Context, cfg config.Config, trustDomain string, node db.Node) (*grpc.ClientConn, func(), error) {
	endpoint := net.JoinHostPort(node.IP, cfg.NodeAgentPort)

	var closeFuncs []func() error
	cleanup := func() {
		for _, f := range closeFuncs {
			_ = f()
		}
	}

	// If SPIFFE is not enabled on the node, assume an insecure connection.
	if !node.SpiffeEnabled {
		conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to connect to node agent via insecure connection: %w", err)
		}
		closeFuncs = append(closeFuncs, conn.Close)
		return conn, cleanup, nil
	}

	// SPIFFE is enabled, using SPIFFE x509 mTLS.
	if trustDomain == "" {
		return nil, nil, fmt.Errorf("trust domain is required for SPIFFE authentication")
	}

	workloadClient, err := workloadapi.New(ctx, workloadapi.WithAddr(cfg.SpiffeEndpointSocket))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create SPIFFE workload API client: %w", err)
	}
	closeFuncs = append(closeFuncs, workloadClient.Close)

	x509Source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClient(workloadClient))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to create SPIFFE X509 source: %w", err)
	}
	closeFuncs = append(closeFuncs, x509Source.Close)

	bundleSource, err := workloadapi.NewBundleSource(ctx, workloadapi.WithClient(workloadClient))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to create SPIFFE bundle source: %w", err)
	}
	closeFuncs = append(closeFuncs, bundleSource.Close)

	expectedID, err := spiffeid.FromString(node.GetSpiffeID(trustDomain))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to parse node SPIFFE ID: %w", err)
	}

	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(
			grpccredentials.MTLSClientCredentials(
				x509Source,
				bundleSource,
				tlsconfig.AuthorizeID(expectedID),
			),
		),
	)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to connect to node agent via SPIFFE: %w", err)
	}
	closeFuncs = append(closeFuncs, conn.Close)

	return conn, cleanup, nil
}
