// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	"errors"
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dsx-ai-factory/nke/controller/api"
)

// Tenants is the part of the controller client a sweep needs. *nke.Client satisfies
// it.
type Tenants interface {
	ListClusters(ctx context.Context) ([]api.Cluster, error)
	DeleteCluster(ctx context.Context, id openapi_types.UUID) error
	WaitForClusterDeleted(ctx context.Context, id openapi_types.UUID) error
}

// SweepTenants deletes every tenant cluster the site's controller knows about and
// waits for each to go. It runs before the site is released: a tenant's
// machines are Forge instances the site holds, and releasing the site first
// leaves them allocated with nothing left to delete them.
//
// Every deletion is requested before any is waited on, so tenants drain in
// parallel, and one tenant failing does not stop the others.
func SweepTenants(ctx context.Context, c Tenants) error {
	clusters, err := c.ListClusters(ctx)
	if err != nil {
		return fmt.Errorf("sweep tenants: %w", err)
	}
	var (
		errs    []error
		pending []api.Cluster
	)
	for _, cl := range clusters {
		if err := c.DeleteCluster(ctx, cl.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete tenant %s (%s): %w", cl.Name, cl.ID, err))
			continue
		}
		pending = append(pending, cl)
	}
	for _, cl := range pending {
		if err := c.WaitForClusterDeleted(ctx, cl.ID); err != nil {
			errs = append(errs, fmt.Errorf("wait for tenant %s (%s): %w", cl.Name, cl.ID, err))
		}
	}
	return errors.Join(errs...)
}
