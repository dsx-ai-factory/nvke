// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiauthz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/auth/authz"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// The resource types Middleware asks about.
const (
	typeCluster    = "Cluster"
	typeNodePool   = "NodePool"
	typeNodeSet    = "NodeSet"
	typeNode       = "Node"
	typeCollection = "Collection"
)

//nolint:gochecknoglobals // fixed for the life of the process
var resourceTypes = []string{typeCluster, typeNodePool, typeNodeSet, typeNode, typeCollection}

// Middleware authorizes each API request with an auth/authz Client. It is
// safe for concurrent use.
type Middleware struct {
	client  *authz.Client
	tenants db.TenantQueries
	record  authz.Recorder
	logger  *slog.Logger
}

// NewMiddleware returns a Middleware that asks client, and looks up each
// object's owner in tenants. record counts a request Middleware answers with
// 500 before asking client; pass the Recorder client counts with.
func NewMiddleware(client *authz.Client, tenants db.TenantQueries, record authz.Recorder, logger *slog.Logger) *Middleware {
	if record == nil {
		record = func(authz.Outcome, string) {}
	}
	return &Middleware{client: client, tenants: tenants, record: record, logger: logger}
}

// Strict is an api.StrictMiddlewareFunc. The generated handler calls it after
// request validation, so a malformed request gets 400 first. The action is
// operationID with its first letter lowered, which is the spec's operationId.
// On an allow it calls next with the context Check returns. Otherwise it
// writes the refusal or error and returns (nil, nil), and the generated
// handler writes nothing more.
func (m *Middleware) Strict(next api.StrictHandlerFunc, operationID string) api.StrictHandlerFunc {
	action := strings.ToLower(operationID[:1]) + operationID[1:]
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		target, err := m.target(ctx, action, request)
		if err != nil {
			m.record(authz.OutcomeError, action)
			authz.WriteError(w, r, err, m.logger)
			return nil, nil
		}
		ctx, err = m.client.Check(ctx, target)
		if err != nil {
			authz.WriteError(w, r, err, m.logger)
			return nil, nil
		}
		return next(ctx, w, r, request)
	}
}

// target maps request to the resource action acts on. An error is a failed
// lookup or an unmapped request, never a refusal.
func (m *Middleware) target(ctx context.Context, action string, request any) (authz.Target, error) {
	switch r := request.(type) {
	case api.ListClustersRequestObject:
		return collection(action, "clusters", authz.ScopeList), nil
	case api.CreateClusterRequestObject:
		return collection(action, "clusters", authz.ScopeCreate), nil
	case api.GetClusterRequestObject:
		return m.object(ctx, action, typeCluster, r.ClusterID)
	case api.UpdateClusterRequestObject:
		return m.object(ctx, action, typeCluster, r.ClusterID)
	case api.DeleteClusterRequestObject:
		return m.object(ctx, action, typeCluster, r.ClusterID)
	case api.GetClusterControlPlaneRequestObject:
		return m.object(ctx, action, typeCluster, r.ClusterID)
	case api.GetClusterControlPlaneConfigHistoryRequestObject:
		return m.object(ctx, action, typeCluster, r.ClusterID)
	case api.GetClusterSpireServerRequestObject:
		return m.object(ctx, action, typeCluster, r.ClusterID)
	case api.UpdateClusterSpireServerRequestObject:
		return m.object(ctx, action, typeCluster, r.ClusterID)

	case api.ListNodePoolsRequestObject:
		return collection(action, "nodePools", authz.ScopeList), nil
	case api.CreateNodePoolRequestObject:
		// A new node pool is an action on its parent cluster. The request
		// validator has already refused a body with no clusterID.
		var clusterID uuid.UUID
		if r.Body != nil {
			clusterID = r.Body.ClusterID
		}
		return m.object(ctx, action, typeCluster, clusterID)
	case api.GetNodePoolRequestObject:
		return m.object(ctx, action, typeNodePool, r.NodePoolID)
	case api.UpdateNodePoolRequestObject:
		return m.object(ctx, action, typeNodePool, r.NodePoolID)
	case api.DeleteNodePoolRequestObject:
		return m.object(ctx, action, typeNodePool, r.NodePoolID)

	case api.ListNodeSetsRequestObject:
		return collection(action, "nodeSets", authz.ScopeList), nil
	case api.GetNodeSetRequestObject:
		return m.object(ctx, action, typeNodeSet, r.NodeSetID)

	case api.ListNodesRequestObject:
		return collection(action, "nodes", authz.ScopeList), nil
	case api.GetNodeRequestObject:
		return m.object(ctx, action, typeNode, r.NodeID)
	case api.DeleteNodeRequestObject:
		return m.object(ctx, action, typeNode, r.NodeID)

	case api.ListReleasesRequestObject:
		return collection(action, "releases", authz.ScopeGlobal), nil
	case api.ListConfigProfilesRequestObject:
		return collection(action, "configProfiles", authz.ScopeGlobal), nil
	}
	return authz.Target{}, fmt.Errorf("no authorization target for a request of type %T", request)
}

// collection returns the Target of a list, a create or a global read.
func collection(action, id string, scope authz.Scope) authz.Target {
	return authz.Target{
		Action:   action,
		Resource: authz.Resource{Type: typeCollection, ID: id, Scope: scope},
	}
}

// object returns the Target of an action on one object, with the tenant that
// owns it. An empty tenant means the object has no owner.
func (m *Middleware) object(ctx context.Context, action, typ string, id uuid.UUID) (authz.Target, error) {
	var lookup func(context.Context, uuid.UUID) (string, error)
	switch typ {
	case typeCluster:
		lookup = m.tenants.GetClusterTenant
	case typeNodePool:
		lookup = m.tenants.GetNodePoolTenant
	case typeNodeSet:
		lookup = m.tenants.GetNodeSetTenant
	case typeNode:
		lookup = m.tenants.GetNodeTenant
	default:
		return authz.Target{}, fmt.Errorf("no tenant lookup for resource type %s", typ)
	}
	tenant, err := lookup(ctx, id)
	missing := errors.Is(err, db.ErrNotFound)
	if err != nil && !missing {
		return authz.Target{}, fmt.Errorf("look up the tenant of %s %s: %w", typ, id, err)
	}
	return authz.Target{
		Action:   action,
		Resource: authz.Resource{Type: typ, ID: id.String(), Scope: authz.ScopeObject, Tenant: tenant},
		Missing:  missing,
	}, nil
}
