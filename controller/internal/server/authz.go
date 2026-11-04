// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"log/slog"

	"github.com/dsx-ai-factory/nvke/auth/authz"
)

// errListTenant is the error a list answers when it can't read the authorized
// caller's tenant. The cause goes to the log, not the response body.
var errListTenant = errors.New("failed to authorize the list")

// listTenant returns the tenant a list is filtered to, or nil for every
// tenant. With authorization off it is nil. With it on, it follows
// authz.ListTenant, which fails closed on a context with no authorized caller.
func listTenant(ctx context.Context, authzEnabled bool, logger *slog.Logger) (*string, error) {
	if !authzEnabled {
		return nil, nil //nolint:nilnil // nil is "no filter"
	}
	tenant, all, err := authz.ListTenant(ctx)
	if err != nil {
		logger.Error("Failed to read the list tenant", "error", err)
		return nil, errListTenant
	}
	if all {
		return nil, nil //nolint:nilnil // nil is "no filter"
	}
	return &tenant, nil
}
